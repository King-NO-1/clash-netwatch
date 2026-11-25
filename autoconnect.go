// 自动连热点：WiFi 候选列表里出现目标网络时，主动把连接切过去。
//
// 背景：Windows 的「自动连接」只在尚未连接时按 profile 优先级挑网络；电脑已经连着
// Campus-WiFi 时，即使手机热点出现在候选里也不会自动跳过去。这里补上这一层：
// 定时读 WLAN 缓存的候选列表（不产生射频扫描、~1ms），发现目标 SSID 且带已保存的
// profile → WlanConnect 直接连过去。连接成功后完全复用既有链路：
// NetworkProfile 事件 → reconcile(matches=true) → 开系统代理 + TUN。
//
// 三个安全约束：
//  1. 只连「带 profile」的网络。Windows 不允许凭 SSID 猜密码，没连过的热点必须
//     用户手动连一次生成 profile，之后才有得自动切。
//  2. 发起连接前先上冷却，连接过程 Windows 会连发多个网络事件，绝不重复发起。
//     失败按 60s→120s→…→10min 指数退避，连上目标后立即清零。
//  3. 托盘的「暂停自动切换」同样暂停这里；暂停是用户对自动行为的完整接管。
package main

import (
	"fmt"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

var (
	pWlanGetAvailList   = wlanapi.NewProc("WlanGetAvailableNetworkList")
	pWlanGetProfileList = wlanapi.NewProc("WlanGetProfileList")
	pWlanConnect        = wlanapi.NewProc("WlanConnect")
	pWlanScan           = wlanapi.NewProc("WlanScan")
)

// WLAN_PROFILE_INFO：256 个 WCHAR 的 profile 名 + 4 字节 flags = 516 字节。
type wlanProfileInfo struct {
	ProfileName [256]uint16
	Flags       uint32
}

type wlanProfileInfoList struct {
	NumberOfItems uint32
	Index         uint32
	ProfileInfo   [1]wlanProfileInfo
}

var (
	_ [516 - unsafe.Sizeof(wlanProfileInfo{})]struct{}
	_ [8 - unsafe.Offsetof(wlanProfileInfoList{}.ProfileInfo)]struct{}
)

// savedProfiles 枚举 Windows 已保存的无线 profile。用 WLAN API 而不是 netsh：
// netsh 输出走 OEM 代码页，中文 SSID 会乱码；而且 spawn 进程要 140ms+。
func savedProfiles() []string {
	var negotiated uint32
	var handle syscall.Handle
	r, _, _ := pWlanOpenHandle.Call(2, 0, uintptr(unsafe.Pointer(&negotiated)), uintptr(unsafe.Pointer(&handle)))
	if r != 0 {
		return nil
	}
	defer pWlanCloseHandle.Call(uintptr(handle), 0)
	var ifaces *wlanInterfaceInfoList
	r, _, _ = pWlanEnumIface.Call(uintptr(handle), 0, uintptr(unsafe.Pointer(&ifaces)))
	if r != 0 || ifaces == nil {
		return nil
	}
	defer pWlanFreeMemory.Call(uintptr(unsafe.Pointer(ifaces)))
	seen := map[string]bool{}
	var out []string
	stride := unsafe.Sizeof(wlanInterfaceInfo{})
	base := unsafe.Pointer(&ifaces.InterfaceInfo[0])
	for i := 0; i < int(ifaces.NumberOfItems); i++ {
		info := (*wlanInterfaceInfo)(unsafe.Add(base, uintptr(i)*stride))
		var list *wlanProfileInfoList
		r, _, _ = pWlanGetProfileList.Call(uintptr(handle),
			uintptr(unsafe.Pointer(&info.InterfaceGUID)), 0, uintptr(unsafe.Pointer(&list)))
		if r != 0 || list == nil {
			continue
		}
		n := int(list.NumberOfItems)
		pstride := unsafe.Sizeof(wlanProfileInfo{})
		pbase := unsafe.Pointer(&list.ProfileInfo[0])
		for j := 0; j < n; j++ {
			pi := (*wlanProfileInfo)(unsafe.Add(pbase, uintptr(j)*pstride))
			name := syscall.UTF16ToString(pi.ProfileName[:])
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, name)
		}
		pWlanFreeMemory.Call(uintptr(unsafe.Pointer(list)))
	}
	return out
}

