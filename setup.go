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
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
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
	out  *os.File // 向导自己的 stdout
	cin  *os.File
	cinOK bool
	sel  []string // 候选 SSID（按保存状态与信号排序）
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
	}
	if stdinUsable() {
		e.cin = os.Stdin
	} else if e.out != os.Stdout { // 双击形态：从新控制台读输入
		c, err := os.OpenFile("CONIN$", os.O_RDONLY, 0)
		if err != nil {
			return false
		}
		e.cin = c
	}
	e.cinOK = e.cin != nil
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

func isConsoleHandle(f *os.File) bool {
	if f == nil {
		return false
	}
	var mode uint32
	r, _, _ := pGetConsoleMode.Call(uintptr(f.Fd()), uintptr(unsafe.Pointer(&mode)))
	return r != 0
}

func (e *suEnv) printf(format string, args ...any) {
	fmt.Fprintf(e.out, format, args...)
}

// ask 问一个问题并读取一行。cinOK=false（非交互）时返回 def。
func (e *suEnv) ask(prompt, def string) string {
	if !e.cinOK {
		e.printf("%s [%s]（非交互模式，取默认）\n", prompt, def)
		return def
	}
	if prompt != "" {
		e.printf("%s ", prompt)
	}
	var line string
	if _, err := fmt.Fscanln(e.cin, &line); err != nil || line == "" {
		return def
	}
	return strings.TrimSpace(line)
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
	targets := e.pickNetworks(nil, true)
	if len(targets) == 0 {
		e.printf("\n没有选择任何网络，已取消。\n")
		return 1
	}

	e.printf("\n第二步：自动连接（候选里出现这些网络时，自动把 Wi-Fi 切过去）\n")
	e.printf("     说明：这些网络必须已经手动连过至少一次（Windows 才有保存的密码）。\n\n")
	autos := e.pickNetworks(targets, false)

	e.printf("\n第三步：禁止代理网络（连上它们时必须立刻关闭代理，优先级最高）\n")
	e.printf("     典型场景：校园网、公司内网等不允许走代理的网络。\n\n")
	nevers := e.pickNetworks(nil, false)

	fuzzy := e.confirm("\n模糊匹配？（开启时，\"MyPhone\" 与 \"MyPhone-5G\" 视为同一网络）", true)

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
	c.AutoConnect = len(autos) > 0
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
			e.printf("自启注册失败：%v（可稍后手动运行 install-task.ps1）\n", err)
		} else {
			e.printf("已注册计划任务 ClashNetWatch（登录 / 联网 / 唤醒 / 每分钟心跳）。\n")
		}
	}

	// 旧守护进程持有互斥体，新配置对它不可见：关掉再拉起
	if pid, _, ok := findProcess("netwatch.exe"); ok && pid != uint32(os.Getpid()) {
		if err := terminate(pid); err == nil {
			e.printf("已停止旧守护进程（pid %d）。\n", pid)
			if err := startProcess(exeAbs()); err != nil {
				e.printf("重新启动守护进程失败：%v（重启电脑后计划任务会自动拉起）\n", err)
			} else {
				e.printf("守护进程已按新配置重启。\n")
			}
		} else {
			e.printf("旧守护进程无法停止（%v）；它仍用旧配置运行，重启后生效。\n", err)
		}
	} else {
		// 本进程即将退出，不会和它抢互斥体
		if err := startProcess(exeAbs()); err != nil {
			e.printf("启动守护进程失败：%v（重新双击 netwatch.exe 或重启电脑即可）\n", err)
		} else {
			e.printf("守护进程已启动（托盘可见 NetWatch 图标）。\n")
		}
	}

	e.printf("\n全部完成。连上目标网络试试，几秒内会有「已开启代理与 TUN」通知。\n")
	e.printf("状态随时查：netwatch.exe -status　|　重新配置：netwatch.exe -setup\n\n")
	e.pauseIfInteractive()
	return 0
}

func exeAbs() string {
	p, _ := filepath.Abs(os.Args[0])
	return p
}

// pauseIfInteractive 双击形态下控制台窗口读完就关，用户来不及看结果：等一次回车。
// 从 shell 启动时 stdin 是控制台，用户看得见输出，不需要暂停。
func (e *suEnv) pauseIfInteractive() {
	if e.cinOK && !isConsoleHandle(os.Stdin) {
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
	for _, p := range savedProfiles() {
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

func hasSavedProfile(ssid string) bool {
	for _, p := range savedProfiles() {
		if p == ssid {
			return true
		}
	}
	return false
}

// savedProfiles 枚举 Windows 已保存的无线 profile（netsh，只在向导里用一次，
// 不在热路径上，可以接受 spawn 进程的代价）。
func savedProfiles() []string {
	out, err := exec.Command("netsh", "wlan", "show", "profiles").Output()
	if err != nil {
		return nil
	}
	var res []string
	for _, line := range strings.Split(string(out), "\n") {
		// 中文系统 "    所有用户配置文件 : MyPhone"，英文 "All User Profile"
		if i := strings.Index(line, ": "); i >= 0 {
			name := strings.TrimSpace(line[i+2:])
			if name != "" && !strings.Contains(line, "接口") && !strings.Contains(line, "Interface") {
				res = append(res, name)
			}
		}
	}
	return res
}

// pickNetworks 选择一组网络。base 非空时以 base 为默认选择（自动连接步骤），
// 允许选择列表之外的值（手动输入），返回原始输入的 SSID 名。
func (e *suEnv) pickNetworks(base []string, required bool) []string {
	defHint := ""
	if len(base) > 0 {
		defHint = "，回车 = 全选上面选过的目标网络"
	}
	prompt := "输入序号（多个用逗号或空格分隔，如 1,3），或 0 手动输入"
	if !required {
		prompt += "，回车跳过"
	}
	e.printf("%s%s：\n", prompt, defHint)

	chosen := map[string]bool{}
	for {
		line := e.ask("> ", "")
		if line == "" {
			if len(chosen) > 0 {
				return chosenList(chosen)
			}
			if len(base) > 0 {
				return append([]string(nil), base...) // 有默认取默认
			}
			if required {
				e.printf("这一步至少选择一个网络。\n")
				continue
			}
			return nil // 可选步骤：回车跳过
		}
		if line == inputMagic {
			name := e.ask("  输入网络名（SSID，区分大小写不必担心，会自动模糊匹配）：", "")
			if name == "" {
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
				e.printf("  序号 %q 不在列表里，请重输。\n", tok)
				ok = false
				break
			}
			chosen[e.sel[n-1]] = true
			e.printf("  已加入 %q\n", e.sel[n-1])
		}
		if ok {
			out := make([]string, 0, len(chosen))
			for _, b := range base { // base 里的先来，保序
				if chosen[b] {
					out = append(out, b)
				}
			}
			for _, s := range e.sel { // 再加列表里手选的
				if chosen[s] && !containsAny(out, s) {
					out = append(out, s)
				}
			}
			for _, k := range chosenList(chosen) { // 最后加手动输入的
				if !containsAny(out, k) {
					out = append(out, k)
				}
			}
			if len(out) == 0 {
				continue
			}
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
