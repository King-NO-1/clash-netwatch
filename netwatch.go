// Clash NetWatch —— 按当前 WiFi 自动开关 Clash Verge 的系统代理与 TUN。
//
// 设计：Windows 事件驱动（NetworkProfile 事件 + IP 地址表变更），单 worker 串行执行，
// 目标是「事件到达 → 状态生效」控制在数十毫秒；适配器真正可用由内核决定。
//
// 构建：go build -trimpath -ldflags "-s -w -H=windowsgui" -o netwatch.exe .
// 文件：netwatch.go（守护主体）、tray.go（托盘/通知）、autoconnect.go（自动连热点）
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

const (
	appName   = "ClashNetWatch"
	mutexName = `Local\ClashNetWatch`

	wlanOpcodeCurrentConnection = 7
	evtSubscribeToFutureEvents  = 1
	evtSubscribeActionDeliver   = 1

	hkeyCurrentUser = 0x80000001
	keyReadWrite    = 0x2001F
	regSZ           = 1
	regDWORD        = 4

	internetOptionRefresh         = 37
	internetOptionSettingsChanged = 39

	keyRead     = 0x20019
	pipeTimeout = 3 * time.Second
	// PATCH /configs 改 tun.enable 时，mihomo 要重建 Wintun 网卡才回响应，
	// 刚切网时实测要 3.4–6.4s，用 3s 超时会误判失败（日志里的 "PATCH 返回 0"），
	// 把开 TUN 推迟到下一轮校准。给写操作单独放宽。
	patchTimeout     = 9 * time.Second
	maxResponseBytes = 8 << 20
)

var (
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	iphlpapi = syscall.NewLazyDLL("iphlpapi.dll")
	wlanapi  = syscall.NewLazyDLL("wlanapi.dll")
	wevtapi  = syscall.NewLazyDLL("wevtapi.dll")
	advapi32 = syscall.NewLazyDLL("advapi32.dll")
	wininet  = syscall.NewLazyDLL("wininet.dll")
	user32   = syscall.NewLazyDLL("user32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")

	pGetModuleHandle = kernel32.NewProc("GetModuleHandleW")

	pCreateMutexW      = kernel32.NewProc("CreateMutexW")
	pCreateEventW      = kernel32.NewProc("CreateEventW")
	pWaitSingle        = kernel32.NewProc("WaitForSingleObject")
	pResetEvent        = kernel32.NewProc("ResetEvent")
	pAttachConsole     = kernel32.NewProc("AttachConsole")
	pNotifyAddrChange  = iphlpapi.NewProc("NotifyAddrChange")
	pWlanOpenHandle    = wlanapi.NewProc("WlanOpenHandle")
	pWlanCloseHandle   = wlanapi.NewProc("WlanCloseHandle")
	pWlanEnumIface     = wlanapi.NewProc("WlanEnumInterfaces")
	pWlanQueryIface    = wlanapi.NewProc("WlanQueryInterface")
	pWlanFreeMemory    = wlanapi.NewProc("WlanFreeMemory")
	pEvtSubscribe      = wevtapi.NewProc("EvtSubscribe")
	pRegCreateKeyExW   = advapi32.NewProc("RegCreateKeyExW")
	pRegOpenKeyExW     = advapi32.NewProc("RegOpenKeyExW")
	pRegSetValueExW    = advapi32.NewProc("RegSetValueExW")
	pRegQueryValueExW  = advapi32.NewProc("RegQueryValueExW")
	pRegCloseKey       = advapi32.NewProc("RegCloseKey")
	pInternetSetOption = wininet.NewProc("InternetSetOptionW")
	pGetConsoleMode    = kernel32.NewProc("GetConsoleMode")
	pWriteConsoleW     = kernel32.NewProc("WriteConsoleW")
	pTerminateProcess  = kernel32.NewProc("TerminateProcess")
	pQueryFullName     = kernel32.NewProc("QueryFullProcessImageNameW")
	pSnapshot          = kernel32.NewProc("CreateToolhelp32Snapshot")
	pProc32First       = kernel32.NewProc("Process32FirstW")
	pProc32Next        = kernel32.NewProc("Process32NextW")

	// 事件到达时刻，用于统计「事件→生效」延迟
	eventAtNano int64
)

// ---------------------------------------------------------------- 配置

type Config struct {
	// TargetSSIDs 是"连上就开代理"的网络。空列表表示尚未配置，守护进程会拒绝启动。
	TargetSSIDs []string `json:"target_ssids"`
	// TargetSSID 是旧版单目标字段，加载时并入 TargetSSIDs，写出时不再产生。
	TargetSSID string `json:"target_ssid,omitempty"`
	// AutoConnectSSIDs 为空表示"所有目标网络都自动连"；非空时只自动连其中列出的。
	AutoConnectSSIDs  []string `json:"auto_connect_ssids"`
	NeverProxySSIDs   []string `json:"never_proxy_ssids"`
	FuzzyMatch        bool     `json:"fuzzy_match"`
	AutoConnect       bool     `json:"auto_connect"`            // 候选里出现目标热点时自动切换过去
	AutoConnectSecs   int      `json:"auto_connect_seconds"`    // 候选轮询间隔
	AutoConnectMinSig int      `json:"auto_connect_min_signal"` // 信号质量阈值（0-100，0 不限）
	ProxyPort         int      `json:"proxy_port"`              // 0 = 读 verge.yaml 的 verge_mixed_port
	SyncVergeConfig   bool     `json:"sync_verge_config"`
	ShowTray          bool     `json:"show_tray"`
	ReconcileSeconds  int      `json:"reconcile_seconds"`
	LogMaxBytes       int64    `json:"log_max_bytes"`
}

var cfg = Config{
	FuzzyMatch:       true,
	AutoConnect:      true,
	AutoConnectSecs:  15,
	SyncVergeConfig:  true,
	ShowTray:         true,
	ReconcileSeconds: 60,
	LogMaxBytes:      512 * 1024,
}

var exeDir string

func configPath() string { return filepath.Join(exeDir, "config.json") }

func configExists() bool {
	_, err := os.Stat(configPath())
	return err == nil
}

// loadConfig 返回 (文件是否存在, 是否解析成功)。不存在不算错误，交给首次运行向导。
func loadConfig() (bool, bool) {
	exeDir, _ = filepath.Abs(filepath.Dir(os.Args[0]))
	raw, err := os.ReadFile(configPath())
	if err != nil {
		return false, false
	}
	// 以当前默认值为基底反序列化：配置文件漏写的字段保持默认，不会被 bool 零值 false 覆盖
	c := cfg
	if json.Unmarshal(raw, &c) != nil {
		return true, false
	}
	if c.TargetSSID != "" {
		c.TargetSSIDs = append(c.TargetSSIDs, c.TargetSSID)
		c.TargetSSID = ""
	}
	c.TargetSSIDs = cleanSSIDList(c.TargetSSIDs)
	c.AutoConnectSSIDs = cleanSSIDList(c.AutoConnectSSIDs)
	c.NeverProxySSIDs = cleanSSIDList(c.NeverProxySSIDs)
	if c.ReconcileSeconds < 10 {
		c.ReconcileSeconds = 60
	}
	if c.AutoConnectSecs < 5 {
		c.AutoConnectSecs = 15 // 兼容旧配置没写字段：回默认值而不是过频轮询
	}
	if c.AutoConnectMinSig < 0 || c.AutoConnectMinSig > 100 {
		c.AutoConnectMinSig = 0
	}
	if c.LogMaxBytes <= 0 {
		c.LogMaxBytes = 512 * 1024
	}
	cfg = c
	return true, true
}

// cleanSSIDList 去掉空白项与归一化后重复的项，保留用户填写的原始大小写。
func cleanSSIDList(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		k := norm(s)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, s)
	}
	return out
}

