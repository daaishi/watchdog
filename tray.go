package main

// System tray icon.
//
// Watchdog normally runs with no console and no window, which makes it
// invisible: on site there is no way to tell whether it is running, what it
// thinks the state is, or how to reach the Web UI. The tray icon fixes that —
// its colour is the overall state, its tooltip the summary, and its menu gives
// the operator the actions they actually need.
//
// Everything here has to happen on one OS thread: a window and its message loop
// belong to the thread that created them. runTray locks itself to a thread and
// owns the window; other goroutines talk to it by posting messages.

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

var (
	procGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")

	procRegisterClassExW      = user32.NewProc("RegisterClassExW")
	procCreateWindowExW       = user32.NewProc("CreateWindowExW")
	procDefWindowProcW        = user32.NewProc("DefWindowProcW")
	procDestroyWindow         = user32.NewProc("DestroyWindow")
	procGetMessageW           = user32.NewProc("GetMessageW")
	procTranslateMessage      = user32.NewProc("TranslateMessage")
	procDispatchMessageW      = user32.NewProc("DispatchMessageW")
	procPostQuitMessage       = user32.NewProc("PostQuitMessage")
	procPostMessageW          = user32.NewProc("PostMessageW")
	procCreatePopupMenu       = user32.NewProc("CreatePopupMenu")
	procAppendMenuW           = user32.NewProc("AppendMenuW")
	procDestroyMenu           = user32.NewProc("DestroyMenu")
	procTrackPopupMenu        = user32.NewProc("TrackPopupMenu")
	procSetMenuDefaultItem    = user32.NewProc("SetMenuDefaultItem")
	procGetCursorPos          = user32.NewProc("GetCursorPos")
	procRegisterWindowMessage = user32.NewProc("RegisterWindowMessageW")
	procMessageBoxW           = user32.NewProc("MessageBoxW")
	procLoadCursorW           = user32.NewProc("LoadCursorW")
	procCreateIconIndirect    = user32.NewProc("CreateIconIndirect")
	procDestroyIcon           = user32.NewProc("DestroyIcon")

	gdi32                = syscall.NewLazyDLL("gdi32.dll")
	procCreateDIBSection = gdi32.NewProc("CreateDIBSection")
	procCreateBitmap     = gdi32.NewProc("CreateBitmap")
	procDeleteObject     = gdi32.NewProc("DeleteObject")

	procShellNotifyIconW = shell32.NewProc("Shell_NotifyIconW")
)

const (
	// Window messages we care about.
	wmDestroy       = 0x0002
	wmClose         = 0x0010
	wmCommand       = 0x0111
	wmRButtonUp     = 0x0205
	wmLButtonUp     = 0x0202
	wmLButtonDblClk = 0x0203
	wmApp           = 0x8000

	trayCallbackMsg = wmApp + 1 // mouse events on our icon
	trayUpdateMsg   = wmApp + 2 // "refresh icon and tooltip"

	nimAdd    = 0x0
	nimModify = 0x1
	nimDelete = 0x2

	nifMessage = 0x01
	nifIcon    = 0x02
	nifTip     = 0x04

	mfString    = 0x0000
	mfGrayed    = 0x0001
	mfChecked   = 0x0008
	mfPopup     = 0x0010
	mfSeparator = 0x0800

	tpmRightButton = 0x0002

	mbYesNo         = 0x00000004
	mbIconWarning   = 0x00000030
	mbSetForeground = 0x00010000
	idYes           = 6

	cwUseDefault = 0x80000000

	// Menu command IDs. They are fixed so the menu is reproducible (and can be
	// driven programmatically in tests).
	idOpenUI          = 100
	idAutoStart       = 101
	idRestartWatchdog = 102
	idQuitWatchdog    = 103
	idAppBase         = 200 // app i => idAppBase + i*10 + action
	appActionStart    = 0
	appActionStop     = 1
	appActionRestart  = 2

	trayWindowClass = "WatchdogTrayWnd"
)

type wndClassEx struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     uintptr
	hIcon         uintptr
	hCursor       uintptr
	hbrBackground uintptr
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       uintptr
}

type notifyIconData struct {
	cbSize            uint32
	hWnd              uintptr
	uID               uint32
	uFlags            uint32
	uCallbackMessage  uint32
	hIcon             uintptr
	szTip             [128]uint16
	dwState           uint32
	dwStateMask       uint32
	szInfo            [256]uint16
	uVersionOrTimeout uint32
	szInfoTitle       [64]uint16
	dwInfoFlags       uint32
	guidItem          [16]byte
	hBalloonIcon      uintptr
}