// WLAN_AVAILABLE_NETWORK（Win32 布局：512 字节 profile 名 + 36 字节 DOT11_SSID +
// 20 个 DWORD = 628 字节）。这些结构体必须和 wlanapi.h 二进制兼容，用编译期断言锁死。
type dot11SSID struct {
	Length uint32
	SSID   [32]byte
}

type wlanAvailableNetwork struct {
	ProfileName            [256]uint16
	Ssid                   dot11SSID
	BssType                uint32
	NumberOfBssids         uint32
	NetworkConnectable     uint32
	NotConnectableReason   uint32
	NumberOfPhyTypes       uint32
	PhyTypes               [8]uint32
	MorePhyTypes           uint32
	SignalQuality          uint32
	SecurityEnabled        uint32
	DefaultAuthAlgorithm   uint32
	DefaultCipherAlgorithm uint32
	Flags                  uint32
	Reserved               uint32
}

type wlanAvailableNetworkList struct {
	NumberOfItems uint32
	Index         uint32
	Network       [1]wlanAvailableNetwork
}

type wlanConnectionParameters struct {
	Mode             uint32
	Profile          *uint16
	SSID             *dot11SSID
	DesiredBSSIDList uintptr
	BssType          uint32
	Flags            uint32
}

var (
	_ [628 - unsafe.Sizeof(wlanAvailableNetwork{})]struct{}
	_ [512 - unsafe.Offsetof(wlanAvailableNetwork{}.Ssid)]struct{}
	_ [556 - unsafe.Offsetof(wlanAvailableNetwork{}.NetworkConnectable)]struct{}
	_ [604 - unsafe.Offsetof(wlanAvailableNetwork{}.SignalQuality)]struct{}
	_ [620 - unsafe.Offsetof(wlanAvailableNetwork{}.Flags)]struct{}
	_ [8 - unsafe.Offsetof(wlanAvailableNetworkList{}.Network)]struct{}

	// x64 下指针 8 字节，WLAN_CONNECTION_PARAMETERS 共 40 字节
	_ [8 - unsafe.Offsetof(wlanConnectionParameters{}.Profile)]struct{}
	_ [16 - unsafe.Offsetof(wlanConnectionParameters{}.SSID)]struct{}
	_ [24 - unsafe.Offsetof(wlanConnectionParameters{}.DesiredBSSIDList)]struct{}
	_ [32 - unsafe.Offsetof(wlanConnectionParameters{}.BssType)]struct{}
	_ [40 - unsafe.Sizeof(wlanConnectionParameters{})]struct{}
)

const (
	wlanConnModeProfile        = 0 // wlan_connection_mode_profile
	dot11BssTypeInfrastructure = 1 // dot11_BSS_type_infrastructure
	ifaceStateNotReady         = 0
	ifaceStateDisconnecting    = 3

	autoVerifyWindow  = 12 * time.Second // WlanConnect 后的验证窗口：到期没连上算失败
	autoSettleDelay   = 3 * time.Second  // 连上目标后观察多久才确认稳定、清退避
	autoScanKickGap   = 20 * time.Second // 主动触发 WlanScan 的最小间隔
	autoBackoffMaxDur = 10 * time.Minute
)

// 自动连接状态。maybeAutoConnect 会被 worker、tray 消息循环、命令行分别调用，全部加锁。
//
// autoCallMu 串行化"扫描候选 + 发起连接"这段含 I/O 的流程；autoMu 只保护下面的状态字段，
// 绝不跨 I/O 持有——否则托盘点「立即连接」时，reconcile 在关代理前要先等这把锁。
var (
	autoCallMu      sync.Mutex
	autoMu          sync.Mutex
	autoNext        time.Time // 验证窗口 / 失败退避的截止时刻
	autoFailures    int       // 未确认的连接次数，稳定连上目标后清零
	autoLastScan    time.Time // 上次主动 WlanScan 的时刻
	autoPending     bool      // 已发起 WlanConnect，等待验证
	autoAttemptAt   time.Time // 最近一次发起连接的时刻
	autoPendingSSID string    // 最近一次发起连接的 SSID，失败日志用
	autoSeenAt      time.Time // 首次读到目标 SSID 的时刻（稳定观察起点）
	autoLastReason  string    // 最近一次判定结论，供 -status / -scan 展示
)