func targetDisplay() string {
	if len(cfg.TargetSSIDs) == 0 {
		return "未配置"
	}
	return strings.Join(cfg.TargetSSIDs, ", ")
}

// ---------------------------------------------------------------- 日志

type logger struct {
	mu   sync.Mutex
	f    *os.File
	size int64
}

var lg = &logger{}

var consoleEcho bool

func (l *logger) open() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		l.f.Close()
	}
	path := filepath.Join(exeDir, "netwatch.log")
	if st, err := os.Stat(path); err == nil && st.Size() > cfg.LogMaxBytes {
		os.Remove(path + ".1")
		os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		l.f = nil
		return
	}
	l.f = f
	l.size = 0
	if st, err := f.Stat(); err == nil {
		l.size = st.Size()
	}
}

func (l *logger) logf(level, format string, args ...any) {
	line := fmt.Sprintf("%s %-5s %s\n", time.Now().Format("2006-01-02 15:04:05.000"), level, fmt.Sprintf(format, args...))
	l.mu.Lock()
	if l.f == nil || l.size > cfg.LogMaxBytes {
		l.mu.Unlock()
		l.open()
		l.mu.Lock()
	}
	if l.f != nil {
		n, _ := l.f.WriteString(line)
		l.size += int64(n)
		l.f.Sync()
	}
	l.mu.Unlock()
	if consoleEcho {
		emit(line)
	}
}

// debug 只在意 NETWATCH_DEBUG 时输出，避免日常日志噪音
func (l *logger) debug(format string, args ...any) {
	if os.Getenv("NETWATCH_DEBUG") != "" {
		l.logf("DEBUG", format, args...)
	}
}

// ---------------------------------------------------------------- 单实例

type instanceState int

const (
	instAcquired instanceState = iota
	instAlreadyRunning
	instFailed
)

// acquireInstance 用命名互斥体保证单实例。失败时返回 instFailed，调用方必须当作"不能启动"处理。
func acquireInstance() instanceState {
	name, _ := syscall.UTF16PtrFromString(mutexName)
	h, _, err := pCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(name)))
	if h == 0 {
		return instFailed
	}
	if err == syscall.ERROR_ALREADY_EXISTS {
		return instAlreadyRunning
	}
	return instAcquired
}

// ---------------------------------------------------------------- WLAN：取当前 SSID（无进程 spawn）
type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

type wlanInterfaceInfo struct {
	InterfaceGUID guid
	Description   [256]uint16
	State         uint32
}

type wlanInterfaceInfoList struct {
	NumberOfItems uint32
	Index         uint32
	InterfaceInfo [1]wlanInterfaceInfo
}

const (
	// WLAN_CONNECTION_ATTRIBUTES 里 DOT11_SSID 的偏移（isState 4 + mode 4 + profileName 512）
	offSSIDLen = 520
	offSSID    = 524
)

// currentSSID 返回当前连接 SSID；ok=false 表示读取失败（视为未知，不做动作）。
func currentSSID() (ssid string, ok bool) {
	var negotiated uint32
	var handle syscall.Handle
	r, _, _ := pWlanOpenHandle.Call(2, 0, uintptr(unsafe.Pointer(&negotiated)), uintptr(unsafe.Pointer(&handle)))
	if r != 0 {
		return "", false
	}
	defer pWlanCloseHandle.Call(uintptr(handle), 0)

	var list *wlanInterfaceInfoList
	r, _, _ = pWlanEnumIface.Call(uintptr(handle), 0, uintptr(unsafe.Pointer(&list)))
	if r != 0 || list == nil {
		return "", false
	}
	defer pWlanFreeMemory.Call(uintptr(unsafe.Pointer(list)))
	if list.NumberOfItems == 0 {
		return "", true // 没有无线网卡 → 必然不是目标网络
	}

	count := int(list.NumberOfItems)
	stride := unsafe.Sizeof(wlanInterfaceInfo{})
	base := unsafe.Pointer(&list.InterfaceInfo[0])
	for i := 0; i < count; i++ {
		info := (*wlanInterfaceInfo)(unsafe.Add(base, uintptr(i)*stride))
		var size uint32
		var data unsafe.Pointer
		var opcodeType uint32
		r, _, _ = pWlanQueryIface.Call(uintptr(handle), uintptr(unsafe.Pointer(&info.InterfaceGUID)),
			wlanOpcodeCurrentConnection, 0,
			uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&data)), uintptr(unsafe.Pointer(&opcodeType)))
		if r != 0 || data == nil || size < offSSID {
			continue // 该网卡未连接
		}
		n := *(*uint32)(unsafe.Add(data, offSSIDLen))
		if n > 32 {
			n = 32
		}
		// 必须校验缓冲区真的够长：驱动返回短缓冲时按 n 读会越界
		if uintptr(size) < uintptr(offSSID)+uintptr(n) {
			pWlanFreeMemory.Call(uintptr(data))
			continue
		}
		raw := unsafe.Slice((*byte)(unsafe.Add(data, offSSID)), n)
		s := string(append([]byte(nil), raw...))
		pWlanFreeMemory.Call(uintptr(data))
		return s, true
	}
	return "", true
}

// ---------------------------------------------------------------- Verge 目录 / yaml
func vergeDir() string {
	appdata := os.Getenv("APPDATA")
	if appdata == "" {
		return ""
	}
	for _, name := range []string{"io.github.clash-verge-rev.clash-verge-rev", "clash-verge"} {
		d := filepath.Join(appdata, name)
		if _, err := os.Stat(filepath.Join(d, "verge.yaml")); err == nil {
			return d
		}
	}
	return ""
}

var (
	reSecret = regexp.MustCompile(`(?m)^secret:\s*(\S+)\s*$`)
	rePort   = regexp.MustCompile(`(?m)^verge_mixed_port:\s*(\d+)\s*$`)
	reTunKey = regexp.MustCompile(`(?m)^enable_tun_mode:.*$`)
	reSpKey  = regexp.MustCompile(`(?m)^enable_system_proxy:.*$`)
)

func proxyPort() int {
	if cfg.ProxyPort > 0 && cfg.ProxyPort < 65536 {
		return cfg.ProxyPort
	}
	if d := vergeDir(); d != "" {
		if raw, err := os.ReadFile(filepath.Join(d, "verge.yaml")); err == nil {
			if m := rePort.FindSubmatch(raw); m != nil {
				if p, err := strconv.Atoi(string(m[1])); err == nil && p > 0 && p < 65536 {
					return p
				}
			}
		}
	}
	return 7897
}

