// Clash NetWatch —— 首次运行 / 重新配置向导。
//
// 定位：一个陌生人下载 netwatch.exe 后双击，不读文档也能用起来。
// 流程：扫描本机 Wi-Fi（当前连接 + 周围候选 + 已保存的配置文件）→
// 选择「哪些网络开代理」「其中哪些自动连接」「哪些网络绝不能有代理」→
// 写 config.json → 可选注册开机自启 → 直接进入守护。
//
// 兼容三种启动形态：终端里 -setup（沿用当前控制台）、双击/托盘（AllocConsole
// 新开一个控制台窗口）、CI 管道喂 stdin（cinOK=false 时所有确认默认取推荐值，
// 只有「目标网络」这一必答项用默认）。
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

var (
	pAllocConsole = kernel32.NewProc("AllocConsole")
	pMessageBoxW  = user32.NewProc("MessageBoxW")
)

const mbOK = 0x0

const (
	// 手动输入模式里，序号 0 表示"手动输入名字"
	inputMagic = "0"
)

type suEnv struct {
	out        *os.File // 向导自己的 stdout
	cin        *os.File
	br         *bufio.Reader
	cinOK      bool
	ownConsole bool     // 双击/托盘启动：我们自己 AllocConsole 出来的窗口
	sel        []string // 候选 SSID（按保存状态与信号排序）
}

// runSetup 是 -setup / 首次双击 / 托盘「网络设置」的入口，返回进程退出码。
func runSetup() int {
	e := &suEnv{}
	if !e.ensureConsole() {
		fatalDialog("NetWatch 无法打开控制台",
			"请打开命令提示符（cmd）或 PowerShell，执行：\n\nnetwatch.exe -setup")
		return 1
	}
	e.welcome()
	if code := e.runFlow(); code != 0 {
		return code
	}
	return e.finish()
}

// ensureConsole 三种形态各有归宿：终端里 -setup 沿用当前控制台；stdout 被重定向成
// 管道/文件（CI 或 shell 脚本）直接写管道；真正的 windowsgui 双击（标准句柄为 NULL）
// 才 AllocConsole 新开一个控制台窗口，并同步打开 CONIN$ 做输入。
func (e *suEnv) ensureConsole() bool {
	if stdoutUsable() {
		e.out = os.Stdout
	} else {
		if r, _, _ := pAllocConsole.Call(); r == 0 {
			return false
		}
		c, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0)
		if err != nil {
			return false
		}
		e.out = c
		e.ownConsole = true
	}
	if stdinUsable() {
		e.cin = os.Stdin
	} else if e.ownConsole {
		c, err := os.OpenFile("CONIN$", os.O_RDONLY, 0)
		if err != nil {
			return false
		}
		e.cin = c
	}
	if e.cin != nil {
		e.br = bufio.NewReader(e.cin)
		e.cinOK = true
	}
	return true
}

// stdoutUsable 区分「重定向到管道/文件」（可用）与「windowsgui 双击」（句柄无效）。
func stdoutUsable() bool {
	if os.Stdout == nil || os.Stdout.Fd() == 0 {
		return false
	}
	_, err := os.Stdout.Stat()
	return err == nil
}

func stdinUsable() bool {
	return os.Stdin != nil && os.Stdin.Fd() != 0
}

// printf 统一走 writeOut：真控制台用 WriteConsoleW（cp936 控制台下中文不乱码），
// 管道/文件则直接写 UTF-8 字节。
func (e *suEnv) printf(format string, args ...any) {
	writeOut(syscall.Handle(e.out.Fd()), fmt.Sprintf(format, args...))
}