// setAutoReason 是 autoLastReason 唯一的非持锁写入出口。
func setAutoReason(s string) {
	autoMu.Lock()
	autoLastReason = s
	autoMu.Unlock()
}

func autoScanInterval() time.Duration {
	s := cfg.AutoConnectSecs
	if s < 5 {
		s = 15
	}
	return time.Duration(s) * time.Second
}

func autoBackoff(failures int) time.Duration {
	if failures < 1 {
		failures = 1
	}
	d := 60 * time.Second << (failures - 1)
	if d <= 0 || d > autoBackoffMaxDur {
		d = autoBackoffMaxDur
	}
	return d
}

// noteTargetSeen 在读到目标 SSID 时调用（每次 reconcile 都调）。
// WlanConnect 是异步的：返回 0 只代表"请求发出去"，不代表连上了。之前把返回成功
// 当成连上、20s 静默后又发一次，两网抖动时连发 8 次。现在必须由"确实连着目标"
// 稳定 autoSettleDelay 才算成功，才能清掉失败退避。
func noteTargetSeen() {
	autoMu.Lock()
	defer autoMu.Unlock()
	now := time.Now()
	if autoSeenAt.IsZero() {
		autoSeenAt = now
		return
	}
	if now.Sub(autoSeenAt) < autoSettleDelay {
		return // 还在观察期，可能是关联中的暂态
	}
	autoFailures = 0
	autoNext = time.Time{}
	autoPending = false
	autoAttemptAt = time.Time{}
	autoLastReason = "已稳定连接目标热点"
}

// noteTargetGone 在读到非目标 SSID 时调用：打断稳定观察，重新计时。
func noteTargetGone() {
	autoMu.Lock()
	autoSeenAt = time.Time{}
	autoMu.Unlock()
}

// confirmConnectResult 在验证窗口到期还未稳定连上目标时调用，把这次尝试记为失败，
// 并按 60s→120s→…→10min 退避。返回是否刚刚记了一次失败。
func confirmConnectResult() bool {
	autoMu.Lock()
	defer autoMu.Unlock()
	if !autoPending || time.Now().Before(autoNext) {
		return false
	}
	autoPending = false // WlanConnect 返回成功但实际没连上 → 失败
	autoFailures++
	d := autoBackoff(autoFailures)
	autoNext = time.Now().Add(d)
	autoLastReason = fmt.Sprintf("连接未成功，第 %d 次，%s 后重试", autoFailures, d)
	lg.logf("WARN", "自动连接 %q 未连上，%s 后重试（第 %d 次）", autoPendingSSID, d, autoFailures)
	return true
}

// ---------------------------------------------------------------- 候选扫描

type scanItem struct {
	iface       guid
	ssid        string
	ssidBytes   []byte
	quality     uint32
	connectable bool
	profile     string
	isTarget    bool
	isAuto      bool // 目标网络且允许自动连接（auto_connect_ssids 过滤后）
	usable      bool // 网卡不是 not_ready / disconnecting，可以接受新连接
}