type msgStruct struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      struct{ x, y int32 }
}

type iconInfo struct {
	fIcon    int32
	xHotspot uint32
	yHotspot uint32
	hbmMask  uintptr
	hbmColor uintptr
}

type bitmapInfoHeader struct {
	biSize          uint32
	biWidth         int32
	biHeight        int32
	biPlanes        uint16
	biBitCount      uint16
	biCompression   uint32
	biSizeImage     uint32
	biXPelsPerMeter int32
	biYPelsPerMeter int32
	biClrUsed       uint32
	biClrImportant  uint32
}

// Overall state shown by the icon colour.
type trayHealth int

const (
	healthIdle    trayHealth = iota // nothing enabled
	healthOK                        // everything that should run is running
	healthPending                   // starting up, or waiting for its schedule
	healthProblem                   // enabled but not running, or restart loop
)

var trayColors = map[trayHealth]uint32{
	healthIdle:    0xFF8A99A6,
	healthOK:      0xFF34D399,
	healthPending: 0xFFFBBF24,
	healthProblem: 0xFFF87171,
}

type tray struct {
	wd        *Watchdog
	hwnd      uintptr
	nid       notifyIconData
	icon      uintptr
	health    trayHealth
	tip       string
	taskbarRe uint32 // "TaskbarCreated" message id
}

// ---------------------------------------------------------------------------
// icon drawing
// ---------------------------------------------------------------------------

// makeDotIcon builds a 16x16 icon holding a filled circle of the given colour
// (0xAARRGGBB). Windows has no "give me a coloured dot" API, so the icon is
// drawn into a 32-bit DIB and handed to CreateIconIndirect.
func makeDotIcon(argb uint32) uintptr {
	const size = 16
	bi := bitmapInfoHeader{
		biSize:        uint32(unsafe.Sizeof(bitmapInfoHeader{})),
		biWidth:       size,
		biHeight:      -size, // top-down
		biPlanes:      1,
		biBitCount:    32,
		biCompression: 0, // BI_RGB
	}
	var bits unsafe.Pointer
	hbmColor, _, _ := procCreateDIBSection.Call(
		0, uintptr(unsafe.Pointer(&bi)), 0, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if hbmColor == 0 || bits == nil {
		return 0
	}
	px := unsafe.Slice((*uint32)(bits), size*size)

	a := (argb >> 24) & 0xFF
	r := (argb >> 16) & 0xFF
	g := (argb >> 8) & 0xFF
	b := argb & 0xFF
	const center = (size - 1) / 2.0
	const radius = 6.6

	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			// 3x3 supersampling gives the circle a smooth edge at this size.
			hits := 0
			for sy := 0; sy < 3; sy++ {
				for sx := 0; sx < 3; sx++ {
					dx := float64(x) + (float64(sx)+0.5)/3 - 0.5 - center
					dy := float64(y) + (float64(sy)+0.5)/3 - 0.5 - center
					if dx*dx+dy*dy <= radius*radius {
						hits++
					}
				}
			}
			if hits == 0 {
				px[y*size+x] = 0
				continue
			}
			alpha := a * uint32(hits) / 9
			// The DIB expects premultiplied BGRA.
			px[y*size+x] = alpha<<24 | (r*alpha/255)<<16 | (g*alpha/255)<<8 | (b * alpha / 255)
		}
	}

	hbmMask, _, _ := procCreateBitmap.Call(size, size, 1, 1, 0)
	ii := iconInfo{fIcon: 1, hbmMask: hbmMask, hbmColor: hbmColor}
	hIcon, _, _ := procCreateIconIndirect.Call(uintptr(unsafe.Pointer(&ii)))
	procDeleteObject.Call(hbmColor)
	procDeleteObject.Call(hbmMask)
	return hIcon
}

// ---------------------------------------------------------------------------
// status
// ---------------------------------------------------------------------------

// appTrayState collapses an app's view into one of the states the tray shows.
func appTrayState(v AppStatusView) string {
	if !v.Enabled {
		return "disabled"
	}
	switch v.Status {
	case string(StatusRunning):
		return "running"
	case string(StatusStarting):
		return "starting"
	}
	if v.Schedule != nil && v.Schedule.StartTime != "" && v.Schedule.StopTime != "" &&
		!isInSchedule(v.Schedule, time.Now()) {
		return "waiting"
	}
	return "stopped"
}

func appStateLabel(state string) string {
	switch state {
	case "running":
		return "稼働中"
	case "starting":
		return "起動中"
	case "waiting":
		return "稼働時間外"
	case "stopped":
		return "停止中"
	default:
		return "無効"
	}
}