// ask 读取一整行（保留 SSID 里的空格，Fscanln 会截断）。非交互时返回默认值。
func (e *suEnv) ask(prompt, def string) string {
	if prompt != "" {
		e.printf("%s ", prompt)
	}
	if !e.cinOK {
		e.printf("[%s]（非交互，取默认）\n", def)
		return def
	}
	line, err := e.br.ReadString('\n')
	line = strings.TrimRight(line, "\r\n")
	if err != nil && line == "" {
		return def
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

func (e *suEnv) confirm(prompt string, defYes bool) bool {
	def := "y"
	hint := "Y/n"
	if !defYes {
		def = "n"
		hint = "y/N"
	}
	ans := strings.ToLower(e.ask(fmt.Sprintf("%s (%s)", prompt, hint), def))
	return ans == "y" || ans == "yes" || (defYes && ans == "")
}

// fatalDialog 向导是 windowsgui 程序，没有控制台时唯一可靠的报错通道是 MessageBox。
func fatalDialog(title, text string) {
	t, _ := syscall.UTF16PtrFromString(title)
	m, _ := syscall.UTF16PtrFromString(text)
	pMessageBoxW.Call(0, uintptr(unsafe.Pointer(t)), uintptr(unsafe.Pointer(m)), mbOK)
}

func (e *suEnv) welcome() {
	e.printf("\n")
	e.printf("  ╭──────────────────────────────────────────╮\n")
	e.printf("  │   Clash NetWatch 网络设置向导             │\n")
	e.printf("  │   按当前 Wi-Fi 自动开关系统代理与 TUN     │\n")
	e.printf("  ╰──────────────────────────────────────────╯\n")
	e.printf("\n")
}

// runFlow 收集配置。返回 0=继续，1=用户取消，2=致命错误。
func (e *suEnv) runFlow() int {
	e.scanNetworks()

	e.printf("第一步：目标网络（连上它们时开启系统代理 + TUN）\n\n")
	targets := e.pickNetworks(true)
	if len(targets) == 0 {
		e.printf("\n没有选择任何网络，已取消。\n")
		return 1
	}

	e.printf("\n第二步：自动连接（电脑在别的网络时，候选里出现目标网络就自动切过去）\n")
	e.printf("     前提：这些网络必须手动连过至少一次（Windows 才有保存的密码）。\n\n")
	autoOn := e.confirm("启用自动连接？", cfg.AutoConnect)
	var autos []string
	if autoOn {
		autos = e.pickAuto(targets)
		if len(autos) == 0 {
			autoOn = false
			e.printf("（未选择自动连接网络，已关闭自动连接）\n")
		}
	}

	e.printf("\n第三步：禁止代理网络（连上它们时必须立刻关闭代理，优先级最高）\n")
	e.printf("     典型场景：校园网、公司内网等不允许走代理的网络。\n\n")
	nevers := e.pickNetworks(false)

	fuzzy := e.confirm("\n模糊匹配？（开启时，\"MyPhone\" 与 \"MyPhone-5G\" 视为同一网络）", true)

	// 交叉校验：同一个网络既开代理又禁止代理，禁止优先，直接从目标里剔除并告知
	var conflicts []string
	kept := targets[:0]
	for _, t := range targets {
		bad := false
		for _, n := range nevers {
			if matchOne(t, n) {
				bad = true
				conflicts = append(conflicts, t)
				break
			}
		}
		if !bad {
			kept = append(kept, t)
		}
	}
	targets = kept
	if len(conflicts) > 0 {
		e.printf("\n注意：%s 同时被选为「目标网络」和「禁止代理」，已从目标里移除（禁止优先）。\n",
			strings.Join(conflicts, ", "))
	}
	if len(targets) == 0 {
		e.printf("目标网络被禁止名单全部覆盖，无法继续。\n")
		return 1
	}
	autos = filterList(autos, targets)

	// 写配置（以默认值为基底，保留用户没改的运行参数）
	c := cfg
	c.TargetSSID = ""
	c.TargetSSIDs = targets
	c.AutoConnectSSIDs = autos
	if c.AutoConnectSSIDs == nil {
		c.AutoConnectSSIDs = []string{} // 序列化成 [] 而不是 null
	}
	c.NeverProxySSIDs = nevers
	if c.NeverProxySSIDs == nil {
		c.NeverProxySSIDs = []string{}
	}
	c.FuzzyMatch = fuzzy
	c.AutoConnect = autoOn && len(autos) > 0
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		fatalDialog("NetWatch 配置失败", "生成 config.json 失败："+err.Error())
		return 2
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(configPath(), raw, 0644); err != nil {
		fatalDialog("NetWatch 配置失败", "写入 config.json 失败："+err.Error())
		return 2
	}

	_, parsed := loadConfig()
	if !parsed || len(cfg.TargetSSIDs) == 0 {
		fatalDialog("NetWatch 配置校验失败", "刚写入的 config.json 校验未通过，请截图反馈给项目。")
		return 2
	}

	e.printf("\n已保存：%s\n", configPath())
	e.printSummary()
	return 0
}

// finish 收尾：注册自启（可选）→ 重启守护进程（若之前在跑）。
func (e *suEnv) finish() int {
	if e.confirm("\n注册开机自启与保活（计划任务）？", true) {
		if err := installTask(); err != nil {
			e.printf("自启注册失败：%v（请确认是从完整 zip 解压的，内含 install-task.ps1）\n", err)
		} else {
			e.printf("已注册计划任务 ClashNetWatch（登录 / 联网 / 唤醒 / 每分钟心跳）。\n")
		}
	}

	// 旧守护进程持有单实例互斥体，且进程内的配置不会热更新：先停、等它真正退出，
	// 再拉起新进程。TerminateProcess 返回不代表进程已消失，抢跑会被互斥体挡掉后
	// 静默退出（用户以为成功，实际还是旧配置甚至没有守护）。
	stopped := 0
	for _, pid := range ourProcesses() {
		if err := terminate(pid); err == nil {
			stopped++
		}
	}
	if stopped > 0 {
		e.printf("已停止旧守护进程（%d 个），等待其释放单实例锁…\n", stopped)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) && len(ourProcesses()) > 0 {
			time.Sleep(150 * time.Millisecond)
		}
	}
	// 拉起新进程并确认存活。互斥体释放可能有几毫秒延迟，所以失败就重试几次，
	// 而不是像以前一样打一句"已重启"了事。
	alive := false
	var startErr error
	for attempt := 0; attempt < 3 && !alive; attempt++ {
		if err := startProcessArgs(exeAbs(), "-daemon"); err != nil {
			startErr = err
			break
		}
		time.Sleep(1500 * time.Millisecond)
		alive = len(ourProcesses()) > 0
		if !alive {
			time.Sleep(700 * time.Millisecond)
		}
	}
	switch {
	case startErr != nil:
		e.printf("启动守护进程失败：%v（可重新双击 netwatch.exe）\n", startErr)
	case !alive:
		e.printf("守护进程未能保持运行，请查看 netwatch.log；计划任务若已注册会在下次心跳拉起。\n")
	default:
		e.printf("守护进程已按新配置启动（托盘可见 NetWatch 图标）。\n")
	}

	e.printf("\n全部完成。连上目标网络试试，几秒内会有「已开启代理与 TUN」通知。\n")
	e.printf("状态随时查：netwatch.exe -status　|　重新配置：netwatch.exe -setup\n\n")
	e.pauseIfInteractive()
	return 0
}

// ourProcesses 只认与本 exe 同路径的 netwatch 进程，避免误杀别处同名程序；
// 同时排除向导自己（它也叫 netwatch.exe，但还没拿互斥体）。
func ourProcesses() []uint32 {
	self := uint32(os.Getpid())
	want := strings.ToLower(exeAbs())
	snap, _, _ := pSnapshot.Call(th32csSnapProcess, 0)
	if snap == 0 || snap == ^uintptr(0) {
		return nil
	}
	defer syscall.CloseHandle(syscall.Handle(snap))
	var pe procEntry
	pe.Size = uint32(unsafe.Sizeof(pe))
	r, _, _ := pProc32First.Call(snap, uintptr(unsafe.Pointer(&pe)))
	var out []uint32
	for r != 0 {
		if pe.ProcessID != self {
			if p := imagePath(pe.ProcessID); p != "" && strings.EqualFold(p, want) {
				out = append(out, pe.ProcessID)
			}
		}
		r, _, _ = pProc32Next.Call(snap, uintptr(unsafe.Pointer(&pe)))
	}
	return out
}

func filterList(in, allow []string) []string {
	var out []string
	for _, s := range in {
		if containsAny(allow, s) {
			out = append(out, s)
		}
	}
	return out
}

func exeAbs() string {
	p, _ := filepath.Abs(os.Args[0])
	return p
}

// pauseIfInteractive 双击/托盘形态下，控制台窗口是我们自己开的，读完就关用户来不及看：
// 等一次回车。从终端 -setup 启动时不暂停。
func (e *suEnv) pauseIfInteractive() {
	if e.ownConsole && e.cinOK {
		e.ask("\n按回车键关闭窗口…", "")
	}
}

// ---------------------------------------------------------------- 网络选择

// scanNetworks 汇总「当前连接 + 周围候选 + 已保存 profile」，去重排序后展示。
func (e *suEnv) scanNetworks() {
	e.printf("正在扫描本机 Wi-Fi 网络…\n\n")

	cur, _ := currentSSID()
	seen := map[string]bool{}
	if cur != "" {
		seen[cur] = true
	}

	items, err := scanAll()
	if err != nil {
		e.printf("（扫描周围网络失败：%v，仅列出当前网络与已保存的网络）\n", err)
	}
	for _, it := range items {
		if it.ssid != "" {
			seen[it.ssid] = true
		}
	}
	for _, p := range cachedProfiles() {
		if p != "" {
			seen[p] = true
		}
	}

	e.sel = make([]string, 0, len(seen))
	for s := range seen {
		e.sel = append(e.sel, s)
	}
	// 排序：当前网络最前，其次带 profile 的，最后按信号；内部按名字字典序保证稳定
	sort.Slice(e.sel, func(i, j int) bool {
		ri, rj := rankOf(e.sel[i], cur), rankOf(e.sel[j], cur)
		if ri != rj {
			return ri < rj
		}
		return e.sel[i] < e.sel[j]
	})

	e.printf("发现 %d 个网络：\n", len(e.sel))
	for i, s := range e.sel {
		mark := ""
		if cur != "" && s == cur {
			mark = "  ← 当前连接"
		} else if items != nil && signalOf(items, s) > 0 {
			mark = fmt.Sprintf("  （信号 %d%%）", signalOf(items, s))
		}
		profileNote := ""
		if !hasSavedProfile(s) {
			profileNote = "  [未保存，自动连接前需手动连一次]"
		}
		e.printf("  %2d. %-32q%s%s\n", i+1, s, mark, profileNote)
	}
	e.printf("   0. 手动输入名字（上面列表里没有）\n\n")
}

func rankOf(ssid, cur string) int {
	if ssid == cur && cur != "" {
		return 0
	}
	if hasSavedProfile(ssid) {
		return 1
	}
	return 2
}

func signalOf(items []scanItem, ssid string) uint32 {
	for _, it := range items {
		if it.ssid == ssid && it.quality > 0 {
			return it.quality
		}
	}
	return 0
}

var (
	profileOnce  sync.Once
	profileCache []string
)

// cachedProfiles 只枚举一次：排序比较器里会反复问「有没有 profile」，
// 每次都走 WLAN API 会让向导卡几秒。
func cachedProfiles() []string {
	profileOnce.Do(func() { profileCache = savedProfiles() })
	return profileCache
}

func hasSavedProfile(ssid string) bool {
	for _, p := range cachedProfiles() {
		if p == ssid {
			return true
		}
	}
	return false
}

// pickNetworks 选择一组网络：序号多选，0 进入手动输入（支持带空格的 SSID），
// 回车结束。required=true 时不允许空选择。
func (e *suEnv) pickNetworks(required bool) []string {
	prompt := "输入序号（多个用逗号或空格分隔，如 1,3）；输 0 手动输入网络名"
	if !required {
		prompt += "；回车跳过"
	}
	e.printf("%s：\n", prompt)

	chosen := map[string]bool{}
	for {
		line := e.ask("> ", "")
		if line == "" {
			if len(chosen) > 0 {
				return chosenList(chosen)
			}
			if required {
				e.printf("这一步至少选择一个网络。\n")
				continue
			}
			return nil
		}
		if line == inputMagic {
			name := e.ask("  输入网络名（SSID，可以带空格）：", "")
			if name == "" || name == inputMagic {
				continue
			}
			chosen[name] = true
			e.printf("  已加入 %q\n", name)
			continue
		}
		ok := true
		for _, tok := range splitTokens(line) {
			n, err := strconv.Atoi(tok)
			if err != nil || n < 1 || n > len(e.sel) {
				e.printf("  序号 %q 不在列表里；要手动输入名字请先输 0。\n", tok)
				ok = false
				break
			}
			chosen[e.sel[n-1]] = true
			e.printf("  已加入 %q\n", e.sel[n-1])
		}
		if ok && len(chosen) > 0 {
			return chosenList(chosen)
		}
	}
}

// pickAuto 只允许在目标网络里挑自动连接子集，避免出现"自动连一个并不开代理的网络"。
func (e *suEnv) pickAuto(targets []string) []string {
	if len(targets) == 0 {
		return nil
	}
	e.printf("候选里出现这些目标网络时，自动把 Wi-Fi 切过去：\n")
	for i, t := range targets {
		note := ""
		if !hasSavedProfile(t) {
			note = "  [未保存，需先手动连一次]"
		}
		e.printf("  %2d. %s%s\n", i+1, t, note)
	}
	e.printf("输入序号（可多选），回车 = 全部；输 0 = 不自动连接\n")
	for {
		line := e.ask("> ", "")
		if line == "" {
			return append([]string(nil), targets...)
		}
		if line == inputMagic {
			return nil
		}
		chosen := map[string]bool{}
		ok := true
		for _, tok := range splitTokens(line) {
			n, err := strconv.Atoi(tok)
			if err != nil || n < 1 || n > len(targets) {
				e.printf("  序号 %q 不在目标网络里，请重输。\n", tok)
				ok = false
				break
			}
			chosen[targets[n-1]] = true
		}
		if !ok {
			continue
		}
		var out []string
		for _, t := range targets {
			if chosen[t] {
				out = append(out, t)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
}

func chosenList(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func splitTokens(line string) []string {
	return strings.FieldsFunc(line, func(r rune) bool {
		return r == ',' || r == ' ' || r == '，' || r == '\t' || r == '、'
	})
}

func containsAny(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// fatalConfig 在无法继续运行时同时给出控制台文案和弹窗（windowsgui 双击场景看不到控制台）。
func fatalConfig(title, text string) {
	emit(title + "：" + text + "\n")
	fatalDialog(title, text)
	os.Exit(2)
}

// installTask 通过 powershell 跑 install-task.ps1（同目录）。
func installTask() error {
	ps, err := exec.LookPath("powershell.exe")
	if err != nil {
		return err
	}
	script := filepath.Join(exeDir, "install-task.ps1")
	if _, err := os.Stat(script); err != nil {
		// 从 Release 下载的用户可能只拿了 exe：脚本缺失不视为失败，任务不注册
		return fmt.Errorf("install-task.ps1 不在 exe 同目录")
	}
	cmd := exec.Command(ps, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", script)
	cmd.Dir = exeDir
	return cmd.Run()
}

func (e *suEnv) printSummary() {
	e.printf("\n配置摘要：\n")
	e.printf("  目标网络（开代理）: %s\n", targetDisplay())
	if len(cfg.AutoConnectSSIDs) > 0 {
		e.printf("  自动连接          : %s\n", strings.Join(cfg.AutoConnectSSIDs, ", "))
	} else {
		e.printf("  自动连接          : 关闭\n")
	}
	if len(cfg.NeverProxySSIDs) > 0 {
		e.printf("  禁止代理          : %s\n", strings.Join(cfg.NeverProxySSIDs, ", "))
	} else {
		e.printf("  禁止代理          : （无，建议至少加上校园网/公司网）\n")
	}
	e.printf("  模糊匹配          : %t\n", cfg.FuzzyMatch)
}