// scanAll 只读枚举所有无线网卡的可用网络。WlanGetAvailableNetworkList 返回的是
// Windows 自己维护的扫描缓存，不主动发射探测请求；改网络的事只发生在 WlanConnect。
func scanAll() ([]scanItem, error) {
	var negotiated uint32
	var handle syscall.Handle
	r, _, _ := pWlanOpenHandle.Call(2, 0, uintptr(unsafe.Pointer(&negotiated)), uintptr(unsafe.Pointer(&handle)))
	if r != 0 {
		return nil, fmt.Errorf("WlanOpenHandle 错误码 %d", r)
	}
	defer pWlanCloseHandle.Call(uintptr(handle), 0)

	var list *wlanInterfaceInfoList
	r, _, _ = pWlanEnumIface.Call(uintptr(handle), 0, uintptr(unsafe.Pointer(&list)))
	if r != 0 || list == nil {
		return nil, fmt.Errorf("WlanEnumInterfaces 错误码 %d", r)
	}
	defer pWlanFreeMemory.Call(uintptr(unsafe.Pointer(list)))
	if list.NumberOfItems == 0 {
		return nil, nil // 没有无线网卡（台式机/有线），自动连接不适用
	}

	var out []scanItem
	count := int(list.NumberOfItems)
	stride := unsafe.Sizeof(wlanInterfaceInfo{})
	base := unsafe.Pointer(&list.InterfaceInfo[0])
	for i := 0; i < count; i++ {
		info := (*wlanInterfaceInfo)(unsafe.Add(base, uintptr(i)*stride))
		// 只在"网卡还能接受新连接"时动手：not_ready / 正在断开 时 WlanConnect 会失败。
		// 关联中、认证中也要允许——那正是"刚连上校园网、想把热点抢回来"的时刻。
		usable := info.State != ifaceStateNotReady && info.State != ifaceStateDisconnecting

		var avail *wlanAvailableNetworkList
		r, _, _ = pWlanGetAvailList.Call(uintptr(handle), uintptr(unsafe.Pointer(&info.InterfaceGUID)),
			0, 0, uintptr(unsafe.Pointer(&avail)))
		if r != 0 || avail == nil {
			lg.debug("WlanGetAvailableNetworkList 失败：%d（网卡 %d）", r, i)
			continue
		}
		n := int(avail.NumberOfItems)
		astride := unsafe.Sizeof(wlanAvailableNetwork{})
		abase := unsafe.Pointer(&avail.Network[0])
		for j := 0; j < n; j++ {
			net := (*wlanAvailableNetwork)(unsafe.Add(abase, uintptr(j)*astride))
			l := net.Ssid.Length
			if l == 0 || l > 32 {
				continue // 隐藏网络 / 驱动返回异常长度
			}
			raw := append([]byte(nil), net.Ssid.SSID[:l]...)
			s := string(raw)
			out = append(out, scanItem{
				iface:       info.InterfaceGUID,
				ssid:        s,
				ssidBytes:   raw,
				quality:     net.SignalQuality,
				connectable: net.NetworkConnectable != 0,
				profile:     syscall.UTF16ToString(net.ProfileName[:]),
				// 不再看 WLAN_AVAILABLE_NETWORK_CONNECTED 标志：刚切走的一两秒内它还是旧值，
				// 会把刚断开的目标网络当成"已连接"而跳过，害得切换要等下一轮扫描。
				// "已经在目标上就别连"由 maybeAutoConnect 用 currentSSID 判断，更准。
				isTarget: matches(s),
				isAuto:   autoConnectTarget(s),
				usable:   usable,
			})
		}
		pWlanFreeMemory.Call(uintptr(unsafe.Pointer(avail)))
	}
	return out, nil
}

type autoCandidate struct {
	iface   guid
	profile string
	ssid    string
	bytes   []byte
	quality uint32
}

// bestCandidate 返回信号最强的可连目标；noProfile 表示「看见了目标但没有已保存配置」。
// autoOnly=true 时只考虑允许自动连接的目标（auto_connect_ssids 过滤之后）。
func bestCandidate(autoOnly bool) (*autoCandidate, bool, error) {
	items, err := scanAll()
	if err != nil {
		return nil, false, err
	}
	var best *autoCandidate
	noProfile := false
	minSig := uint32(cfg.AutoConnectMinSig)
	for _, it := range items {
		wanted := it.isTarget
		if autoOnly {
			wanted = it.isAuto
		}
		if !wanted || !it.usable || !it.connectable {
			continue
		}
		if it.profile == "" {
			noProfile = true // 没手动连过 → Windows 不给连，提示用户
			continue
		}
		if it.quality < minSig {
			continue
		}
		if best == nil || it.quality > best.quality {
			best = &autoCandidate{
				iface: it.iface, profile: it.profile, ssid: it.ssid,
				bytes: it.ssidBytes, quality: it.quality,
			}
		}
	}
	// 同一 SSID 往往同时出现「带 profile」和「不带 profile」两条候选（不同 BSS），
	// 只要有一条能连就不该报"缺配置"。
	if best != nil {
		noProfile = false
	}
	return best, noProfile, nil
}