func stopModeLabel(mode string) string {
	switch mode {
	case "graceful":
		return "保存して終了"
	case "osc":
		return "OSCで保存して終了"
	default:
		return "強制終了"
	}
}

// health summarises every app into one icon colour plus a tooltip line.
func (t *tray) summarize() (trayHealth, string) {
	views := t.wd.getStatusViews()
	var running, pending, problem, disabled int
	for _, v := range views {
		switch appTrayState(v) {
		case "running":
			if v.Restarts > 0 {
				problem++
			} else {
				running++
			}
		case "starting", "waiting":
			pending++
		case "stopped":
			problem++
		default:
			disabled++
		}
	}

	h := healthIdle
	switch {
	case problem > 0:
		h = healthProblem
	case running > 0:
		h = healthOK
	case pending > 0:
		h = healthPending
	}

	tip := fmt.Sprintf("Watchdog %s\n稼働中 %d / 要確認 %d / 待機 %d / 無効 %d",
		Version, running, problem, pending, disabled)
	if len(views) == 0 {
		tip = fmt.Sprintf("Watchdog %s\n監視するアプリがありません", Version)
	}
	return h, tip
}

// ---------------------------------------------------------------------------
// menu
// ---------------------------------------------------------------------------

func appendItem(menu uintptr, flags uintptr, id uintptr, text string) {
	p, err := syscall.UTF16PtrFromString(text)
	if err != nil {
		return
	}
	procAppendMenuW.Call(menu, flags, id, uintptr(unsafe.Pointer(p)))
}

func (t *tray) showMenu() {
	menu, _, _ := procCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer procDestroyMenu.Call(menu)

	appendItem(menu, mfString|mfGrayed, 0, "Watchdog "+Version)
	appendItem(menu, mfSeparator, 0, "")
	appendItem(menu, mfString, idOpenUI, fmt.Sprintf("管理画面を開く (:%d)", t.wd.webPort()))
	procSetMenuDefaultItem.Call(menu, idOpenUI, 0)
	appendItem(menu, mfSeparator, 0, "")

	views := t.wd.getStatusViews()
	if len(views) == 0 {
		appendItem(menu, mfString|mfGrayed, 0, "監視するアプリがありません")
	}
	for i, v := range views {
		state := appTrayState(v)
		sub, _, _ := procCreatePopupMenu.Call()
		base := uintptr(idAppBase + i*10)

		startFlags := uintptr(mfString)
		stopFlags := uintptr(mfString)
		if state == "running" || state == "starting" {
			startFlags |= mfGrayed
		} else {
			stopFlags |= mfGrayed
		}
		appendItem(sub, startFlags, base+appActionStart, "起動")
		appendItem(sub, stopFlags, base+appActionStop, "停止（"+stopModeLabel(v.StopMode)+"）")
		appendItem(sub, mfString, base+appActionRestart, "再起動")

		label := fmt.Sprintf("%s … %s", v.Name, appStateLabel(state))
		if v.Restarts > 0 {
			label += fmt.Sprintf("（再起動 %d 回）", v.Restarts)
		}
		if v.PID > 0 {
			label += fmt.Sprintf("  PID %d", v.PID)
		}
		p, err := syscall.UTF16PtrFromString(label)
		if err == nil {
			procAppendMenuW.Call(menu, mfPopup|mfString, sub, uintptr(unsafe.Pointer(p)))
		}
	}

	appendItem(menu, mfSeparator, 0, "")
	autoFlags := uintptr(mfString)
	if enabled, _ := autoStartState(); enabled {
		autoFlags |= mfChecked
	}
	appendItem(menu, autoFlags, idAutoStart, "Windows起動時に自動実行")
	appendItem(menu, mfSeparator, 0, "")
	appendItem(menu, mfString, idRestartWatchdog, "Watchdogを再起動")
	appendItem(menu, mfString, idQuitWatchdog, "Watchdogを終了")

	var pt struct{ x, y int32 }
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	// The tray window must be foreground or the menu will not dismiss properly.
	procSetForegroundWindow.Call(t.hwnd)
	procTrackPopupMenu.Call(menu, tpmRightButton, uintptr(pt.x), uintptr(pt.y), 0, t.hwnd, 0)
	procPostMessageW.Call(t.hwnd, 0 /* WM_NULL */, 0, 0)
}