// syncVergeYAML 保证 Verge 重启/内核重载后不会把状态翻回去。
func syncVergeYAML(on bool) {
	if !cfg.SyncVergeConfig {
		return
	}
	d := vergeDir()
	if d == "" {
		return
	}
	path := filepath.Join(d, "verge.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	val := fmt.Sprintf("%s: %t", "enable_tun_mode", on)
	next := reTunKey.ReplaceAll(raw, []byte(val))
	next = reSpKey.ReplaceAll(next, []byte(fmt.Sprintf("%s: %t", "enable_system_proxy", on)))
	if bytes.Equal(raw, next) {
		return
	}
	tmp := path + ".netwatch.tmp"
	if os.WriteFile(tmp, next, 0644) == nil {
		if os.Rename(tmp, path) == nil {
			lg.logf("INFO", "已同步 verge.yaml：%t", on)
		} else {
			os.Remove(tmp)
		}
	}
}

// ---------------------------------------------------------------- 内核控制器（命名管道 HTTP）
var secretCache atomic.Value // string

func findCorePipes() []string {
	pat, _ := syscall.UTF16PtrFromString(`\\.\pipe\verge-mihomo*`)
	var d syscall.Win32finddata
	h, err := syscall.FindFirstFile(pat, &d)
	if h == syscall.InvalidHandle || err != nil {
		return nil
	}
	defer syscall.FindClose(h)
	var out []string
	for {
		out = append(out, `\\.\pipe\`+syscall.UTF16ToString(d.FileName[:]))
		if syscall.FindNextFile(h, &d) != nil {
			return out
		}
	}
}

func secretCandidates() []string {
	var out []string
	if v, ok := secretCache.Load().(string); ok && v != "" {
		out = append(out, v)
	}
	if d := vergeDir(); d != "" {
		if raw, err := os.ReadFile(filepath.Join(d, "config.yaml")); err == nil {
			if m := reSecret.FindSubmatch(raw); m != nil {
				s := string(m[1])
				if len(out) == 0 || out[0] != s {
					out = append(out, s)
				}
			}
		}
	}
	out = append(out, "set-your-secret")
	return out
}

// pipeHTTP 发一条 HTTP 请求到命名管道，带超时（超时靠关闭句柄打断阻塞读）。
func pipeHTTP(pipe, method, path string, body []byte, secret string, timeout time.Duration) (int, []byte, error) {
	name, _ := syscall.UTF16PtrFromString(pipe)
	h, err := syscall.CreateFile(name, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_EXISTING, 0, 0)
	if err != nil {
		return 0, nil, err
	}
	var once sync.Once
	closeH := func() { once.Do(func() { syscall.CloseHandle(h) }) }
	defer closeH()

	var sb strings.Builder
	fmt.Fprintf(&sb, "%s %s HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n", method, path)
	if secret != "" {
		fmt.Fprintf(&sb, "Authorization: Bearer %s\r\n", secret)
	}
	if body != nil {
		fmt.Fprintf(&sb, "Content-Type: application/json\r\nContent-Length: %d\r\n", len(body))
	}
	sb.WriteString("\r\n")
	req := append([]byte(sb.String()), body...)

	type result struct {
		status int
		body   []byte
		err    error
	}
	ch := make(chan result, 1)
	go func() {
		var n uint32
		if err := syscall.WriteFile(h, req, &n, nil); err != nil {
			ch <- result{err: err}
			return
		}
		var buf []byte
		tmp := make([]byte, 4096)
		for {
			if err := syscall.ReadFile(h, tmp, &n, nil); err != nil || n == 0 {
				break
			}
			buf = append(buf, tmp[:n]...)
			if len(buf) > maxResponseBytes { // 防止异常对端把内存吃光
				break
			}
			if i := bytes.Index(buf, []byte("\r\n\r\n")); i >= 0 {
				if cl := contentLength(buf[:i]); cl >= 0 && len(buf)-i-4 >= cl {
					break
				}
			}
		}
		st, b := parseHTTP(buf)
		ch <- result{status: st, body: b}
	}()

	select {
	case r := <-ch:
		return r.status, r.body, r.err
	case <-time.After(timeout):
		closeH() // 关句柄打断阻塞读
		select {
		case <-ch:
		case <-time.After(300 * time.Millisecond):
			// 读线程仍未退出（句柄关闭没能打断同步 I/O）。不再等，交给进程退出回收。
			lg.logf("WARN", "管道读线程未在超时后退出，已放弃等待")
		}
		return 0, nil, errors.New("pipe timeout")
	}
}

func contentLength(head []byte) int {
	for _, line := range strings.Split(string(head), "\r\n") {
		l := strings.ToLower(line)
		if strings.HasPrefix(l, "content-length:") {
			if n, err := strconv.Atoi(strings.TrimSpace(line[len("content-length:"):])); err == nil {
				return n
			}
		}
	}
	return -1
}

func parseHTTP(raw []byte) (int, []byte) {
	i := bytes.Index(raw, []byte("\r\n\r\n"))
	if i < 0 {
		return 0, nil
	}
	head, body := raw[:i], raw[i+4:]
	parts := strings.Fields(string(head))
	if len(parts) < 2 {
		return 0, nil
	}
	st, _ := strconv.Atoi(parts[1])
	if cl := contentLength(head); cl >= 0 && cl <= len(body) {
		body = body[:cl]
	}
	return st, body
}

// coreCall 依次尝试管道与密钥；返回 status==0 表示不可达。
func coreCall(method, path string, payload any) (int, []byte) {
	var body []byte
	if payload != nil {
		body, _ = json.Marshal(payload)
	}
	for _, pipe := range findCorePipes() {
		secrets := secretCandidates() // 提到管道循环外，避免每个管道都重读 config.yaml
		// 写操作放宽超时：PATCH tun.enable 会同步重建 TUN，耗时以秒计
		timeout := pipeTimeout
		if method == "PATCH" || method == "PUT" || method == "POST" || method == "DELETE" {
			timeout = patchTimeout
		}
		for _, sec := range secrets {
			st, resp, err := pipeHTTP(pipe, method, path, body, sec, timeout)
			if err != nil {
				break // 这个管道不通，换下一个
			}
			if st == 401 {
				continue
			}
			secretCache.Store(sec)
			return st, resp
		}
	}
	return 0, nil
}

// tunConfig 取当前 tun 段（保持其它字段不变，避免 PATCH 把配置清空）。
func tunConfig() (map[string]any, bool) {
	st, body := coreCall("GET", "/configs", nil)
	if st != 200 {
		return nil, false
	}
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return nil, false
	}
	tun, ok := m["tun"].(map[string]any)
	return tun, ok
}

// coreInfo 只查 /configs（一次管道往返）；版本号留给 -status 单独查，避免每次判定多付一次往返。
func coreInfo() (tun bool, reachable bool) {
	if st, body := coreCall("GET", "/configs", nil); st == 200 {
		reachable = true
		var m map[string]any
		if json.Unmarshal(body, &m) == nil {
			if t, ok := m["tun"].(map[string]any); ok {
				tun, _ = t["enable"].(bool)
			}
		}
	}
	return tun, reachable
}

func coreVersion() string {
	if st, body := coreCall("GET", "/version", nil); st == 200 {
		var m map[string]any
		if json.Unmarshal(body, &m) == nil {
			v, _ := m["version"].(string)
			return v
		}
	}
	return ""
}

func setTun(on bool) error {
	tun, ok := tunConfig()
	if !ok {
		return errors.New("内核不可达")
	}
	if b, _ := tun["enable"].(bool); b == on {
		return nil
	}
	tun["enable"] = on
	// 网络刚切换时 mihomo 可能正在重载配置/重建监听，管道会短暂不可用（返回 0 或超时）。
	// 单次失败会把"开 TUN"推迟到下一轮校准（实测晚 11s），这里做短重试；
	// PATCH 是幂等的，重试前先复查一次 enable，避免重复下发。
	payload := map[string]any{"tun": tun}
	var st int
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			time.Sleep(200 * time.Millisecond)
			if t2, ok2 := tunConfig(); ok2 {
				if b, _ := t2["enable"].(bool); b == on {
					return nil // 上一次 PATCH 其实已生效，只是响应没回来
				}
			}
		}
		st, _ = coreCall("PATCH", "/configs", payload)
		if st == 200 || st == 204 {
			return nil
		}
	}
	// 最后一次也可能是"已经生效但响应丢了"：回读确认，别误报失败让上层以为半开
	if t2, ok2 := tunConfig(); ok2 {
		if b, _ := t2["enable"].(bool); b == on {
			return nil
		}
	}
	return fmt.Errorf("PATCH /configs 返回 %d", st)
}

// ---------------------------------------------------------------- 系统代理（注册表）
const regPath = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`

func regOpenRead() (syscall.Handle, error) {
	p, _ := syscall.UTF16PtrFromString(regPath)
	var h syscall.Handle
	r, _, _ := pRegOpenKeyExW.Call(hkeyCurrentUser, uintptr(unsafe.Pointer(p)), 0, keyRead,
		uintptr(unsafe.Pointer(&h)))
	if r != 0 {
		return 0, fmt.Errorf("RegOpenKeyEx 失败: %d", r)
	}
	return h, nil
}

func regOpen(access uint32) (syscall.Handle, error) {
	p, _ := syscall.UTF16PtrFromString(regPath)
	var h syscall.Handle
	r, _, _ := pRegCreateKeyExW.Call(hkeyCurrentUser, uintptr(unsafe.Pointer(p)), 0, 0, 0,
		uintptr(access), 0, uintptr(unsafe.Pointer(&h)), 0)
	if r != 0 {
		return 0, fmt.Errorf("RegCreateKeyEx 失败: %d", r)
	}
	return h, nil
}

func regSetDword(h syscall.Handle, name string, v uint32) error {
	n, _ := syscall.UTF16PtrFromString(name)
	r, _, _ := pRegSetValueExW.Call(uintptr(h), uintptr(unsafe.Pointer(n)), 0, regDWORD,
		uintptr(unsafe.Pointer(&v)), 4)
	if r != 0 {
		return fmt.Errorf("RegSetValueEx(%s) 失败: %d", name, r)
	}
	return nil
}

func regSetString(h syscall.Handle, name, v string) error {
	n, _ := syscall.UTF16PtrFromString(name)
	val, _ := syscall.UTF16FromString(v)
	r, _, _ := pRegSetValueExW.Call(uintptr(h), uintptr(unsafe.Pointer(n)), 0, regSZ,
		uintptr(unsafe.Pointer(&val[0])), uintptr(len(val)*2))
	if r != 0 {
		return fmt.Errorf("RegSetValueEx(%s) 失败: %d", name, r)
	}
	return nil
}

func regGet(h syscall.Handle, name string) (uint32, string, error) {
	n, _ := syscall.UTF16PtrFromString(name)
	var typ, size uint32
	r, _, _ := pRegQueryValueExW.Call(uintptr(h), uintptr(unsafe.Pointer(n)), 0,
		uintptr(unsafe.Pointer(&typ)), 0, uintptr(unsafe.Pointer(&size)))
	if r != 0 || size == 0 || size > 8192 {
		return typ, "", errors.New("读取失败")
	}
	buf := make([]byte, size)
	r, _, _ = pRegQueryValueExW.Call(uintptr(h), uintptr(unsafe.Pointer(n)), 0,
		uintptr(unsafe.Pointer(&typ)), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)))
	if r != 0 {
		return typ, "", errors.New("读取失败")
	}
	if typ == regDWORD {
		if size < 4 { // 值被写坏时不要越界读
			return typ, "", errors.New("DWORD 长度异常")
		}
		return typ, strconv.Itoa(int(*(*uint32)(unsafe.Pointer(&buf[0])))), nil
	}
	return typ, syscall.UTF16ToString(unsafe.Slice((*uint16)(unsafe.Pointer(&buf[0])), size/2)), nil
}

func sysproxyState() (enabled bool, server string) {
	h, err := regOpenRead()
	if err != nil {
		return false, ""
	}
	defer pRegCloseKey.Call(uintptr(h))
	if _, v, err := regGet(h, "ProxyEnable"); err == nil {
		enabled = v == "1"
	}
	if _, v, err := regGet(h, "ProxyServer"); err == nil {
		server = v
	}
	return enabled, server
}

func setSysproxy(on bool, port int) error {
	h, err := regOpen(keyReadWrite)
	if err != nil {
		return err
	}
	defer pRegCloseKey.Call(uintptr(h))
	if on {
		if err := regSetString(h, "ProxyServer", fmt.Sprintf("127.0.0.1:%d", port)); err != nil {
			return err
		}
		if err := regSetDword(h, "ProxyEnable", 1); err != nil {
			return err
		}
	} else if err := regSetDword(h, "ProxyEnable", 0); err != nil {
		return err
	}
	pInternetSetOption.Call(0, internetOptionSettingsChanged, 0, 0)
	pInternetSetOption.Call(0, internetOptionRefresh, 0, 0)
	return nil
}

// ---------------------------------------------------------------- 判定
var normReplacer = strings.NewReplacer(" ", "", "\t", "", "_", "", "-", "", "·", "", ".", "")

func norm(s string) string {
	return strings.ToLower(normReplacer.Replace(s))
}

func matchOne(ssid, target string) bool {
	a, b := norm(ssid), norm(target)
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	if !cfg.FuzzyMatch {
		return false
	}
	// 前缀容错必须"够像"：短的一方至少 6 字节（约 2 个汉字），且长度差 ≤4，
	// 否则任何一个短前缀网络（如一个叫"MyPhone"的邻居热点）都会误判成目标——
	// 对自动连接来说误判意味着真的把 WiFi 切过去，必须收紧。
	short, long := a, b
	if len(short) > len(long) {
		short, long = long, short
	}
	if len(short) < 6 || len(long)-len(short) > 4 {
		return false
	}
	return strings.HasPrefix(long, short)
}

func matchList(ssid string, list []string) bool {
	for _, t := range list {
		if matchOne(ssid, t) {
			return true
		}
	}
	return false
}

// matches 判断当前网络是否属于"开代理"的目标网络集合。
func matches(ssid string) bool {
	if ssid == "" || len(cfg.TargetSSIDs) == 0 {
		return false
	}
	return matchList(ssid, cfg.TargetSSIDs)
}

// autoConnectTarget 判断该网络是否允许被自动连接：
// 必须是目标网络，且（auto_connect_ssids 为空，或它被显式列在其中）。
func autoConnectTarget(ssid string) bool {
	if !matches(ssid) {
		return false
	}
	if len(cfg.AutoConnectSSIDs) == 0 {
		return true
	}
	return matchList(ssid, cfg.AutoConnectSSIDs)
}

// isNeverProxy 判定当前网络是否属于「永不代理」名单。用归一化后的前缀匹配，
// 覆盖 "Campus-WiFi"、"Campus-WiFi 5G"、"Campus-WiFi-Guest" 这类变体。
// 名单完全来自配置，由首次运行向导或用户手工填写。
func isNeverProxy(ssid string) bool {
	if ssid == "" {
		return false
	}
	n := norm(ssid)
	if n == "" {
		return false
	}
	for _, b := range cfg.NeverProxySSIDs {
		if nb := norm(b); nb != "" && strings.HasPrefix(n, nb) {
			return true
		}
	}
	return false
}

// effectiveWant 是唯一的目标判定入口：即使 fuzzy 把某网络当成目标，
// 只要它命中永不代理名单，一律判为 OFF。所有决策点都必须走这里，
// 不允许任何路径绕过它去开代理。
func effectiveWant(ssid string) bool {
	return matches(ssid) && !isNeverProxy(ssid)
}

// ---------------------------------------------------------------- 执行
type snapshot struct {
	tun     bool
	coreOK  bool
	proxyOn bool
	server  string
	port    int
}

func readSnapshot() snapshot {
	tun, ok := coreInfo()
	on, srv := sysproxyState()
	return snapshot{tun: tun, coreOK: ok, proxyOn: on, server: srv, port: proxyPort()}
}

func inSync(s snapshot, want bool) bool {
	if want {
		return s.coreOK && s.tun && s.proxyOn && s.server == fmt.Sprintf("127.0.0.1:%d", s.port)
	}
	return !s.tun && !s.proxyOn
}

var lastSwitch atomic.Value // string: "on"/"off"
var lastSSID string         // 最近一次判定时的网络名，供托盘提示用

// 暂停自动切换（托盘菜单触发），避免守护进程和用户手动操作互相打架。
// 到期时间同时写盘，进程被心跳/重启拉起后暂停不会丢。
var pauseUntil atomic.Int64

func pauseFile() string { return filepath.Join(exeDir, "paused.until") }

func autoPaused() bool {
	if time.Now().UnixNano() < pauseUntil.Load() {
		return true
	}
	raw, err := os.ReadFile(pauseFile())
	if err != nil {
		return false
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return false
	}
	if time.Now().Unix() < ts {
		return true
	}
	os.Remove(pauseFile())
	return false
}

func pauseFor(d time.Duration) {
	until := time.Now().Add(d)
	pauseUntil.Store(until.UnixNano())
	os.WriteFile(pauseFile(), []byte(strconv.FormatInt(until.Unix(), 10)), 0644)
}

func resumeAuto() {
	pauseUntil.Store(0)
	os.Remove(pauseFile())
}

// pauseRemain 返回剩余暂停时间（读盘，供 -status 用）
func pauseRemain() time.Duration {
	if !autoPaused() {
		return 0
	}
	raw, err := os.ReadFile(pauseFile())
	if err != nil {
		return time.Minute // 内存里暂停但文件读不到，给个非零值
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return time.Minute
	}
	return time.Until(time.Unix(ts, 0))
}

// 通知判据：只在"网络或目标状态相对上次通知真的变了"时弹通知。
// 基准是「上次通知时的 (网络, 运行态)」，不是目标态——这样：
//   - 断网（网络变）→ 弹"已关闭"
//   - 重连（网络变）→ 弹"已开启"
//   - 外部关 TUN 被拉回（网络没变，运行态最终也没变）→ 不弹
//   - 启动时已在目标状态 → 不弹
//   - 启动时状态不对（如 config 改过、进程停期间状态漂了）→ 弹
var (
	lastNotifiedSSID  string
	lastNotifiedState bool // 上次通知时的运行态（代理+TUN 是否都开）
)

// networkChanged 判定：相对上次通知，网络或运行态是否真的变了。
func networkChanged(ssid string, runningOn bool) bool {
	if ssid == lastNotifiedSSID && runningOn == lastNotifiedState {
		return false
	}
	lastNotifiedSSID, lastNotifiedState = ssid, runningOn
	return true
}

// resetNotifyState 进程启动时调用：用「当前网络 + 当前实际运行态」初始化基准。
// 这样第一次 apply 如果改了运行态（状态漂了/config 改过）→ 弹；
// 如果状态本就在目标值（inSync 跳过 apply）→ 不弹。
func resetNotifyState() {
	ssid, ok := currentSSID()
	if !ok {
		ssid = ""
	}
	s := readSnapshot()
	lastNotifiedSSID = ssid
	lastNotifiedState = s.tun && s.proxyOn
}

// apply 把运行态推到 want，返回是否成功。evtAt 是本轮触发事件的时刻（纳秒，0 表示非事件触发）。
func apply(want bool, reason string, evtAt int64) bool {
	start := time.Now()
	s := readSnapshot()
	port := s.port

	if want && !s.coreOK {
		lg.logf("WARN", "内核不可达（%s），暂不开启，等待下次校准", reason)
		// Verge 内核重启时 TUN 随内核消失，但系统代理写在注册表里不会跟着清。
		// 11-24 17:40 就是 tun=false/proxy=true/core=false 的半开态：流量全进
		// 127.0.0.1 的死端口，用户彻底断网。既然开不了 TUN，就把残留代理也关掉，
		// 让流量直连；内核恢复后下一轮校准会重新把代理+TUN 一起开起来。
		if s.proxyOn {
			if e := setSysproxy(false, port); e != nil {
				lg.logf("ERROR", "内核不可达时清理残留系统代理失败：%v", e)
			} else {
				lg.logf("INFO", "内核不可达，已关闭残留系统代理避免断网（内核恢复后会自动重开）")
			}
		}
		return false
	}

	var err error
	var tTun, tProxy int64
	if want {
		// 先开系统代理（快，~200ms 内流量就能走代理），再建 TUN（mihomo 重建 Wintun 需 1s+）
		t0 := time.Now()
		err = setSysproxy(true, port)
		tProxy = time.Since(t0).Milliseconds()
		if err == nil {
			t0 = time.Now()
			if e := setTun(true); e != nil {
				err = e
				if rbErr := setSysproxy(false, port); rbErr != nil { // 回滚也要确认，否则会留半开状态
					lg.logf("ERROR", "TUN 开启失败且回滚系统代理失败：%v", rbErr)
				}
			}
			tTun = time.Since(t0).Milliseconds()
		}
	} else {
		t0 := time.Now()
		if e := setSysproxy(false, port); e != nil {
			err = e
		}
		tProxy = time.Since(t0).Milliseconds()
		t0 = time.Now()
		if e := setTun(false); e != nil && err == nil {
			err = e
		}
		tTun = time.Since(t0).Milliseconds()
	}
	if err != nil {
		lg.logf("ERROR", "切换到 %s 失败：%v", stateName(want), err)
		return false
	}
	syncVergeYAML(want)
	lastSwitch.Store(stateName(want))

	total := time.Since(start)
	evt := "非事件触发"
	if evtAt > 0 {
		evt = fmt.Sprintf("事件→完成 %dms", time.Since(time.Unix(0, evtAt)).Milliseconds())
	}
	// 只回读注册表（微秒级），不再为了一行日志多打一次管道往返
	proxyOn, _ := sysproxyState()
	lg.logf("INFO", "切换 %s via %q 完成：代理 %dms + TUN %dms = %dms（%s）；tun=%t proxy=%t",
		stateName(want), reason, tProxy, tTun, total.Milliseconds(), evt, want, proxyOn)

	// 界面反馈：只在"网络或运行态相对上次通知真的变了"时弹，后台纠错不弹。
	if networkChanged(lastSSID, want) {
		ssidText := lastSSID
		if ssidText == "" {
			ssidText = "未连接网络"
		}
		action := "已关闭代理与 TUN"
		if want {
			action = "已开启代理与 TUN"
		}
		trayNotify(lastSSID, want, fmt.Sprintf("%s · %s · %dms", action, ssidText, total.Milliseconds()))
	} else {
		// 同网络下的后台纠错：只更新图标提示，不弹窗
		trayNotify(lastSSID, want, "")
	}
	return true
}

func stateName(on bool) string {
	if on {
		return "ON"
	}
	return "OFF"
}

// reconcile 是唯一的决策入口：网络变化、定时校准、启动都走这里。
func reconcile(reason string) {
	evtAt := atomic.SwapInt64(&eventAtNano, 0) // 进入时就把触发时刻取走，避免被自己产生的后续事件覆盖
	ssid, ok := currentSSID()
	if !ok {
		lg.logf("WARN", "读取 SSID 失败，保持现状（%s）", reason)
		return
	}
	want := effectiveWant(ssid)
	lastSSID = ssid
	trayNotify(ssid, want, "") // 每次判定都刷新托盘提示（不发通知）
	if want {
		noteTargetSeen() // 稳定连上目标 3s 才清失败退避，防抖动刷掉冷却
	} else {
		noteTargetGone() // 不在目标：打断稳定观察，重新计时
	}

	// 断网瞬间往往只是换网中途：等 250ms 再确认一次，避免关→开来回抖
	if !want && ssid == "" {
		time.Sleep(250 * time.Millisecond)
		if s2, ok2 := currentSSID(); ok2 && s2 != "" {
			ssid, want = s2, effectiveWant(s2)
			// 复读拿到了新网络，必须同步更新 lastSSID：
			// 否则 apply 里构造通知文案时用的还是空字符串，会误报"未连接网络"
			lastSSID = ssid
			trayNotify(ssid, want, "")
		}
	}

	// 顺序不能反：先把代理/TUN 按当前 SSID 归位，再尝试自动连热点。
	// 2026-11-25 的事故就是反了——候选里一直有MyPhone，每轮都"发起连接→return"，
	// 代理挂在 Campus-WiFi 上 85 秒没关。当前网络不是目标就必须立刻关，这是一号需求。
	//
	// 暂停语义：只暂停「开」和「自动连」，绝不暂停「关」。
	// 这是硬约束——Campus-WiFi 等禁止网络下就算用户点了暂停，代理也必须立刻断开。
	// 2026-11-25 审查发现旧代码在暂停时直接 return，切到 Campus-WiFi 后代理会一直挂着。
	s := readSnapshot()
	if !inSync(s, want) && (want && !autoPaused() || !want) {
		detect := "定时/启动"
		if evtAt > 0 {
			detect = fmt.Sprintf("事件→判定 %dms", time.Since(time.Unix(0, evtAt)).Milliseconds())
		}
		lg.logf("INFO", "状态不一致（%s，%s）：ssid=%q 目标=%s tun=%t proxy=%t core=%v",
			reason, detect, ssid, stateName(want), s.tun, s.proxyOn, s.coreOK)
		apply(want, reason, evtAt)
	}

	// 状态归位之后再连。连上后由网络事件走下一轮 reconcile → ON。
	// 即使连接失败也无所谓：本轮已经按"非目标"关干净了，不会挂着代理。
	// 暂停期间不自动连热点（用户完全接管），但上面的「关」不受暂停影响。
	if !want && !autoPaused() {
		maybeAutoConnect(reason)
	}
}

// ---------------------------------------------------------------- 事件源
var wake = make(chan struct{}, 1)

func signal() {
	atomic.StoreInt64(&eventAtNano, time.Now().UnixNano())
	select {
	case wake <- struct{}{}:
	default:
	}
}

func watchAddrChanges() {
	ev, _, _ := pCreateEventW.Call(0, 1, 0, 0) // 手动重置事件
	if ev == 0 {
		lg.logf("ERROR", "CreateEvent 失败，IP 变更事件源不可用")
		return
	}
	ov := &syscall.Overlapped{HEvent: syscall.Handle(ev)}
	const errorIOPending = 997
	for {
		r, _, _ := pNotifyAddrChange.Call(0, uintptr(unsafe.Pointer(ov)))
		switch {
		case r == 0:
			// 已有变更待上报，立即返回。加最小间隔，避免连续变更时把 CPU 打满。
			signal()
			time.Sleep(50 * time.Millisecond)
			continue
		case r == errorIOPending:
			// 正常异步路径：等事件（无超时，靠进程退出回收）
			pWaitSingle.Call(ev, 0xFFFFFFFF) // INFINITE
			pResetEvent.Call(ev)
			signal()
		default:
			lg.logf("ERROR", "NotifyAddrChange 失败（错误码 %d），5 秒后重试", r)
			time.Sleep(5 * time.Second)
		}
	}
}

// watchNeverProxy 是「永不代理」的独立防线，每 2 秒巡检一次，刻意与事件/暂停/自动连接解耦：
// 禁止网络（Campus-WiFi）下只要检测到系统代理开着，就直接强制关闭，不经过 reconcile 的暂停分支。
// 它只写 HKCU 注册表（微秒级、不经内核管道），因此内核挂掉时同样能清掉代理。
// TUN 是否随内核消失由内核自身决定；守护进程能关的部分一定关掉。
func watchNeverProxy() {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for range t.C {
		ssid, ok := currentSSID()
		if !ok || !isNeverProxy(ssid) {
			continue
		}
		on, srv := sysproxyState()
		if !on {
			continue
		}
		port := proxyPort()
		lg.logf("WARN", "看门狗：%q 属永不代理网络但系统代理开着（%s），强制关闭", ssid, srv)
		if err := setSysproxy(false, port); err != nil {
			lg.logf("ERROR", "看门狗强制关闭系统代理失败：%v", err)
			continue
		}
		syncVergeYAML(false)
		lg.logf("INFO", "看门狗已关闭系统代理（%q，端口 %d）", ssid, port)
	}
}

func evtCallback(action, userContext, event, eventContext uintptr) uintptr {
	if action == evtSubscribeActionDeliver {
		signal()
	}
	return 0
}

// evtSubHandle 保留订阅句柄，避免被 GC 回收后订阅失效（也便于将来优雅退出时 EvtClose）。
var evtSubHandle uintptr

func watchNetworkProfile() error {
	ch, err := syscall.UTF16PtrFromString("Microsoft-Windows-NetworkProfile/Operational")
	if err != nil {
		return err
	}
	q, err := syscall.UTF16PtrFromString("*[System[(EventID=10000 or EventID=10001)]]")
	if err != nil {
		return err
	}
	cb := syscall.NewCallback(evtCallback)
	h, _, errno := pEvtSubscribe.Call(0, 0, uintptr(unsafe.Pointer(ch)), uintptr(unsafe.Pointer(q)),
		0, 0, cb, evtSubscribeToFutureEvents)
	if h == 0 {
		return fmt.Errorf("EvtSubscribe 失败: %v", errno)
	}
	evtSubHandle = h
	return nil
}

// ---------------------------------------------------------------- 刷新 Verge 界面状态
//
// Verge 的开关显示的是它自己进程内存里的配置（启动时从 verge.yaml 读一次，之后只跟着
// 用户点击更新）。我们从内核 API / 注册表改真实状态，它感知不到，界面就停在旧值。
// Tauri 没暴露"重载配置"的外部接口，唯一能让界面重新初始化的办法是重启 GUI 进程。
// 实测：内核挂在 clash-verge-service 下（不是 GUI 的子进程），但重启 GUI 会连带换内核，
// 期间代理中断几秒、TUN 重建。

type procEntry struct {
	Size            uint32
	CntUsage        uint32
	ProcessID       uint32
	DefaultHeapID   uintptr
	ModuleID        uint32
	CntThreads      uint32
	ParentProcessID uint32
	PriClassBase    int32
	Flags           uint32
	ExeFile         [260]uint16
}

const (
	th32csSnapProcess = 0x00000002
	processTerminate  = 0x0001
	processQueryInfo  = 0x1000
)

func imagePath(pid uint32) string {
	h, err := syscall.OpenProcess(processQueryInfo, false, pid)
	if err != nil {
		return ""
	}
	defer syscall.CloseHandle(h)
	buf := make([]uint16, 1024)
	size := uint32(len(buf))
	if r, _, _ := pQueryFullName.Call(uintptr(h), 0,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size))); r == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf[:size])
}