// wlanKickScan 让网卡重新扫一遍周围 AP（异步、立即返回）。找不到目标时限频调用，
// 下一轮候选轮询就能看到新开的热点，又不至于每 15 秒发射一次探测。
func wlanKickScan() {
	var negotiated uint32
	var handle syscall.Handle
	r, _, _ := pWlanOpenHandle.Call(2, 0, uintptr(unsafe.Pointer(&negotiated)), uintptr(unsafe.Pointer(&handle)))
	if r != 0 {
		return
	}
	defer pWlanCloseHandle.Call(uintptr(handle), 0)
	var list *wlanInterfaceInfoList
	r, _, _ = pWlanEnumIface.Call(uintptr(handle), 0, uintptr(unsafe.Pointer(&list)))
	if r != 0 || list == nil {
		return
	}
	defer pWlanFreeMemory.Call(uintptr(unsafe.Pointer(list)))
	stride := unsafe.Sizeof(wlanInterfaceInfo{})
	base := unsafe.Pointer(&list.InterfaceInfo[0])
	for i := 0; i < int(list.NumberOfItems); i++ {
		info := (*wlanInterfaceInfo)(unsafe.Add(base, uintptr(i)*stride))
		pWlanScan.Call(uintptr(handle), uintptr(unsafe.Pointer(&info.InterfaceGUID)), 0, 0, 0)
	}
}

// wlanConnectTo 用已保存的 profile 连接指定 SSID。WlanConnect 立即返回，
// 真正的认证/DHCP 由 Windows 在后台完成，结果通过既有网络事件回到 reconcile。
func wlanConnectTo(c *autoCandidate) error {
	prof, err := syscall.UTF16PtrFromString(c.profile)
	if err != nil {
		return err
	}
	ss := &dot11SSID{Length: uint32(len(c.bytes))}
	copy(ss.SSID[:], c.bytes)
	cp := wlanConnectionParameters{
		Mode:    wlanConnModeProfile,
		Profile: prof,
		SSID:    ss,
		BssType: dot11BssTypeInfrastructure,
	}
	var negotiated uint32
	var handle syscall.Handle
	r, _, _ := pWlanOpenHandle.Call(2, 0, uintptr(unsafe.Pointer(&negotiated)), uintptr(unsafe.Pointer(&handle)))
	if r != 0 {
		return fmt.Errorf("WlanOpenHandle 错误码 %d", r)
	}
	defer pWlanCloseHandle.Call(uintptr(handle), 0)
	r, _, _ = pWlanConnect.Call(uintptr(handle), uintptr(unsafe.Pointer(&c.iface)),
		uintptr(unsafe.Pointer(&cp)), 0)
	if r != 0 {
		return fmt.Errorf("WlanConnect 错误码 %d", r)
	}
	return nil
}

// ---------------------------------------------------------------- 决策入口

// maybeAutoConnect 在「当前不在目标网络」时检查候选并发起连接。
// 返回值只用于诊断，调用方（reconcile）已经先把代理/TUN 按当前 SSID 归位了。
func maybeAutoConnect(reason string) bool {
	if !cfg.AutoConnect || autoPaused() {
		return false
	}
	started, alert := autoConnectLocked(reason)
	if alert != "" {
		// 通知放到锁外：Shell_NotifyIcon 会与托盘线程交互，
		// 不能在自己持锁时等托盘线程（那边也可能在等这把锁）。
		trayAlert("NetWatch", alert)
	}
	return started
}