func confirm(hwnd uintptr, text, caption string) bool {
	tp, err1 := syscall.UTF16PtrFromString(text)
	cp, err2 := syscall.UTF16PtrFromString(caption)
	if err1 != nil || err2 != nil {
		return false
	}
	ret, _, _ := procMessageBoxW.Call(hwnd,
		uintptr(unsafe.Pointer(tp)), uintptr(unsafe.Pointer(cp)),
		mbYesNo|mbIconWarning|mbSetForeground)
	return ret == idYes
}

// handleCommand runs a menu action. Anything that can block (stopping an app
// saves first, which takes seconds) runs on its own goroutine so the message
// loop keeps serving the tray.
func (t *tray) handleCommand(id uint32) {
	switch {
	case id == idOpenUI:
		go t.wd.openAdminUI()

	case id == idAutoStart:
		enabled, _ := autoStartState()
		if err := setAutoStart(!enabled); err != nil {
			log.Printf("[tray] autostart: %v", err)
		} else {
			log.Printf("[tray] Windows autostart %s", map[bool]string{true: "enabled", false: "disabled"}[!enabled])
		}

	case id == idRestartWatchdog:
		if confirm(t.hwnd, "Watchdogを再起動します。\n監視中のアプリも一度停止して起動し直します。\n\n続行しますか？", "Watchdog") {
			go func() {
				if err := t.wd.restartSelf(); err != nil {
					log.Printf("[tray] restart failed: %v", err)
				}
			}()
		}

	case id == idQuitWatchdog:
		if confirm(t.hwnd, "Watchdogを終了します。\n監視中のアプリも停止します。\n\n続行しますか？", "Watchdog") {
			t.wd.requestShutdown()
		}

	case id >= idAppBase:
		idx := int(id-idAppBase) / 10
		action := int(id-idAppBase) % 10
		views := t.wd.getStatusViews()
		if idx < 0 || idx >= len(views) {
			return
		}
		appID := views[idx].ID
		go func() {
			switch action {
			case appActionStart:
				if err := t.wd.startAppNow(appID); err != nil {
					log.Printf("[%s] tray start: %v", appID, err)
				}
			case appActionStop:
				if _, err := t.wd.setAppEnabled(appID, false); err != nil {
					log.Printf("[%s] tray stop: %v", appID, err)
				}
			case appActionRestart:
				if err := t.wd.restartAppNow(appID); err != nil {
					log.Printf("[%s] tray restart: %v", appID, err)
				}
			}
			t.postUpdate()
		}()
	}
}

func (t *tray) postUpdate() {
	if t.hwnd != 0 {
		procPostMessageW.Call(t.hwnd, trayUpdateMsg, 0, 0)
	}
}

// ---------------------------------------------------------------------------
// icon lifecycle
// ---------------------------------------------------------------------------

func setUTF16(dst []uint16, s string) {
	u, err := syscall.UTF16FromString(s)
	if err != nil {
		return
	}
	if len(u) > len(dst) {
		u = u[:len(dst)]
		u[len(dst)-1] = 0
	}
	copy(dst, u)
}

// apply pushes the current icon and tooltip to the notification area. It reports
// whether the shell accepted the call.
func (t *tray) apply(op uintptr) bool {
	t.nid.cbSize = uint32(unsafe.Sizeof(t.nid))
	t.nid.hWnd = t.hwnd
	t.nid.uID = 1
	t.nid.uFlags = nifMessage | nifIcon | nifTip
	t.nid.uCallbackMessage = trayCallbackMsg
	t.nid.hIcon = t.icon
	setUTF16(t.nid.szTip[:], t.tip)
	ret, _, _ := procShellNotifyIconW.Call(op, uintptr(unsafe.Pointer(&t.nid)))
	return ret != 0
}

// refresh recomputes the state and updates the icon only when it changed, so we
// are not handing explorer a new icon handle every few seconds.
func (t *tray) refresh(force bool) {
	h, tip := t.summarize()
	if !force && h == t.health && tip == t.tip {
		return
	}
	if h != t.health || t.icon == 0 || force {
		icon := makeDotIcon(trayColors[h])
		if icon != 0 {
			old := t.icon
			t.icon = icon
			if old != 0 {
				defer procDestroyIcon.Call(old)
			}
		}
	}
	t.health = h
	t.tip = tip
	t.apply(nimModify)
}

// ---------------------------------------------------------------------------
// window + message loop
// ---------------------------------------------------------------------------