func findProcess(name string) (pid uint32, exe string, ok bool) {
	snap, _, _ := pSnapshot.Call(th32csSnapProcess, 0)
	if snap == 0 || snap == ^uintptr(0) {
		return 0, "", false
	}
	defer syscall.CloseHandle(syscall.Handle(snap))
	var e procEntry
	e.Size = uint32(unsafe.Sizeof(e))
	r, _, _ := pProc32First.Call(snap, uintptr(unsafe.Pointer(&e)))
	for r != 0 {
		if strings.EqualFold(syscall.UTF16ToString(e.ExeFile[:]), name) {
			return e.ProcessID, imagePath(e.ProcessID), true
		}
		r, _, _ = pProc32Next.Call(snap, uintptr(unsafe.Pointer(&e)))
	}
	return 0, "", false
}

func terminate(pid uint32) error {
	h, err := syscall.OpenProcess(processTerminate, false, pid)
	if err != nil {
		return fmt.Errorf("打开进程失败（可能需要管理员权限）：%v", err)
	}
	defer syscall.CloseHandle(h)
	if r, _, e := pTerminateProcess.Call(uintptr(h), 1); r == 0 {
		return fmt.Errorf("TerminateProcess 失败：%v", e)
	}
	return nil
}

func startProcess(exe string) error {
	cmd, err := syscall.UTF16FromString(`"` + exe + `"`)
	if err != nil {
		return err
	}
	dir, _ := syscall.UTF16PtrFromString(filepath.Dir(exe))
	var si syscall.StartupInfo
	var pi syscall.ProcessInformation
	if err := syscall.CreateProcess(nil, &cmd[0], nil, nil, false, 0, nil, dir, &si, &pi); err != nil {
		return err
	}
	syscall.CloseHandle(pi.Thread)
	syscall.CloseHandle(pi.Process)
	return nil
}