// autoConnectLocked 的调用方必须不持有 autoMu / autoCallMu；返回值 alert 由外层负责弹通知。
func autoConnectLocked(reason string) (started bool, alert string) {
	// 扫描与 WlanConnect 都含 I/O，用 autoCallMu 串行化，且不把 autoMu 带进这段。
	autoCallMu.Lock()
	defer autoCallMu.Unlock()

	// 上次发起的连接还在验证窗口内：别重发。窗口到期仍未连上 → 记一次失败并退避。
	if confirmConnectResult() {
		return false, ""
	}
	autoMu.Lock()
	if now := time.Now(); now.Before(autoNext) {
		autoLastReason = fmt.Sprintf("冷却中，%ds 后可再试", int(time.Until(autoNext).Seconds())+1)
		autoMu.Unlock()
		return false, ""
	}
	autoMu.Unlock()
	if ssid, ok := currentSSID(); ok && matches(ssid) {
		return false, "" // 已在目标，noteTargetSeen 会负责稳定判定与清退避
	}
	c, noProfile, err := bestCandidate(true)
	if err != nil {
		msg := "读取候选失败：" + err.Error()
		setAutoReason(msg)
		lg.debug("自动连接（%s）：%s", reason, msg)
		return false, ""
	}
	if c == nil {
		var msg string
		if noProfile {
			msg = "候选里有目标 SSID，但没有保存的连接配置（需手动连一次）"
		} else {
			// 可能是压根没看见，也可能看见了但不可连/信号低于阈值，措辞不要一口咬定
			msg = "候选里没有可连的目标热点（未出现、不可连或信号低于阈值）"
			autoMu.Lock()
			due := time.Since(autoLastScan) >= autoScanKickGap
			if due {
				autoLastScan = time.Now()
			}
			autoMu.Unlock()
			if due {
				wlanKickScan()
			}
		}
		setAutoReason(msg)
		lg.debug("自动连接（%s）：%s", reason, msg)
		return false, ""
	}

	// 先上验证窗口再发起：WlanConnect 返回 0 只代表请求发出去，绝不能当成连上了。
	// 窗口内不会重发；到期由 noteTargetSeen（真的连着目标）或 confirmConnectResult（没连上）结算。
	now := time.Now()
	autoMu.Lock()
	autoNext = now.Add(autoVerifyWindow)
	autoPending = true
	autoPendingSSID = c.ssid
	autoAttemptAt = now
	autoMu.Unlock()
	if err := wlanConnectTo(c); err != nil {
		autoMu.Lock()
		autoPending = false
		autoFailures++
		autoNext = time.Now().Add(autoBackoff(autoFailures))
		autoLastReason = fmt.Sprintf("连接失败（第 %d 次）：%v", autoFailures, err)
		autoMu.Unlock()
		lg.logf("WARN", "自动连接 %q 失败：%v", c.ssid, err)
		return false, "自动连接热点失败，稍后自动重试"
	}
	msg := fmt.Sprintf("已发起连接：%q（信号 %d%%，profile %q），等待确认", c.ssid, c.quality, c.profile)
	setAutoReason(msg)
	lg.logf("INFO", "检测到目标热点，自动连接（%s）：%s", reason, msg)
	// 文案用实际候选 SSID，多目标时不会张冠李戴
	return true, fmt.Sprintf("检测到 %s，正在自动切换…", c.ssid)
}

// forceAutoConnect 供托盘菜单 / -connect 手动触发：绕过冷却与暂停，
// 但依然遵守「没 profile 不连」「已在目标不重复连」两条底线。
func forceAutoConnect() (string, bool) {
	// 串行化"扫描 + 连接"，但只在这一段持有 autoCallMu；
	// 状态字段用 autoMu 单独保护，reconcile 的关代理路径永远不会被这里的 I/O 卡住。
	autoCallMu.Lock()
	defer autoCallMu.Unlock()

	if ssid, ok := currentSSID(); ok && matches(ssid) {
		// 已经在目标网络：对"期望状态"来说这就是成功，退出码不该是失败
		return "当前已在目标网络，无需切换", true
	}
	c, noProfile, err := bestCandidate(false)
	if err != nil {
		return "读取 WiFi 候选失败：" + err.Error(), false
	}
	if c == nil {
		if noProfile {
			return "候选里有目标热点，但没保存过连接配置——请先在 WiFi 列表手动连接一次", false
		}
		return "候选列表里没有目标热点（先打开手机热点再试）", false
	}
	now := time.Now()
	autoMu.Lock()
	autoNext = now.Add(autoVerifyWindow)
	autoPending = true
	autoPendingSSID = c.ssid
	autoAttemptAt = now
	autoMu.Unlock()
	if err := wlanConnectTo(c); err != nil {
		autoMu.Lock()
		autoPending = false
		autoFailures++
		autoNext = time.Now().Add(autoBackoff(autoFailures))
		autoMu.Unlock()
		lg.logf("WARN", "手动连接 %q 失败：%v", c.ssid, err)
		return fmt.Sprintf("连接失败：%v", err), false
	}
	lg.logf("INFO", "手动触发自动连接：%q（信号 %d%%，profile %q）", c.ssid, c.quality, c.profile)
	return fmt.Sprintf("正在连接 %q，连上后会自动开代理与 TUN", c.ssid), true
}