// runTray owns the tray window. It returns when Watchdog shuts down.
func (w *Watchdog) runTray() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	t := &tray{wd: w}

	hInstance, _, _ := procGetModuleHandleW.Call(0)
	classPtr, err := syscall.UTF16PtrFromString(trayWindowClass)
	if err != nil {
		log.Printf("[tray] class name: %v", err)
		return
	}
	cursor, _, _ := procLoadCursorW.Call(0, 32512 /* IDC_ARROW */)

	wndProc := syscall.NewCallback(func(hwnd, msg, wparam, lparam uintptr) uintptr {
		switch uint32(msg) {
		case trayCallbackMsg:
			switch uint32(lparam) {
			case wmRButtonUp, wmLButtonUp:
				t.showMenu()
			case wmLButtonDblClk:
				go w.openAdminUI()
			}
			return 0
		case wmCommand:
			t.handleCommand(uint32(wparam & 0xFFFF))
			return 0
		case trayUpdateMsg:
			t.refresh(false)
			return 0
		case wmClose:
			t.apply(nimDelete)
			procDestroyWindow.Call(hwnd)
			return 0
		case wmDestroy:
			procPostQuitMessage.Call(0)
			return 0
		}
		if t.taskbarRe != 0 && uint32(msg) == t.taskbarRe {
			// Explorer restarted and dropped every icon; add ours back.
			t.apply(nimAdd)
			t.refresh(true)
			return 0
		}
		ret, _, _ := procDefWindowProcW.Call(hwnd, msg, wparam, lparam)
		return ret
	})

	wc := wndClassEx{
		style:         0,
		lpfnWndProc:   wndProc,
		hInstance:     hInstance,
		hCursor:       cursor,
		lpszClassName: classPtr,
	}
	wc.cbSize = uint32(unsafe.Sizeof(wc))
	if ret, _, callErr := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); ret == 0 {
		log.Printf("[tray] RegisterClassEx: %v", callErr)
		return
	}

	titlePtr, _ := syscall.UTF16PtrFromString("Watchdog")
	hwnd, _, callErr := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(classPtr)),
		uintptr(unsafe.Pointer(titlePtr)),
		0, // not WS_VISIBLE: the window only exists to receive messages
		cwUseDefault, cwUseDefault, 0, 0,
		0, 0, hInstance, 0,
	)
	if hwnd == 0 {
		log.Printf("[tray] CreateWindowEx: %v", callErr)
		return
	}
	t.hwnd = hwnd

	if namePtr, err := syscall.UTF16PtrFromString("TaskbarCreated"); err == nil {
		id, _, _ := procRegisterWindowMessage.Call(uintptr(unsafe.Pointer(namePtr)))
		t.taskbarRe = uint32(id)
	}

	t.health, t.tip = t.summarize()
	t.icon = makeDotIcon(trayColors[t.health])
	if t.apply(nimAdd) {
		log.Printf("Tray icon added (right-click it for the menu; Windows may keep it in the hidden-icons flyout)")
	} else {
		log.Printf("WARNING: the shell refused the tray icon; use the Web UI instead")
	}

	// Keep the state fresh, and leave the loop when Watchdog shuts down.
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-w.shutdownCh:
				procPostMessageW.Call(hwnd, wmClose, 0, 0)
				return
			case <-stop:
				return
			case <-ticker.C:
				t.postUpdate()
			}
		}
	}()
	defer close(stop)

	var msg msgStruct
	for {
		ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(ret) <= 0 { // 0 = WM_QUIT, -1 = error
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}
	if t.icon != 0 {
		procDestroyIcon.Call(t.icon)
	}
}

// ---------------------------------------------------------------------------
// actions the tray triggers
// ---------------------------------------------------------------------------

func (w *Watchdog) webPort() int {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.config.WebPort == 0 {
		return 4649
	}
	return w.config.WebPort
}

// openAdminUI opens the Web UI in the default browser.
func (w *Watchdog) openAdminUI() {
	url := fmt.Sprintf("http://localhost:%d/", w.webPort())
	if err := openURL(url); err != nil {
		log.Printf("[tray] could not open %s: %v", url, err)
	}
}

// restartSelf starts a fresh copy of this executable and shuts this one down.
// The new process waits for this PID to disappear before it binds the port, so
// the two never fight over it.
func (w *Watchdog) restartSelf() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, fmt.Sprintf("--wait-pid=%d", os.Getpid()))
	cmd.Dir = filepath.Dir(exe)
	flags := uint32(0x08000000) // CREATE_NO_WINDOW
	if w.config.ShowConsole {
		flags = 0x00000010 // CREATE_NEW_CONSOLE
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags}
	if err := cmd.Start(); err != nil {
		return err
	}
	if cmd.Process != nil {
		cmd.Process.Release()
	}
	log.Printf("Restarting: started a new instance, this one is shutting down")
	w.requestShutdown()
	return nil
}
