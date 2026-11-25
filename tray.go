// 托盘图标 + 切换通知：让"到底有没有生效"变成看得见的事。
//
// 为什么需要它：Verge 的界面不会因为我们改内核/注册表而刷新，用户在界面上看不出变化。
// 所以由本进程自己给出可见反馈——切换瞬间弹一条通知，图标悬停显示当前网络与状态。
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	wmAppStatus = 0x8000 + 1 // 状态变了，刷新图标提示
	wmTrayCall  = 0x8000 + 2 // 托盘鼠标事件

	nimAdd    = 0
	nimModify = 1
	nimDelete = 2

	nifMessage = 0x01
	nifIcon    = 0x02
	nifTip     = 0x04
	nifInfo    = 0x10
	niifInfo   = 0x01

	idTray        = 1
	idMenuRefresh = 1001
	idMenuLog     = 1002
	idMenuPause   = 1003
	idMenuQuit    = 1004
	idMenuConnect = 1005
	idMenuSetup   = 1006

	wmLButtonUp = 0x0202
	wmRButtonUp = 0x0205
	wmCommand   = 0x0111
	wmDestroy   = 0x0002
)

type notifyIconData struct {
	CbSize           uint32
	HWnd             syscall.Handle
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            syscall.Handle
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UVersion         uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GuidItem         [16]byte
	HBalloonIcon     syscall.Handle
}

type wndClassEx struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     syscall.Handle
	HIcon         syscall.Handle
	HCursor       syscall.Handle
	HbrBackground syscall.Handle
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       syscall.Handle
}

type msgStruct struct {
	HWnd    syscall.Handle
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      struct{ X, Y int32 }
}

var (
	pRegisterClassEx   = user32.NewProc("RegisterClassExW")
	pCreateWindowExW   = user32.NewProc("CreateWindowExW")
	pDefWindowProc     = user32.NewProc("DefWindowProcW")
	pDestroyWindow     = user32.NewProc("DestroyWindow")
	pPostMessage       = user32.NewProc("PostMessageW")
	pGetMessage        = user32.NewProc("GetMessageW")
	pTranslateMessage  = user32.NewProc("TranslateMessage")
	pDispatchMessage   = user32.NewProc("DispatchMessageW")
	pLoadIcon          = user32.NewProc("LoadIconW")
	pCreatePopupMenu   = user32.NewProc("CreatePopupMenu")
	pAppendMenu        = user32.NewProc("AppendMenuW")
	pTrackPopupMenu    = user32.NewProc("TrackPopupMenu")
	pDestroyMenu       = user32.NewProc("DestroyMenu")
	pGetCursorPos      = user32.NewProc("GetCursorPos")
	pSetForeground     = user32.NewProc("SetForegroundWindow")
	pRegisterWindowMsg = user32.NewProc("RegisterWindowMessageW")

	pShellNotifyIcon = shell32.NewProc("Shell_NotifyIconW")
	pShellExecute    = shell32.NewProc("ShellExecuteW")
)

var (
	trayMu        sync.Mutex
	trayHWnd      syscall.Handle
	traySSID      string
	trayWant      bool
	trayLast      string // 最近一次切换（"10:51:05 切换开"），悬停时显示
	trayBalloonQ  string // 待弹出的通知文本
	trayOK        bool   // 托盘是否真的加上了
	lastClickAt   time.Time
	lastBalloonAt time.Time

	taskbarCreatedMsg uint32
)

// trayNotify 更新托盘状态；balloon 非空时同时弹一条通知。
func trayNotify(ssid string, want bool, balloon string) {
	trayMu.Lock()
	traySSID, trayWant = ssid, want
	if balloon != "" {
		trayLast = time.Now().Format("15:04:05") + " 切换" + map[bool]string{true: "开", false: "关"}[want]
		trayBalloonQ = balloon
	}
	hwnd := trayHWnd
	trayMu.Unlock()
	if hwnd != 0 {
		pPostMessage.Call(uintptr(hwnd), wmAppStatus, 0, 0)
	}
}