var (
	reTunBool = regexp.MustCompile(`(?m)^enable_tun_mode:[ \t]*(\S+)[ \t]*$`)
	reSpBool  = regexp.MustCompile(`(?m)^enable_system_proxy:[ \t]*(\S+)[ \t]*$`)
)

func vergeYAMLState() string {
	d := vergeDir()
	if d == "" {
		return "读不到 verge.yaml"
	}
	raw, err := os.ReadFile(filepath.Join(d, "verge.yaml"))
	if err != nil {
		return "读不到 verge.yaml"
	}
	tun, proxy := "?", "?"
	if m := reTunBool.FindSubmatch(raw); m != nil {
		tun = string(m[1])
	}
	if m := reSpBool.FindSubmatch(raw); m != nil {
		proxy = string(m[1])
	}
	return fmt.Sprintf("tun=%s 代理=%s", tun, proxy)
}

// latestLogMtime 取 logs 目录下最新的 .log 修改时间，用作"界面已初始化"的判据。
// Verge 启动时会立刻写日志，比"进程存在"更能说明它真的起来了。
func latestLogMtime() time.Time {
	d := vergeDir()
	if d == "" {
		return time.Time{}
	}
	entries, err := os.ReadDir(filepath.Join(d, "logs"))
	if err != nil {
		return time.Time{}
	}
	var newest time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".log") {
			continue
		}
		if info, err := e.Info(); err == nil && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	return newest
}