// ---------------------------------------------------------------- 展示 / 诊断

func autoConnectStatusLine() string {
	if !cfg.AutoConnect {
		return "关闭（config.json auto_connect=false）"
	}
	s := fmt.Sprintf("开启（轮询 %ds，信号阈值 %d%%）", cfg.AutoConnectSecs, cfg.AutoConnectMinSig)
	if autoPaused() {
		s += "；已暂停（含连热点）"
	}
	autoMu.Lock()
	if !autoNext.IsZero() {
		if d := time.Until(autoNext); d > 0 {
			s += fmt.Sprintf("；冷却 %ds", int(d.Seconds())+1)
		}
	}
	if autoLastReason != "" {
		s += "；" + autoLastReason
	}
	autoMu.Unlock()
	return s
}

// printScan 是 -scan 的只读诊断：列出全部候选，说明此刻自动连接会怎么做，
// 不发起任何连接、不改注册表/内核，用于验证 profile 与模糊匹配是否正确。
func printScan() {
	var b strings.Builder
	if ssid, ok := currentSSID(); ok {
		fmt.Fprintf(&b, "当前网络 : %q\n", ssid)
	} else {
		fmt.Fprintf(&b, "当前网络 : <读取失败>\n")
	}
	fmt.Fprintf(&b, "目标网络 : %s（模糊匹配 %t）\n", targetDisplay(), cfg.FuzzyMatch)
	if len(cfg.NeverProxySSIDs) > 0 {
		fmt.Fprintf(&b, "禁止代理 : %s\n", strings.Join(cfg.NeverProxySSIDs, ", "))
	}
	fmt.Fprintf(&b, "自动连接 : %s\n\n", autoConnectStatusLine())
	items, err := scanAll()
	if err != nil {
		fmt.Fprintf(&b, "扫描失败 : %v\n", err)
		emit(b.String())
		return
	}
	fmt.Fprintf(&b, "WiFi 候选 %d 个：\n", len(items))
	for _, it := range items {
		mark := ""
		if it.isTarget {
			mark = "   << 目标"
			if !it.isAuto {
				mark += "（不自动连）"
			}
		}
		if isNeverProxy(it.ssid) {
			mark += "   << 禁止代理"
		}
		fmt.Fprintf(&b, "  %-30q  信号 %3d%%  可连 %-5v  profile %q%s\n",
			it.ssid, it.quality, it.connectable, it.profile, mark)
	}
	c, noProfile, err := bestCandidate(true)
	fmt.Fprintf(&b, "\n本轮判定 : ")
	cur, curOK := currentSSID()
	switch {
	case curOK && matches(cur):
		fmt.Fprintf(&b, "已在目标网络，无需连接\n")
	case err != nil:
		fmt.Fprintf(&b, "扫描失败 %v\n", err)
	case c != nil:
		fmt.Fprintf(&b, "会发起连接 → %q（profile %q，信号 %d%%）\n", c.ssid, c.profile, c.quality)
	case noProfile:
		fmt.Fprintf(&b, "看见目标但没有 profile，需手动连一次后才能自动切\n")
	default:
		fmt.Fprintf(&b, "候选里没有可连的目标热点，不动作\n")
	}
	if autoPaused() {
		fmt.Fprintf(&b, "注意     : 自动切换处于暂停状态，守护进程不会发起连接\n")
	}
	emit(b.String())
}