func trayTip() string {
	trayMu.Lock()
	defer trayMu.Unlock()
	ssid := traySSID
	if ssid == "" {
		ssid = "未连接"
	}
	state := "代理关"
	if trayWant {
		state = "代理开"
	}
	tip := fmt.Sprintf("NetWatch · %s · %s", ssid, state)
	if autoPaused() {
		tip += "（已暂停）"
	}
	if trayLast != "" {
		tip += "｜" + trayLast
	}
	return tip
}

func trayIconData(flags uint32) *notifyIconData {
	n := &notifyIconData{
		HWnd:             trayHWnd,
		UID:              idTray,
		UFlags:           flags,
		UCallbackMessage: wmTrayCall,
	}
	n.CbSize = uint32(unsafe.Sizeof(*n))
	icon, _, _ := pLoadIcon.Call(0, 32512) // IDI_APPLICATION
	n.HIcon = syscall.Handle(icon)
	return n
}

// trayReady 读取托盘状态：trayOK/trayHWnd 由托盘线程写、worker 线程也会读，
// 统一走这把锁，避免数据竞争。
func trayReady() (syscall.Handle, bool) {
	trayMu.Lock()
	defer trayMu.Unlock()
	return trayHWnd, trayOK
}

func trayAdd() {
	if !cfg.ShowTray || trayHWnd == 0 {
		return
	}
	n := trayIconData(nifMessage | nifIcon | nifTip)
	tip, _ := syscall.UTF16FromString(trayTip())
	copy(n.SzTip[:], tip)
	r, _, _ := pShellNotifyIcon.Call(nimAdd, uintptr(unsafe.Pointer(n)))
	trayMu.Lock()
	trayOK = r != 0
	ok := trayOK
	trayMu.Unlock()
	if !ok {
		// 常见原因：explorer 未运行、或进程不在交互式会话里
		lg.logf("WARN", "托盘图标添加失败，界面通知不可用（功能本身不受影响）")
	}
}

func trayRefresh() {
	if !cfg.ShowTray || trayHWnd == 0 {
		return
	}
	if _, ok := trayReady(); !ok {
		trayAdd()
		return
	}
	n := trayIconData(nifTip)
	tip, _ := syscall.UTF16FromString(trayTip())
	copy(n.SzTip[:], tip)
	pShellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(n)))
}

func showBalloon(title, text string) bool {
	hwnd, ok := trayReady()
	if !cfg.ShowTray || hwnd == 0 || !ok {
		return false
	}
	n := trayIconData(nifInfo)
	t, _ := syscall.UTF16FromString(title)
	b, _ := syscall.UTF16FromString(text)
	copy(n.SzInfoTitle[:], t)
	copy(n.SzInfo[:], b)
	n.DwInfoFlags = niifInfo
	r, _, _ := pShellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(n)))
	return r != 0
}

// trayBalloon 是状态变更通知，要进日志（便于对账"通知到底是没发还是没显示"）
func trayBalloon(title, text string) {
	// 网络抖动时可能连续切换，3 秒内只弹一次，避免通知风暴
	if time.Since(lastBalloonAt) < 3*time.Second {
		lg.debug("通知被限流：%s", text)
		return
	}
	if showBalloon(title, text) {
		lastBalloonAt = time.Now()
		lg.logf("INFO", "已弹出通知：%s", text)
		return
	}
	lg.logf("WARN", "托盘通知发送失败（可能被系统通知设置拦截）：%s", text)
}

// trayBalloonForce 用户主动点击触发的提示：跳过 3 秒限流（否则刚切完网再点菜单会"没反应"），
// 仍然记日志。
func trayBalloonForce(title, text string) {
	if showBalloon(title, text) {
		lastBalloonAt = time.Now()
		lg.logf("INFO", "已弹出提示：%s", text)
		return
	}
	lg.logf("WARN", "托盘提示发送失败（可能被系统通知设置拦截）：%s", text)
}