func cmdRefresh(soft bool) int {
	var sb strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&sb, format+"\n", a...) }

	ssid, okSSID := currentSSID()
	want := okSSID && effectiveWant(ssid)
	before := readSnapshot()
	daemonRunning := acquireInstance() == instAlreadyRunning
	p("当前网络 : %q → 目标 %s", ssid, stateName(want))
	p("刷新前   : 内核 tun=%t，注册表 代理=%t；verge.yaml %s", before.tun, before.proxyOn, vergeYAMLState())

	// [1/3] 真实状态先对齐目标，避免 Verge 重启后按错误的值初始化
	if !daemonRunning && !inSync(before, want) {
		apply(want, "手动刷新", 0)
	}
	syncVergeYAML(want)
	p("[1/3] 配置已同步：verge.yaml %s%s", vergeYAMLState(),
		map[bool]string{true: "（守护进程在跑，状态由它维护）", false: ""}[daemonRunning])

	if soft {
		p("[2/3] 跳过 GUI 重启（-soft）")
		p("注意：Verge 界面仍显示旧值，需要重启它才会重新读盘（去掉 -soft 重跑本命令）")
		emit(sb.String())
		return 0
	}

	pid, exe, ok := findProcess("clash-verge.exe")
	if !ok {
		p("[2/3] 未检测到 Clash Verge 进程 —— 界面不存在，无需刷新")
		emit(sb.String())
		return 0
	}
	if exe == "" {
		p("[2/3] 已找到进程 pid=%d 但读不到 exe 路径，请手动重启 Clash Verge", pid)
		emit(sb.String())
		return 1
	}

	t0 := time.Now()
	if err := terminate(pid); err != nil {
		p("[2/3] 终止进程失败：%v", err)
		p("请手动右键托盘图标退出 Clash Verge 后重新运行本命令")
		emit(sb.String())
		return 1
	}
	// 等旧进程真正退出（最多 5 秒）
	for i := 0; i < 50; i++ {
		if _, _, alive := findProcess("clash-verge.exe"); !alive {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	p("[2/3] 已终止旧进程 pid=%d（%dms）", pid, time.Since(t0).Milliseconds())

	t1 := time.Now()
	oldCorePid, _, _ := findProcess("verge-mihomo.exe")
	baseLog := latestLogMtime()
	if err := startProcess(exe); err != nil {
		p("[3/3] 启动失败：%v；请手动打开 Clash Verge", err)
		emit(sb.String())
		return 1
	}
	newPid, _, started := findProcess("clash-verge.exe")
	if !started {
		p("[3/3] 已调用启动但未检测到新进程，请手动打开 Clash Verge")
		emit(sb.String())
		return 1
	}
	// 等两个条件：Verge 写出新的启动日志（界面真初始化了），且内核可达。
	// 不能只看内核——内核挂在服务下，GUI 被杀时它还活着，会立刻造成"已就绪"的假象。
	var uiReady, coreReady bool
	var ver string
	for i := 0; i < 200; i++ { // 最多 20 秒
		if !uiReady && latestLogMtime().After(baseLog) {
			uiReady = true
		}
		if !coreReady {
			if v := coreVersion(); v != "" {
				coreReady, ver = true, v
			}
		}
		if uiReady && coreReady {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	elapsed := time.Since(t0)
	newCorePid, _, coreFound := findProcess("verge-mihomo.exe")
	coreNote := "内核沿用"
	if coreFound && oldCorePid != 0 && newCorePid != oldCorePid {
		coreNote = fmt.Sprintf("内核已换 pid=%d→%d", oldCorePid, newCorePid)
	}
	if !uiReady || !coreReady {
		p("[3/3] 新进程 pid=%d 已启动，但 20 秒内未确认界面就绪（日志=%t 内核=%t）", newPid, uiReady, coreReady)
		p("      请打开 Clash Verge 看一眼，必要时手动重启")
		emit(sb.String())
		return 1
	}
	p("[3/3] 已重启：pid=%d → %d，启动日志 %dms，内核 %s（%s；总 %dms）",
		pid, newPid, time.Since(t1).Milliseconds(), ver, coreNote, elapsed.Milliseconds())

	// 收尾校验
	after := readSnapshot()
	sync := inSync(after, want)
	p("刷新后   : 内核 tun=%t，注册表 代理=%t；verge.yaml %s", after.tun, after.proxyOn, vergeYAMLState())
	if !sync && !daemonRunning {
		apply(want, "刷新后校正", 0)
		after = readSnapshot()
		sync = inSync(after, want)
	}
	p("结果     : %s（总耗时 %dms）", map[bool]string{true: "一致", false: "仍不一致，请查看日志"}[sync],
		time.Since(t0).Milliseconds())
	lg.logf("INFO", "手动刷新：重启界面 pid=%d→%d 用时 %dms，结果 一致=%t",
		pid, newPid, elapsed.Milliseconds(), sync)
	emit(sb.String())
	if sync {
		return 0
	}
	return 1
}

// ---------------------------------------------------------------- 入口
var conOut *os.File

// emit 优先用继承来的 stdout（重定向/控制台都能拿到），拿不到才去 AttachConsole。
// 目标是真控制台时走 WriteConsoleW，避免按代码页解码导致中文乱码。
func emit(text string) {
	if os.Stdout != nil {
		if _, err := os.Stdout.Stat(); err == nil {
			writeOut(syscall.Handle(os.Stdout.Fd()), text)
			return
		}
	}
	if conOut == nil {
		attachConsole()
	}
	if conOut != nil {
		writeOut(syscall.Handle(conOut.Fd()), text)
	}
}

func writeOut(h syscall.Handle, text string) {
	var mode uint32
	if r, _, _ := pGetConsoleMode.Call(uintptr(h), uintptr(unsafe.Pointer(&mode))); r != 0 {
		u, err := syscall.UTF16FromString(text)
		if err == nil && len(u) > 1 {
			var written uint32
			pWriteConsoleW.Call(uintptr(h), uintptr(unsafe.Pointer(&u[0])),
				uintptr(len(u)-1), uintptr(unsafe.Pointer(&written)), 0)
			return
		}
	}
	// 重定向到管道/文件：直接写字节，不用 os.NewFile（它带关闭句柄的 finalizer）
	var n uint32
	syscall.WriteFile(h, []byte(text), &n, nil)
}

func attachConsole() {
	const attachParent = ^uintptr(0) // 0xFFFFFFFF
	if r, _, _ := pAttachConsole.Call(attachParent); r == 0 {
		return
	}
	name, _ := syscall.UTF16PtrFromString("CONOUT$")
	h, err := syscall.CreateFile(name, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil,
		syscall.OPEN_EXISTING, 0, 0)
	if err == nil {
		conOut = os.NewFile(uintptr(h), "CONOUT$")
	}
}

func runDaemon() int {
	switch acquireInstance() {
	case instAlreadyRunning:
		return 0 // 已有实例，静默退出（心跳/事件触发会频繁走到这里）
	case instFailed:
		return 1 // 拿不到互斥体，宁可不启动，也不冒双实例风险
	}
	lg.open()
	resetNotifyState() // 启动后第一次 apply 不弹通知，避免心跳复活刷屏
	node, _ := os.Hostname()
	lg.logf("INFO", "启动 pid=%d host=%s 目标=%q fuzzy=%t 校准=%ds",
		os.Getpid(), node, targetDisplay(), cfg.FuzzyMatch, cfg.ReconcileSeconds)

	err1 := watchNetworkProfile()
	go watchAddrChanges()
	go watchNeverProxy() // 永不代理网络的独立防线（不受暂停/自动连接影响）
	go startTray()       // 托盘 + 通知：让状态变化看得见
	if err1 != nil {
		lg.logf("WARN", "%v；仅依赖 IP 变更事件", err1)
	} else {
		lg.logf("INFO", "已订阅 NetworkProfile 事件")
	}

	reconcile("启动")

	tick := time.NewTicker(time.Duration(cfg.ReconcileSeconds) * time.Second)
	defer tick.Stop()
	// 候选轮询：NetworkProfile / IP 变更事件只在"连接变了"时触发，
	// 手机热点刚打开而电脑还连在校园网上时不会有任何事件，必须有这条兜底。
	// 只读 WLAN 缓存的候选列表（~1ms，不产生射频扫描），所以 15s 一次开销可忽略。
	autoTick := time.NewTicker(autoScanInterval())
	defer autoTick.Stop()
	for {
		select {
		case <-wake:
			for { // 合并同一批事件（含自己开关 TUN 产生的）
				select {
				case <-wake:
					continue
				default:
				}
				break
			}
			reconcile("网络事件")
		case <-tick.C:
			reconcile("定时校准")
		case <-autoTick.C:
			// 走完整校准而不仅是自动连接：发起连接后如果失败/被 Windows 回退，
			// 这里能在 15s 内把代理与 TUN 关掉，而不是等 60s 定时校准。
			reconcile("候选轮询")
		}
	}
}

func printStatus() {
	ssid, ok := currentSSID()
	s := readSnapshot()
	want := ok && effectiveWant(ssid)
	version := coreVersion()
	var b strings.Builder
	fmt.Fprintf(&b, "时间     : %s\n", time.Now().Format("2006-01-02 15:04:05"))
	if ok {
		fmt.Fprintf(&b, "当前网络 : %q\n", ssid)
	} else {
		fmt.Fprintf(&b, "当前网络 : <读取失败>\n")
	}
	fmt.Fprintf(&b, "目标     : %s（%s）\n", stateName(want), targetDisplay())
	if len(cfg.NeverProxySSIDs) > 0 {
		fmt.Fprintf(&b, "禁止代理 : %s\n", strings.Join(cfg.NeverProxySSIDs, ", "))
	}
	fmt.Fprintf(&b, "内核     : %v %s\n", s.coreOK, version)
	fmt.Fprintf(&b, "TUN      : %t\n", s.tun)
	fmt.Fprintf(&b, "系统代理 : %t server=%s（期望 127.0.0.1:%d）\n", s.proxyOn, s.server, s.port)
	fmt.Fprintf(&b, "是否一致 : %t\n", inSync(s, want))
	fmt.Fprintf(&b, "自动连热点 : %s\n", autoConnectStatusLine())
	if !inSync(s, want) {
		fmt.Fprintf(&b, "提示     : 需要重新加载界面状态时执行  netwatch.exe -refresh\n")
	}
	if d := pauseRemain(); d > 0 {
		fmt.Fprintf(&b, "自动切换 : 已暂停，还剩 %d 分钟（托盘菜单可恢复）\n", int(d.Minutes())+1)
	}
	switch acquireInstance() {
	case instAlreadyRunning:
		fmt.Fprintf(&b, "守护进程 : 运行中\n")
	case instAcquired:
		fmt.Fprintf(&b, "守护进程 : 未运行（互斥体空闲）\n")
	default:
		fmt.Fprintf(&b, "守护进程 : 未知（互斥体创建失败）\n")
	}
	if v, ok := lastSwitch.Load().(string); ok {
		fmt.Fprintf(&b, "本次运行 : 最近切换 %s\n", v)
	}
	emit(b.String())
}

// version 由发布流水线用 -ldflags "-X main.version=vX.Y.Z" 注入
var version = "dev"

func main() {
	// 常驻进程几乎不分配内存，收紧 GC 目标换来更小的常驻占用（实测 15MB -> 9MB）
	debug.SetGCPercent(30)

	status := flag.Bool("status", false, "打印当前状态后退出")
	once := flag.Bool("once", false, "执行一次校准后退出（要求没有常驻实例在跑）")
	refresh := flag.Bool("refresh", false, "刷新 Clash Verge 界面状态（重启 GUI，让它重新读盘）")
	soft := flag.Bool("soft", false, "配合 -refresh：只同步配置，不重启 GUI")
	scan := flag.Bool("scan", false, "列出 WiFi 候选与自动连接判定（只读，不改任何状态）")
	connect := flag.Bool("connect", false, "立即尝试连接目标热点（绕过冷却，供手动触发/验证）")
	setup := flag.Bool("setup", false, "打开设置向导：选择目标网络、自动连接网络与禁止代理网络")
	daemon := flag.Bool("daemon", false, "守护模式（计划任务使用；没有配置时静默退出）")
	showVersion := flag.Bool("version", false, "打印版本号后退出")
	flag.Parse()

	exists, parsed := loadConfig()

	if *showVersion {
		emit("Clash NetWatch " + version + "\n")
		return
	}
	if *setup {
		os.Exit(runSetup())
	}

	// 双击首次运行：没有配置文件就直接进向导，回答几个问题就能用
	if !exists && !*daemon && !*status && !*scan && !*connect && !*once && !*refresh {
		os.Exit(runSetup())
	}
	if !exists {
		if *daemon {
			return // 计划任务/心跳：没有配置就安静退出，绝不弹窗
		}
		emit("尚未配置：请先运行 netwatch.exe -setup（或直接双击 netwatch.exe）\n")
		os.Exit(2)
	}
	if !parsed {
		fatalConfig("config.json 解析失败", "请检查 JSON 格式，或运行 netwatch.exe -setup 重新生成配置。")
	}
	if len(cfg.TargetSSIDs) == 0 {
		fatalConfig("未配置目标网络", "请运行 netwatch.exe -setup 选择哪些网络需要开启代理。")
	}

	if *refresh {
		os.Exit(cmdRefresh(*soft))
	}
	if *scan {
		consoleEcho = true
		printScan()
		return
	}
	if *connect {
		consoleEcho = true
		msg, ok := forceAutoConnect()
		emit(msg + "\n")
		if !ok {
			os.Exit(1)
		}
		return
	}
	if *status {
		printStatus()
		return
	}
	if *once {
		// 和常驻实例抢着改注册表/TUN 会出半开状态，必须互斥
		switch acquireInstance() {
		case instAlreadyRunning:
			emit("常驻实例正在运行，无法执行 -once（状态由它维护，用 -status 查看）\n")
			os.Exit(1)
		case instFailed:
			emit("无法创建单实例互斥体，已中止\n")
			os.Exit(1)
		}
		consoleEcho = true
		lg.open()
		reconcile("单次校准")
		return
	}
	os.Exit(runDaemon())
}