// trayAlert 是用户主动点击触发的提示，不算状态变更，日志降级
func trayAlert(title, text string) {
	if !showBalloon(title, text) {
		lg.debug("托盘提示未显示：%s", text)
	}
}

func trayRemove() {
	hwnd, ok := trayReady()
	if hwnd == 0 || !ok {
		return
	}
	n := trayIconData(0)
	pShellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(n)))
	trayMu.Lock()
	trayOK = false
	trayMu.Unlock()
}

func trayMenu(hwnd syscall.Handle) {
	menu, _, _ := pCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer pDestroyMenu.Call(menu)
	add := func(id uintptr, text string) {
		p, _ := syscall.UTF16FromString(text)
		pAppendMenu.Call(menu, 0, id, uintptr(unsafe.Pointer(&p[0])))
	}
	add(idMenuRefresh, "刷新 Clash Verge 界面")
	add(idMenuConnect, "立即连接目标热点")
	add(idMenuSetup, "网络设置（重新运行向导）")
	add(idMenuLog, "打开日志")
	if autoPaused() {
		add(idMenuPause, "恢复自动切换")
	} else {
		add(idMenuPause, fmt.Sprintf("暂停自动切换 %d 分钟", cfg.PauseMinutes))
	}
	add(idMenuQuit, "退出 NetWatch")

	var pt struct{ X, Y int32 }
	pGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	pSetForeground.Call(uintptr(hwnd))
	pTrackPopupMenu.Call(menu, 0, uintptr(pt.X), uintptr(pt.Y), 0, uintptr(hwnd), 0)
	pPostMessage.Call(uintptr(hwnd), 0, 0, 0) // 关掉菜单后补一个空消息，避免菜单不消失
}

func trayCommand(id uint32) {
	switch id {
	case idMenuRefresh:
		exe, _ := filepath.Abs(os.Args[0])
		op, _ := syscall.UTF16FromString("open")
		f, _ := syscall.UTF16FromString(exe)
		args, _ := syscall.UTF16FromString("-refresh")
		pShellExecute.Call(0, uintptr(unsafe.Pointer(&op[0])), uintptr(unsafe.Pointer(&f[0])),
			uintptr(unsafe.Pointer(&args[0])), 0, 0)
		trayBalloonForce("NetWatch", "已触发刷新，详见 netwatch.log")
	case idMenuConnect:
		msg, _ := forceAutoConnect()
		lg.logf("INFO", "托盘触发「立即连接目标热点」：%s", msg)
		trayBalloonForce("NetWatch", msg)
	case idMenuSetup:
		exe, _ := filepath.Abs(os.Args[0])
		op, _ := syscall.UTF16FromString("open")
		f, _ := syscall.UTF16FromString(exe)
		args, _ := syscall.UTF16FromString("-setup")
		pShellExecute.Call(0, uintptr(unsafe.Pointer(&op[0])), uintptr(unsafe.Pointer(&f[0])),
			uintptr(unsafe.Pointer(&args[0])), 0, 1) // SW_SHOWNORMAL：向导要可见的控制台
		trayBalloonForce("NetWatch", "设置向导已打开，按提示重新选择网络即可")
	case idMenuLog:
		logPath, _ := filepath.Abs(filepath.Join(exeDir, "netwatch.log"))
		op, _ := syscall.UTF16FromString("open")
		f, _ := syscall.UTF16FromString(logPath)
		pShellExecute.Call(0, uintptr(unsafe.Pointer(&op[0])), uintptr(unsafe.Pointer(&f[0])), 0, 0, 1)
	case idMenuPause:
		if autoPaused() {
			resumeAuto()
			lg.logf("INFO", "用户从托盘恢复自动切换")
			trayBalloonForce("NetWatch", "已恢复自动切换")
		} else {
			pauseFor(time.Duration(cfg.PauseMinutes) * time.Minute)
			lg.logf("INFO", "用户从托盘暂停自动切换 %d 分钟", cfg.PauseMinutes)
			trayBalloonForce("NetWatch", "已暂停 30 分钟，不再自动开代理/连热点；禁止网络下仍会强制关闭")
		}
	case idMenuQuit:
		trayRemove()
		os.Exit(0)
	}
}

func trayWndProc(hwnd syscall.Handle, msg uint32, wParam, lParam uintptr) uintptr {
	switch msg {
	case wmTrayCall:
		switch lParam {
		case wmLButtonUp:
			// 双击会连发两次 LButtonUp，连点也会刷屏——1.5 秒内只响应一次
			if time.Since(lastClickAt) < 1500*time.Millisecond {
				return 0
			}
			lastClickAt = time.Now()
			trayAlert("NetWatch", trayTip())
		case wmRButtonUp:
			trayMenu(hwnd)
		}
		return 0
	case wmAppStatus:
		trayRefresh()
		trayMu.Lock()
		b := trayBalloonQ
		trayBalloonQ = ""
		trayMu.Unlock()
		if b != "" {
			trayBalloon("NetWatch", b)
		}
		return 0
	case wmCommand:
		trayCommand(uint32(wParam & 0xFFFF))
		return 0
	case wmDestroy:
		trayMu.Lock()
		trayHWnd = 0
		trayMu.Unlock()
		return 0
	}
	if taskbarCreatedMsg != 0 && msg == taskbarCreatedMsg {
		trayMu.Lock()
		trayOK = false // explorer 重启后托盘图标丢失，重新添加
		trayMu.Unlock()
		trayAdd()
		return 0
	}
	r, _, _ := pDefWindowProc.Call(uintptr(hwnd), uintptr(msg), wParam, lParam)
	return r
}

// startTray 建隐藏窗口 + 注册托盘 + 跑消息循环。失败不影响主功能。
// 必须在同一线程里创建窗口并跑消息循环，否则消息派发不到。
func startTray() {
	runtime.LockOSThread()
	if !cfg.ShowTray {
		return
	}
	cls, _ := syscall.UTF16FromString("NetWatchTrayWnd")
	title, _ := syscall.UTF16FromString("NetWatch")
	hInst, _, _ := pGetModuleHandle.Call(0)

	var wc wndClassEx
	wc.CbSize = uint32(unsafe.Sizeof(wc))
	wc.LpfnWndProc = syscall.NewCallback(trayWndProc)
	wc.HInstance = syscall.Handle(hInst)
	wc.LpszClassName = &cls[0]
	if r, _, e := pRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		lg.logf("WARN", "注册托盘窗口类失败：%v，界面通知不可用", e)
		return
	}
	r, _, e := pCreateWindowExW.Call(0, uintptr(unsafe.Pointer(&cls[0])),
		uintptr(unsafe.Pointer(&title[0])), 0, 0, 0, 0, 0, 0, 0, hInst, 0)
	if r == 0 {
		lg.logf("WARN", "创建托盘窗口失败：%v，界面通知不可用", e)
		return
	}
	trayMu.Lock()
	trayHWnd = syscall.Handle(r)
	trayMu.Unlock()

	tb, _ := syscall.UTF16FromString("TaskbarCreated")
	regMsg, _, _ := pRegisterWindowMsg.Call(uintptr(unsafe.Pointer(&tb[0])))
	taskbarCreatedMsg = uint32(regMsg)
	trayAdd()
	lg.logf("INFO", "托盘图标已就绪（悬停看状态，右键有菜单）")

	// 托盘初始化会碰不少 USER32 路径，堆会长一截；空闲后归还给系统，压住常驻内存
	go func() {
		time.Sleep(8 * time.Second)
		debug.FreeOSMemory()
	}()

	var m msgStruct
	for {
		// GetMessage 返回 0 表示 WM_QUIT，-1 表示出错
		rr, _, _ := pGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(rr) <= 0 {
			return
		}
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		pDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
}
