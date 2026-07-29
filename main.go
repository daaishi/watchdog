package main

import (
	"bufio"
	"context"
	"crypto/md5"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// Version is set at build time via -ldflags.
var Version = "dev"

//go:embed templates/index.html
var embeddedTemplate string

// ---------------------------------------------------------------------------
// Date-rotating log writer
// ---------------------------------------------------------------------------

type dateRotatingWriter struct {
	mu      sync.Mutex
	dir     string
	current *os.File
	date    string
	console bool
}

func newDateRotatingWriter(dir string, console bool) (*dateRotatingWriter, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create log dir %q: %w", dir, err)
	}
	w := &dateRotatingWriter{dir: dir, console: console}
	if err := w.rotate(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *dateRotatingWriter) rotate() error {
	today := time.Now().Format("2006-01-02")
	if w.date == today && w.current != nil {
		return nil
	}
	if w.current != nil {
		w.current.Close()
	}
	filename := filepath.Join(w.dir, fmt.Sprintf("watchdog-%s.log", today))
	f, err := os.OpenFile(filename, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	w.current = f
	w.date = today
	return nil
}

func (w *dateRotatingWriter) Write(p []byte) (n int, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if rotErr := w.rotate(); rotErr != nil {
		if w.current == nil {
			if w.console {
				return os.Stdout.Write(p)
			}
			return 0, rotErr
		}
	}
	n, err = w.current.Write(p)
	if w.console {
		os.Stdout.Write(p)
	}
	return n, err
}

// CurrentPath returns the log file being written to, or "" if there is none.
func (w *dateRotatingWriter) CurrentPath() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.current == nil {
		return ""
	}
	return w.current.Name()
}

func (w *dateRotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.current != nil {
		return w.current.Close()
	}
	return nil
}

// ---------------------------------------------------------------------------
// Windows API for window-title monitoring
// ---------------------------------------------------------------------------

var (
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleWindow   = kernel32.NewProc("GetConsoleWindow")
	procGetCurrentThreadId = kernel32.NewProc("GetCurrentThreadId")
	procGetProcessId       = kernel32.NewProc("GetProcessId")
	procCreateMutexW       = kernel32.NewProc("CreateMutexW")

	user32                       = syscall.NewLazyDLL("user32.dll")
	procShowWindow               = user32.NewProc("ShowWindow")
	procFindWindowW              = user32.NewProc("FindWindowW")
	procEnumWindows              = user32.NewProc("EnumWindows")
	procGetWindowTextW           = user32.NewProc("GetWindowTextW")
	procGetWindowThreadProcessId = user32.NewProc("GetWindowThreadProcessId")
	procIsWindowVisible          = user32.NewProc("IsWindowVisible")
	procSetForegroundWindow      = user32.NewProc("SetForegroundWindow")
	procGetForegroundWindow      = user32.NewProc("GetForegroundWindow")
	procBringWindowToTop         = user32.NewProc("BringWindowToTop")
	procAttachThreadInput        = user32.NewProc("AttachThreadInput")
	procKeybdEvent               = user32.NewProc("keybd_event")

	comdlg32            = syscall.NewLazyDLL("comdlg32.dll")
	procGetOpenFileName = comdlg32.NewProc("GetOpenFileNameW")

	shell32                 = syscall.NewLazyDLL("shell32.dll")
	procSHBrowseForFolder   = shell32.NewProc("SHBrowseForFolderW")
	procSHGetPathFromIDList = shell32.NewProc("SHGetPathFromIDListW")
	procShellExecuteEx      = shell32.NewProc("ShellExecuteExW")

	shlwapi               = syscall.NewLazyDLL("shlwapi.dll")
	procAssocQueryStringW = shlwapi.NewProc("AssocQueryStringW")

	ole32              = syscall.NewLazyDLL("ole32.dll")
	procCoInitializeEx = ole32.NewProc("CoInitializeEx")
	procCoUninitialize = ole32.NewProc("CoUninitialize")
	procCoTaskMemFree  = ole32.NewProc("CoTaskMemFree")
)

// hideConsoleWindow hides the console window attached to this process.
func hideConsoleWindow() {
	hwnd, _, _ := procGetConsoleWindow.Call()
	if hwnd != 0 {
		procShowWindow.Call(hwnd, 0) // SW_HIDE = 0
	}
}

func findWindowByTitle(title string) bool {
	found := false
	cb := syscall.NewCallback(func(hwnd uintptr, lParam uintptr) uintptr {
		buf := make([]uint16, 256)
		procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), 256)
		windowTitle := syscall.UTF16ToString(buf)
		if strings.Contains(windowTitle, title) {
			found = true
			return 0 // stop enumeration
		}
		return 1 // continue
	})
	procEnumWindows.Call(cb, 0)
	return found
}

// ---------------------------------------------------------------------------
// Native file / folder picker dialogs (Windows)
//
// The Web UI runs on the same machine as the watched apps, so opening a native
// dialog server-side lets the user pick a real absolute path instead of typing
// it. (A browser <input type=file> only exposes a fake "C:\fakepath\" path.)
// ---------------------------------------------------------------------------

type openFileName struct {
	lStructSize       uint32
	hwndOwner         uintptr
	hInstance         uintptr
	lpstrFilter       *uint16
	lpstrCustomFilter *uint16
	nMaxCustFilter    uint32
	nFilterIndex      uint32
	lpstrFile         *uint16
	nMaxFile          uint32
	lpstrFileTitle    *uint16
	nMaxFileTitle     uint32
	lpstrInitialDir   *uint16
	lpstrTitle        *uint16
	flags             uint32
	nFileOffset       uint16
	nFileExtension    uint16
	lpstrDefExt       *uint16
	lCustData         uintptr
	lpfnHook          uintptr
	lpTemplateName    *uint16
	pvReserved        uintptr
	dwReserved        uint32
	flagsEx           uint32
}

const (
	ofnPathMustExist = 0x00000800
	ofnFileMustExist = 0x00001000
	ofnExplorer      = 0x00080000
	ofnNoChangeDir   = 0x00000008
)

// buildFilter converts ["Programs","*.exe","All Files","*.*"] into the
// double-NUL-terminated UTF-16 buffer that comdlg32 expects.
func buildFilter(pairs []string) *uint16 {
	var u []uint16
	for _, s := range pairs {
		p, err := syscall.UTF16FromString(s)
		if err != nil {
			continue
		}
		u = append(u, p...) // each includes a trailing NUL
	}
	u = append(u, 0) // final extra NUL terminates the list
	return &u[0]
}

// pickFile shows a native "Open File" dialog and returns the chosen path,
// or "" if the user cancelled.
func pickFile(title string, filterPairs []string) string {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	buf := make([]uint16, 4096)

	var filter *uint16
	if len(filterPairs) > 0 {
		filter = buildFilter(filterPairs)
	}
	var titlePtr *uint16
	if title != "" {
		titlePtr, _ = syscall.UTF16PtrFromString(title)
	}

	ofn := openFileName{
		lpstrFilter: filter,
		lpstrFile:   &buf[0],
		nMaxFile:    uint32(len(buf)),
		lpstrTitle:  titlePtr,
		flags:       ofnPathMustExist | ofnFileMustExist | ofnExplorer | ofnNoChangeDir,
	}
	ofn.lStructSize = uint32(unsafe.Sizeof(ofn))

	ret, _, _ := procGetOpenFileName.Call(uintptr(unsafe.Pointer(&ofn)))
	if ret == 0 {
		return "" // cancelled or error
	}
	return syscall.UTF16ToString(buf)
}

type browseInfo struct {
	hwndOwner      uintptr
	pidlRoot       uintptr
	pszDisplayName *uint16
	lpszTitle      *uint16
	ulFlags        uint32
	lpfn           uintptr
	lParam         uintptr
	iImage         int32
}

const (
	bifReturnOnlyFSDirs = 0x00000001
	bifEditBox          = 0x00000010
	bifNewDialogStyle   = 0x00000040
	coinitApartment     = 0x2
)

// pickFolder shows a native "Browse For Folder" dialog and returns the chosen
// directory, or "" if the user cancelled.
func pickFolder(title string) string {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// BIF_NEWDIALOGSTYLE needs an apartment-threaded COM context.
	procCoInitializeEx.Call(0, coinitApartment)
	defer procCoUninitialize.Call()

	var titlePtr *uint16
	if title != "" {
		titlePtr, _ = syscall.UTF16PtrFromString(title)
	}
	displayBuf := make([]uint16, 260)
	bi := browseInfo{
		pszDisplayName: &displayBuf[0],
		lpszTitle:      titlePtr,
		ulFlags:        bifReturnOnlyFSDirs | bifEditBox | bifNewDialogStyle,
	}

	pidl, _, _ := procSHBrowseForFolder.Call(uintptr(unsafe.Pointer(&bi)))
	if pidl == 0 {
		return "" // cancelled
	}
	defer procCoTaskMemFree.Call(pidl)

	pathBuf := make([]uint16, 4096)
	ret, _, _ := procSHGetPathFromIDList.Call(pidl, uintptr(unsafe.Pointer(&pathBuf[0])))
	if ret == 0 {
		return ""
	}
	return syscall.UTF16ToString(pathBuf)
}

// ---------------------------------------------------------------------------
// Watch method constants
// ---------------------------------------------------------------------------

const (
	WatchUDP     = "udp"
	WatchProcess = "process"
	WatchFile    = "file"
	WatchHTTP    = "http"
	WatchWindow  = "window"
)

// ---------------------------------------------------------------------------
// Config types
// ---------------------------------------------------------------------------

type WatchConfig struct {
	// UDP
	UDPPort int `json:"udp_port,omitempty"`
	// File
	FilePath string `json:"file_path,omitempty"`
	// HTTP
	URL        string `json:"url,omitempty"`
	ExpectCode int    `json:"expect_code,omitempty"`
	// Window
	WindowTitle string `json:"window_title,omitempty"`
}

type ScheduleConfig struct {
	StartTime string   `json:"start_time"` // "HH:MM"
	StopTime  string   `json:"stop_time"`  // "HH:MM"
	Days      []string `json:"days"`       // e.g. ["Monday","Friday"], empty = every day
}

type AppConfig struct {
	ID            string          `json:"id"`
	Name          string          `json:"name"`
	ExePath       string          `json:"exe_path"`
	Args          []string        `json:"args"`
	UseShellOpen  bool            `json:"use_shell_open"`
	WatchMethod   string          `json:"watch_method"`
	WatchConfig   WatchConfig     `json:"watch_config"`
	TimeoutSec    int             `json:"timeout_sec"`
	Enabled       bool            `json:"enabled"`
	AutoStart     bool            `json:"auto_start"`
	StartOrder    int             `json:"start_order"`
	StartDelaySec int             `json:"start_delay_sec"`
	Schedule      *ScheduleConfig `json:"schedule,omitempty"`
	// StopMode controls how the process is stopped on schedule/manual stop:
	//   "" or "force" -> taskkill (immediate)
	//   "graceful"    -> focus window, send Ctrl+S (save), then terminate by PID
	//   "osc"         -> send an OSC save (and optional quit) message, then
	//                    terminate by PID as a fallback
	StopMode string `json:"stop_mode,omitempty"`
	// OSC stop settings (used when StopMode == "osc").
	OSCHost     string `json:"osc_host,omitempty"`      // default 127.0.0.1
	OSCPort     int    `json:"osc_port,omitempty"`      // default 8010
	OSCSaveAddr string `json:"osc_save_addr,omitempty"` // default /mmServer/global/save
	OSCQuitAddr string `json:"osc_quit_addr,omitempty"` // blank = save then PID-kill
}

// CommandConfig is a network command (UDP / OSC / PJLINK) fired at a scheduled
// time, and also manually via the "Test" button.
type CommandConfig struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"` // "udp" | "osc" | "pjlink"
	Host string `json:"host"`
	Port int    `json:"port"`
	// UDP
	Payload    string `json:"payload,omitempty"`
	PayloadHex bool   `json:"payload_hex,omitempty"`
	// OSC
	OSCAddr string `json:"osc_addr,omitempty"`
	OSCArgs string `json:"osc_args,omitempty"` // space-separated; int/float/string auto-typed
	// PJLINK
	PJCommand  string `json:"pj_command,omitempty"`  // e.g. "POWR 1"
	PJPassword string `json:"pj_password,omitempty"` // blank = no auth
	// Schedule
	Time    string   `json:"time"`           // "HH:MM"; blank = manual only
	Days    []string `json:"days,omitempty"` // empty = every day
	Enabled bool     `json:"enabled"`
}

type Config struct {
	WebPort     int  `json:"web_port"`
	ShowConsole bool `json:"show_console"`
	// DisableTray removes the system tray icon. Off by default: the tray is the
	// only visible sign that Watchdog is running at all.
	DisableTray bool            `json:"disable_tray,omitempty"`
	LogDir      string          `json:"log_dir"`
	RebootTime  string          `json:"reboot_time,omitempty"`
	RebootDays  []string        `json:"reboot_days,omitempty"`
	Apps        []AppConfig     `json:"apps"`
	Commands    []CommandConfig `json:"commands,omitempty"`
}

// ---------------------------------------------------------------------------
// Runtime state per watched app
// ---------------------------------------------------------------------------

type AppStatus string

const (
	StatusRunning  AppStatus = "running"
	StatusStopped  AppStatus = "stopped"
	StatusStarting AppStatus = "starting"
)

type WatchedApp struct {
	Config        AppConfig
	PID           int
	Status        AppStatus
	LastHeartbeat time.Time
	StartedAt     time.Time

	mu      sync.Mutex
	udpConn *net.UDPConn
	stopCh  chan struct{}
	stopped bool

	// lastDiscover throttles process-list scans, which cost a PowerShell run.
	lastDiscover time.Time
	// restarts counts consecutive automatic restarts; it drives the backoff that
	// stops a mis-detected crash from turning into a launch storm.
	restarts           int
	lastRestart        time.Time
	restartDeferLogged bool
}

// ---------------------------------------------------------------------------
// Watchdog – central coordinator
// ---------------------------------------------------------------------------

type Watchdog struct {
	configPath   string
	config       Config
	apps         map[string]*WatchedApp
	mu           sync.RWMutex
	templates    *template.Template
	httpServer   *http.Server
	logWriter    *dateRotatingWriter
	shutdownCh   chan struct{}
	shutdownOnce sync.Once
}

// requestShutdown starts an orderly shutdown. Safe to call more than once and
// from anywhere (Web UI, tray, scheduled reboot).
func (w *Watchdog) requestShutdown() {
	w.shutdownOnce.Do(func() { close(w.shutdownCh) })
}

func NewWatchdog(configPath string) (*Watchdog, error) {
	w := &Watchdog{
		configPath: configPath,
		apps:       make(map[string]*WatchedApp),
		shutdownCh: make(chan struct{}),
	}
	if err := w.loadConfig(); err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	t, err := template.New("index.html").Parse(embeddedTemplate)
	if err != nil {
		return nil, fmt.Errorf("parse template: %w", err)
	}
	w.templates = t
	return w, nil
}

func (w *Watchdog) loadConfig() error {
	data, err := os.ReadFile(w.configPath)
	if err != nil {
		return err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return err
	}
	w.config = cfg
	return nil
}

func (w *Watchdog) saveConfig() error {
	data, err := json.MarshalIndent(w.config, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(w.configPath, data, 0644)
}

// ---------------------------------------------------------------------------
// Process launch helpers
// ---------------------------------------------------------------------------

const (
	seeMaskNoCloseProcess = 0x00000040
	seeMaskNoAsync        = 0x00000100
	swShowNormal          = 1
)

type shellExecuteInfo struct {
	cbSize         uint32
	fMask          uint32
	hwnd           uintptr
	lpVerb         *uint16
	lpFile         *uint16
	lpParameters   *uint16
	lpDirectory    *uint16
	nShow          int32
	hInstApp       uintptr
	lpIDList       uintptr
	lpClass        *uint16
	hkeyClass      uintptr
	dwHotKey       uint32
	hIconOrMonitor uintptr
	hProcess       uintptr
}

// shellOpenDocument opens a document with its associated application — the
// equivalent of double-clicking it — and returns the PID of the process the
// shell created.
//
// It returns 0 when the shell created no process: single-instance apps like
// MadMapper and TouchDesigner hand the document to an already-running instance
// instead. The caller then has to discover the PID by scanning the process list.
//
// This replaces the older `cmd /C start` trick: ShellExecuteEx gives us a real
// process handle, so in the normal case the PID is known exactly and
// immediately instead of being guessed afterwards.
func shellOpenDocument(path string, args []string) (int, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	procCoInitializeEx.Call(0, coinitApartment)
	defer procCoUninitialize.Call()

	filePtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	dirPtr, _ := syscall.UTF16PtrFromString(filepath.Dir(path))
	var paramPtr *uint16
	if len(args) > 0 {
		paramPtr, _ = syscall.UTF16PtrFromString(strings.Join(args, " "))
	}

	// lpVerb stays nil so the file type's default verb is used, exactly like a
	// double-click. SEE_MASK_NOASYNC is required because we have no message loop.
	info := shellExecuteInfo{
		fMask:        seeMaskNoCloseProcess | seeMaskNoAsync,
		lpFile:       filePtr,
		lpParameters: paramPtr,
		lpDirectory:  dirPtr,
		nShow:        swShowNormal,
	}
	info.cbSize = uint32(unsafe.Sizeof(info))

	ret, _, callErr := procShellExecuteEx.Call(uintptr(unsafe.Pointer(&info)))
	if ret == 0 {
		return 0, fmt.Errorf("ShellExecuteEx %q: %v", path, callErr)
	}
	if info.hProcess == 0 {
		return 0, nil
	}
	pid, _, _ := procGetProcessId.Call(info.hProcess)
	syscall.CloseHandle(syscall.Handle(info.hProcess))
	return int(pid), nil
}

// openURL opens a URL with the user's default handler (used for the Web UI).
func openURL(target string) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	procCoInitializeEx.Call(0, coinitApartment)
	defer procCoUninitialize.Call()

	filePtr, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	info := shellExecuteInfo{
		fMask:  seeMaskNoAsync,
		lpFile: filePtr,
		nShow:  swShowNormal,
	}
	info.cbSize = uint32(unsafe.Sizeof(info))
	if ret, _, callErr := procShellExecuteEx.Call(uintptr(unsafe.Pointer(&info))); ret == 0 {
		return fmt.Errorf("ShellExecuteEx %q: %v", target, callErr)
	}
	return nil
}

// acquireSingleInstance takes a named mutex derived from the config path. Two
// instances watching the same config would fight over the same apps and the same
// port, which autostart makes easy to do by accident (shortcut + logon entry).
// The returned bool is false when another instance already holds it.
func acquireSingleInstance(configPath string) (syscall.Handle, bool) {
	name := "Local\\watchdog-" + strings.Map(func(r rune) rune {
		if r == '\\' || r == '/' || r == ':' {
			return '_'
		}
		return r
	}, strings.ToLower(configPath))

	namePtr, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return 0, true
	}
	h, _, callErr := procCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(namePtr)))
	if h == 0 {
		return 0, true // cannot guard; better to run than to refuse
	}
	if errno, ok := callErr.(syscall.Errno); ok && errno == 183 { // ERROR_ALREADY_EXISTS
		return syscall.Handle(h), false
	}
	return syscall.Handle(h), true
}

// waitForProcessExit blocks until pid is gone or the timeout expires. Used when
// a restarted instance has to let its predecessor release the port first.
func waitForProcessExit(pid int, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !isProcessAlive(pid) {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// launchApp starts an app and returns the PID assigned to it, or 0 when the
// launch produced no new process (see shellOpenDocument).
func launchApp(cfg AppConfig, showConsole bool) (int, error) {
	if cfg.UseShellOpen {
		return shellOpenDocument(cfg.ExePath, cfg.Args)
	}

	cmd := exec.Command(cfg.ExePath, cfg.Args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if !showConsole {
		cmd.SysProcAttr = &syscall.SysProcAttr{
			CreationFlags: 0x08000000, // CREATE_NO_WINDOW
		}
	}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	// Reap the child once it exits so its handle isn't held forever; liveness is
	// tracked by PID, not through this handle.
	go cmd.Wait()
	return cmd.Process.Pid, nil
}

// procInfo is a single running process as reported by Win32_Process.
type procInfo struct {
	pid     int
	name    string
	created time.Time
	cmdline string
}

// enumProcesses lists running processes via PowerShell CIM. We use CIM instead
// of wmic because wmic has been removed from recent Windows 11 builds.
func enumProcesses() ([]procInfo, error) {
	const sep = "\x1f" // unit separator — won't appear in paths/command lines
	script := `Get-CimInstance Win32_Process | ForEach-Object { ` +
		`$t = ''; if ($_.CreationDate) { $t = $_.CreationDate.ToString('yyyyMMddHHmmss') }; ` +
		`"$($_.ProcessId)` + sep + `$($_.Name)` + sep + `$t` + sep + `$($_.CommandLine)" }`
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("enum processes: %w", err)
	}

	var procs []procInfo
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, sep, 4)
		if len(parts) < 3 {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil {
			continue
		}
		p := procInfo{pid: pid, name: parts[1]}
		if t, err := time.ParseInLocation("20060102150405", strings.TrimSpace(parts[2]), time.Local); err == nil {
			p.created = t
		}
		if len(parts) == 4 {
			p.cmdline = parts[3]
		}
		procs = append(procs, p)
	}
	return procs, nil
}

const (
	assocstrExecutable      = 2
	assocfInitIgnoreUnknown = 0x00000400
)

// associatedHostExe resolves the application registered to open a document type,
// e.g. ".mad" -> "MadMapper.exe". Returns "" if nothing is registered.
func associatedHostExe(docPath string) string {
	ext := filepath.Ext(docPath)
	if ext == "" {
		return ""
	}
	extPtr, err := syscall.UTF16PtrFromString(ext)
	if err != nil {
		return ""
	}
	buf := make([]uint16, 1024)
	n := uint32(len(buf))
	ret, _, _ := procAssocQueryStringW.Call(
		assocfInitIgnoreUnknown,
		assocstrExecutable,
		uintptr(unsafe.Pointer(extPtr)),
		0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&n)),
	)
	if ret != 0 { // anything but S_OK
		return ""
	}
	return filepath.Base(syscall.UTF16ToString(buf))
}

// How confidently a running process was identified as a configured app.
// Lower is better.
const (
	// The configured path appears in the command line: identified beyond doubt.
	matchDocPath = 0
	// Only the document's file name appears — same document, moved or copied.
	matchDocName = 1
	// The application registered for this document type is running, but was
	// started from some other document. Single-instance apps like MadMapper and
	// TouchDesigner keep the command line of whatever document opened them
	// first, so this is the only way to recognise an instance that adopted our
	// document afterwards — which is what happens every time the shell hands a
	// document to a running instance.
	matchHostExe = 2
)

// appMatch is a live process that looks like a configured app.
type appMatch struct {
	proc      procInfo
	rank      int
	hasWindow bool
}

// matchApp reports whether p looks like cfg's process.
//
//   - For a shell-open document (e.g. a .mad / .toe file launched by file
//     association) the real process is the host app (MadMapper.exe, launched
//     with the document path on its command line), so we match by that path —
//     NOT by the document's own image name.
//   - For a directly-launched exe we match by image name, plus the first arg
//     if present to disambiguate multiple instances.
func matchApp(cfg AppConfig, hostExe string, p procInfo) (rank int, ok bool) {
	cl := strings.ToLower(p.cmdline)
	if cfg.UseShellOpen {
		if full := strings.ToLower(cfg.ExePath); full != "" && strings.Contains(cl, full) {
			return matchDocPath, true
		}
		if base := strings.ToLower(filepath.Base(cfg.ExePath)); base != "" && strings.Contains(cl, base) {
			return matchDocName, true
		}
		if hostExe != "" && strings.EqualFold(p.name, hostExe) {
			return matchHostExe, true
		}
		return 0, false
	}
	if !strings.EqualFold(p.name, filepath.Base(cfg.ExePath)) {
		return 0, false
	}
	if len(cfg.Args) > 0 && !strings.Contains(cl, strings.ToLower(cfg.Args[0])) {
		return 0, false
	}
	return matchDocPath, true
}

// findAppInstances returns every live process that looks like cfg's app, the
// most plausible one first.
//
// The ranking is what keeps document-based apps stable. MadMapper and
// TouchDesigner are single-instance: opening a document while the app is already
// running does not give us a process of our own — the shell hands the document
// to the running instance, whose command line still names whatever document
// opened it first. Identifying the app only by the configured document path
// therefore fails exactly when the app is healthy, which used to be read as a
// crash and "fixed" by re-opening the document every timeout, forever.
//
// So a process is matched on three levels (see the match* constants), and among
// equally-ranked candidates we prefer the one owning a visible window and then
// the older one — the instance that actually survives a hand-off.
func findAppInstances(cfg AppConfig) []appMatch {
	procs, err := enumProcesses()
	if err != nil {
		log.Printf("[%s] process enumeration failed: %v", cfg.ID, err)
		return nil
	}
	self := os.Getpid()

	// A script document is hosted by cmd.exe itself, so only exclude the shell
	// for documents opened by a real application.
	ext := strings.ToLower(filepath.Ext(cfg.ExePath))
	skipShell := ext != ".bat" && ext != ".cmd"

	var hostExe string
	if cfg.UseShellOpen {
		hostExe = associatedHostExe(cfg.ExePath)
	}

	var out []appMatch
	for _, p := range procs {
		if p.pid == self || strings.EqualFold(p.name, "conhost.exe") {
			continue
		}
		if skipShell && strings.EqualFold(p.name, "cmd.exe") {
			continue
		}
		rank, ok := matchApp(cfg, hostExe, p)
		if !ok || !isProcessAlive(p.pid) {
			continue
		}
		out = append(out, appMatch{
			proc:      p,
			rank:      rank,
			hasWindow: findMainWindowByPID(p.pid) != 0,
		})
	}

	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.rank != b.rank {
			return a.rank < b.rank
		}
		if a.hasWindow != b.hasWindow {
			return a.hasWindow
		}
		if !a.proc.created.Equal(b.proc.created) {
			return a.proc.created.Before(b.proc.created)
		}
		return a.proc.pid < b.proc.pid
	})
	return out
}

// discoverPID finds the PID of an app's process, or 0 if no live instance
// matches.
func discoverPID(cfg AppConfig) int {
	inst := findAppInstances(cfg)
	if len(inst) == 0 {
		return 0
	}
	return inst[0].proc.pid
}

// killAppInstances terminates every live instance of a shell-open app except
// exceptPID, so a restart can't leave duplicates behind. Only processes carrying
// the configured document path are touched, which makes this precise — unlike
// image-name matching, it can never hit an unrelated copy of the host app.
func killAppInstances(cfg AppConfig, exceptPID int) {
	for _, m := range findAppInstances(cfg) {
		if m.rank != matchDocPath || m.proc.pid == exceptPID {
			continue
		}
		if err := killPID(m.proc.pid); err == nil {
			log.Printf("[%s] Killed leftover instance PID %d", cfg.ID, m.proc.pid)
		}
	}
}

func killPID(pid int) error {
	cmd := exec.Command("taskkill", "/F", "/PID", strconv.Itoa(pid))
	return cmd.Run()
}

// findMainWindowByPID returns the first visible top-level window owned by pid,
// or 0 if none is found.
func findMainWindowByPID(pid int) uintptr {
	var target uintptr
	cb := syscall.NewCallback(func(hwnd uintptr, lParam uintptr) uintptr {
		var wpid uint32
		procGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&wpid)))
		if int(wpid) == pid {
			if vis, _, _ := procIsWindowVisible.Call(hwnd); vis != 0 {
				target = hwnd
				return 0 // stop enumeration
			}
		}
		return 1 // continue
	})
	procEnumWindows.Call(cb, 0)
	return target
}

// focusWindow brings hwnd to the foreground and confirms it actually became
// the foreground window. It uses the AttachThreadInput trick to bypass
// Windows' foreground-lock (which otherwise blocks SetForegroundWindow from a
// background process). Returns false if focus could not be confirmed — in which
// case the caller MUST NOT send keystrokes (they would land on another app).
func focusWindow(hwnd uintptr) bool {
	const swRestore = 9

	// Fast path: already the foreground window (typical on a kiosk).
	if fg, _, _ := procGetForegroundWindow.Call(); fg == hwnd {
		return true
	}

	// AttachThreadInput requires stable thread identity for the duration.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	fg, _, _ := procGetForegroundWindow.Call()
	myThread, _, _ := procGetCurrentThreadId.Call()
	var fgThread uintptr
	if fg != 0 {
		fgThread, _, _ = procGetWindowThreadProcessId.Call(fg, 0)
	}

	attached := false
	if fgThread != 0 && fgThread != myThread {
		if ok, _, _ := procAttachThreadInput.Call(myThread, fgThread, 1); ok != 0 {
			attached = true
		}
	}

	procShowWindow.Call(hwnd, swRestore)
	procBringWindowToTop.Call(hwnd)
	procSetForegroundWindow.Call(hwnd)

	if attached {
		procAttachThreadInput.Call(myThread, fgThread, 0)
	}

	for i := 0; i < 10; i++ {
		if f, _, _ := procGetForegroundWindow.Call(); f == hwnd {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// sendCtrlS synthesizes a Ctrl+S keystroke to the foreground window.
func sendCtrlS() {
	const (
		vkControl = 0x11
		vkS       = 0x53
		keyUp     = 0x0002
	)
	procKeybdEvent.Call(vkControl, 0, 0, 0)
	procKeybdEvent.Call(vkS, 0, 0, 0)
	procKeybdEvent.Call(vkS, 0, keyUp, 0)
	procKeybdEvent.Call(vkControl, 0, keyUp, 0)
}

// gracefulSave focuses the app's own window and sends Ctrl+S so it saves before
// being terminated. Ctrl+S is sent ONLY after confirming the app's window is
// the foreground window, so a stray keystroke can never reach another app
// (e.g. the terminal). It deliberately does NOT send Alt+F4 — the caller stops
// the process by PID, which is reliable and, since we saved first, data-safe.
func gracefulSave(cfg AppConfig, pid int) {
	hwnd := findMainWindowByPID(pid)
	if hwnd == 0 {
		log.Printf("[%s] graceful: no visible window for PID %d; skipping save", cfg.ID, pid)
		return
	}
	if !focusWindow(hwnd) {
		log.Printf("[%s] graceful: could not focus window; skipping Ctrl+S to avoid stray keystrokes", cfg.ID)
		return
	}
	time.Sleep(300 * time.Millisecond)
	sendCtrlS()
	log.Printf("[%s] graceful: sent Ctrl+S, waiting for save to complete ...", cfg.ID)
	time.Sleep(3 * time.Second)
}

// oscPad null-terminates an OSC string and pads it to a 4-byte boundary.
func oscPad(s string) []byte {
	b := append([]byte(s), 0)
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return b
}

// sendOSC sends a no-argument OSC message to host:port over UDP.
func sendOSC(host string, port int, addr string) error {
	if addr == "" {
		return nil
	}
	raddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", host, port))
	if err != nil {
		return err
	}
	conn, err := net.DialUDP("udp", nil, raddr)
	if err != nil {
		return err
	}
	defer conn.Close()
	msg := append(oscPad(addr), oscPad(",")...) // address + empty type-tag string
	_, err = conn.Write(msg)
	return err
}

// oscStop saves (and optionally quits) the app over OSC. Sending OSC does not
// depend on window focus, so unlike the keystroke path it can never disturb
// another window. It always returns; the caller force-kills as a fallback.
func oscStop(cfg AppConfig, pid int) {
	host := cfg.OSCHost
	if host == "" {
		host = "127.0.0.1"
	}
	port := cfg.OSCPort
	if port == 0 {
		port = 8010
	}
	saveAddr := cfg.OSCSaveAddr
	if saveAddr == "" {
		saveAddr = "/mmServer/global/save"
	}

	if err := sendOSC(host, port, saveAddr); err != nil {
		log.Printf("[%s] OSC save send failed: %v", cfg.ID, err)
	} else {
		log.Printf("[%s] OSC save sent (%s -> %s:%d); waiting for save ...", cfg.ID, saveAddr, host, port)
	}
	time.Sleep(3 * time.Second)

	if cfg.OSCQuitAddr != "" {
		if err := sendOSC(host, port, cfg.OSCQuitAddr); err != nil {
			log.Printf("[%s] OSC quit send failed: %v", cfg.ID, err)
		} else {
			log.Printf("[%s] OSC quit sent (%s); waiting for exit ...", cfg.ID, cfg.OSCQuitAddr)
		}
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			if !isProcessAlive(pid) {
				log.Printf("[%s] Exited via OSC quit (PID %d)", cfg.ID, pid)
				return
			}
			time.Sleep(1 * time.Second)
		}
		log.Printf("[%s] Still alive after OSC quit; forcing kill", cfg.ID)
	}
}

// stopProcess stops a running app's process honoring its StopMode:
//   - "graceful": focus the app's window and send Ctrl+S, then terminate by PID.
//   - "osc": send an OSC save (and optional quit) message, then terminate by PID.
//   - anything else: terminate immediately.
//
// In every mode the process is ultimately terminated by PID, which is reliable
// and — because we save first — data-safe.
func stopProcess(cfg AppConfig, pid int) {
	if pid <= 0 {
		return
	}
	switch cfg.StopMode {
	case "graceful":
		log.Printf("[%s] Graceful stop (PID %d): save (keys) then quit", cfg.ID, pid)
		gracefulSave(cfg, pid)
	case "osc":
		log.Printf("[%s] OSC stop (PID %d): save (OSC) then quit", cfg.ID, pid)
		oscStop(cfg, pid)
	}
	killPID(pid)
}

// isProcessAlive checks whether a PID is still running using the Windows
// OpenProcess API. This is more reliable than parsing tasklist output which
// can vary by locale.
//
// IMPORTANT: a successful OpenProcess is NOT proof the process is still running.
// A process object lingers as long as any handle to it remains open — which is
// common for apps launched via the shell / file association (e.g. MadMapper via
// a .mad file). In that case OpenProcess keeps succeeding after the app is
// closed, so we must additionally verify the process hasn't been signaled
// (exited) and that its exit code is still STILL_ACTIVE.
func isProcessAlive(pid int) bool {
	const processQueryLimitedInformation = 0x1000
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)

	// GetExitCodeProcess works with PROCESS_QUERY_LIMITED_INFORMATION and does
	// not require SYNCHRONIZE access (which WaitForSingleObject would). A running
	// process reports STILL_ACTIVE; a process that has already exited reports its
	// real exit code even while its handle lingers, so this correctly detects a
	// closed app whose process object is kept alive by another open handle.
	const stillActive = 259 // STILL_ACTIVE
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}

// ---------------------------------------------------------------------------
// Per-app lifecycle
// ---------------------------------------------------------------------------

func (w *Watchdog) startApp(wa *WatchedApp) error {
	wa.mu.Lock()
	defer wa.mu.Unlock()

	cfg := wa.Config

	// Single-instance document apps (MadMapper .mad, TouchDesigner .toe) give us
	// no process of our own when the document is opened again — the shell hands
	// it to the running instance. Attach to that instance instead of launching on
	// top of it, so a restart can never stack up windows.
	if cfg.UseShellOpen {
		if inst := findAppInstances(cfg); len(inst) > 0 {
			best := inst[0]
			if best.rank == matchHostExe {
				// The app is up but was started from a different document, so ask
				// it to open ours before attaching — otherwise we would watch an
				// instance showing the wrong project.
				if _, err := launchApp(cfg, w.config.ShowConsole); err != nil {
					log.Printf("[%s] could not send open request: %v", cfg.ID, err)
				} else {
					log.Printf("[%s] %s already running with another document; opened %s in it",
						cfg.ID, best.proc.name, filepath.Base(cfg.ExePath))
				}
			} else {
				log.Printf("[%s] Already running; attached to PID %d", cfg.ID, best.proc.pid)
			}
			wa.PID = best.proc.pid
			wa.Status = StatusRunning
			wa.StartedAt = time.Now()
			wa.LastHeartbeat = time.Now()
			return nil
		}
	}

	wa.Status = StatusStarting
	log.Printf("[%s] Starting %s ...", cfg.ID, cfg.Name)

	pid, err := launchApp(cfg, w.config.ShowConsole)
	if err != nil {
		wa.Status = StatusStopped
		return fmt.Errorf("launch %s: %w", cfg.ID, err)
	}

	if pid > 0 {
		wa.PID = pid
		wa.Status = StatusRunning
		wa.StartedAt = time.Now()
		wa.LastHeartbeat = time.Now()
		log.Printf("[%s] Started with PID %d", cfg.ID, pid)
		return nil
	}

	// The shell reported no new process, so the document went to an instance that
	// was already coming up. Find it in the background; Status stays "starting"
	// meanwhile, which keeps the timeout checker from firing.
	log.Printf("[%s] Launched; discovering PID ...", cfg.ID)
	go func() {
		for i := 0; i < 20; i++ {
			select {
			case <-wa.stopCh:
				return
			case <-time.After(3 * time.Second):
			}
			pid := discoverPID(cfg)
			wa.mu.Lock()
			if wa.stopped {
				wa.mu.Unlock()
				return
			}
			if pid > 0 {
				wa.PID = pid
				wa.Status = StatusRunning
				wa.StartedAt = time.Now()
				wa.LastHeartbeat = time.Now()
				wa.mu.Unlock()
				log.Printf("[%s] Discovered PID %d", cfg.ID, pid)
				return
			}
			wa.mu.Unlock()
		}
		// Give up on discovery but keep watching: the process watcher retries,
		// and the timeout checker restarts the app if nothing ever shows up.
		log.Printf("[%s] WARNING: could not discover PID; will keep looking", cfg.ID)
		wa.mu.Lock()
		if !wa.stopped {
			wa.Status = StatusRunning
			wa.StartedAt = time.Now()
			wa.LastHeartbeat = time.Now()
		}
		wa.mu.Unlock()
	}()

	return nil
}

func (w *Watchdog) killAndRestart(wa *WatchedApp) {
	wa.mu.Lock()
	pid := wa.PID
	autoStart := wa.Config.AutoStart
	wa.Status = StatusStopped
	wa.PID = 0
	wa.restarts++
	wa.lastRestart = time.Now()
	wa.restartDeferLogged = false
	wa.mu.Unlock()

	if pid > 0 {
		log.Printf("[%s] Killing PID %d ...", wa.Config.ID, pid)
		if err := killPID(pid); err != nil {
			log.Printf("[%s] taskkill: %v (process may have already exited)", wa.Config.ID, err)
		}
	}
	// Take any other instance of the same document with it, so the restart starts
	// from a clean slate instead of adding to what is already open.
	if wa.Config.UseShellOpen {
		killAppInstances(wa.Config, 0)
	}

	time.Sleep(2 * time.Second)

	if autoStart {
		if err := w.startApp(wa); err != nil {
			log.Printf("[%s] Restart failed: %v", wa.Config.ID, err)
		}
	} else {
		// Watch-only mode: try to find the process again (it may have
		// been restarted externally, e.g. by Windows startup).
		log.Printf("[%s] Watch-only: searching for process ...", wa.Config.ID)
		newPID := findExistingPID(wa.Config)
		wa.mu.Lock()
		wa.PID = newPID
		wa.Status = StatusRunning
		wa.LastHeartbeat = time.Now()
		wa.mu.Unlock()
		if newPID > 0 {
			log.Printf("[%s] Re-discovered PID %d", wa.Config.ID, newPID)
		} else {
			log.Printf("[%s] Process not found, will keep watching", wa.Config.ID)
		}
	}
}

// ---------------------------------------------------------------------------
// Watch method: UDP heartbeat listener
// ---------------------------------------------------------------------------

func (w *Watchdog) listenHeartbeatUDP(wa *WatchedApp) {
	port := wa.Config.WatchConfig.UDPPort
	if port == 0 {
		log.Printf("[%s] UDP port not configured", wa.Config.ID)
		return
	}
	addr := &net.UDPAddr{IP: net.IPv4(0, 0, 0, 0), Port: port}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		log.Printf("[%s] UDP listen error on port %d: %v", wa.Config.ID, port, err)
		return
	}
	wa.mu.Lock()
	wa.udpConn = conn
	wa.mu.Unlock()

	log.Printf("[%s] Listening for heartbeat on UDP :%d", wa.Config.ID, port)

	buf := make([]byte, 512)
	for {
		select {
		case <-wa.stopCh:
			conn.Close()
			return
		default:
		}
		conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			continue
		}
		if n > 0 {
			wa.mu.Lock()
			wa.LastHeartbeat = time.Now()
			wa.mu.Unlock()
		}
	}
}

// ---------------------------------------------------------------------------
// Watch method: Process existence check
// ---------------------------------------------------------------------------

// rediscoverInterval throttles how often a lost PID is looked up again; each
// scan costs a PowerShell / CIM run.
const rediscoverInterval = 15 * time.Second

func (w *Watchdog) checkProcessOnce(wa *WatchedApp) {
	wa.mu.Lock()
	pid := wa.PID
	status := wa.Status
	cfg := wa.Config
	lastDiscover := wa.lastDiscover
	wa.mu.Unlock()

	if status != StatusRunning {
		return
	}

	if pid > 0 && isProcessAlive(pid) {
		wa.mu.Lock()
		wa.LastHeartbeat = time.Now()
		wa.mu.Unlock()
		return
	}

	// The tracked PID is gone, or was never found. That is not proof the app died:
	// a document-based app may have handed its document to another instance and
	// exited, and a watch-only app may have been restarted outside our control.
	// Look for a live instance before letting the timeout checker restart it.
	if time.Since(lastDiscover) < rediscoverInterval {
		return
	}
	wa.mu.Lock()
	wa.lastDiscover = time.Now()
	wa.mu.Unlock()

	newPID := discoverPID(cfg)
	if newPID <= 0 {
		return // really gone: the timeout checker takes over
	}
	wa.mu.Lock()
	if wa.Status == StatusRunning {
		wa.PID = newPID
		wa.LastHeartbeat = time.Now()
	}
	wa.mu.Unlock()
	log.Printf("[%s] Re-attached to live instance PID %d (was %d)", cfg.ID, newPID, pid)
}

func (w *Watchdog) watchProcess(wa *WatchedApp) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	log.Printf("[%s] Watching process existence (PID-based)", wa.Config.ID)

	// Immediate first check.
	w.checkProcessOnce(wa)

	for {
		select {
		case <-wa.stopCh:
			return
		case <-ticker.C:
			w.checkProcessOnce(wa)
		}
	}
}

// ---------------------------------------------------------------------------
// Watch method: File timestamp check
// ---------------------------------------------------------------------------

func (w *Watchdog) checkFileOnce(wa *WatchedApp) {
	filePath := wa.Config.WatchConfig.FilePath
	info, err := os.Stat(filePath)
	if err != nil {
		return
	}
	modTime := info.ModTime()
	wa.mu.Lock()
	timeout := wa.Config.TimeoutSec
	wa.mu.Unlock()

	if time.Since(modTime) < time.Duration(timeout)*time.Second {
		wa.mu.Lock()
		wa.LastHeartbeat = time.Now()
		wa.mu.Unlock()
	}
}

func (w *Watchdog) watchFile(wa *WatchedApp) {
	filePath := wa.Config.WatchConfig.FilePath
	if filePath == "" {
		log.Printf("[%s] File path not configured", wa.Config.ID)
		return
	}
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	log.Printf("[%s] Watching file timestamp: %s", wa.Config.ID, filePath)
	w.checkFileOnce(wa)

	for {
		select {
		case <-wa.stopCh:
			return
		case <-ticker.C:
			w.checkFileOnce(wa)
		}
	}
}

// ---------------------------------------------------------------------------
// Watch method: HTTP health check
// ---------------------------------------------------------------------------

func (w *Watchdog) watchHTTP(wa *WatchedApp) {
	url := wa.Config.WatchConfig.URL
	if url == "" {
		log.Printf("[%s] HTTP URL not configured", wa.Config.ID)
		return
	}
	expectCode := wa.Config.WatchConfig.ExpectCode
	if expectCode == 0 {
		expectCode = 200
	}

	client := &http.Client{Timeout: 5 * time.Second}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	log.Printf("[%s] Watching HTTP endpoint: %s (expect %d)", wa.Config.ID, url, expectCode)

	checkOnce := func() {
		wa.mu.Lock()
		status := wa.Status
		wa.mu.Unlock()
		if status != StatusRunning {
			return
		}
		resp, err := client.Get(url)
		if err != nil {
			return
		}
		resp.Body.Close()
		if resp.StatusCode == expectCode {
			wa.mu.Lock()
			wa.LastHeartbeat = time.Now()
			wa.mu.Unlock()
		}
	}

	// Delay initial HTTP check to give the app time to start.
	time.Sleep(3 * time.Second)
	checkOnce()

	for {
		select {
		case <-wa.stopCh:
			return
		case <-ticker.C:
			checkOnce()
		}
	}
}

// ---------------------------------------------------------------------------
// Watch method: Window title check
// ---------------------------------------------------------------------------

func (w *Watchdog) checkWindowOnce(wa *WatchedApp) {
	wa.mu.Lock()
	status := wa.Status
	wa.mu.Unlock()
	if status != StatusRunning {
		return
	}
	if findWindowByTitle(wa.Config.WatchConfig.WindowTitle) {
		wa.mu.Lock()
		wa.LastHeartbeat = time.Now()
		wa.mu.Unlock()
	}
}

func (w *Watchdog) watchWindow(wa *WatchedApp) {
	title := wa.Config.WatchConfig.WindowTitle
	if title == "" {
		log.Printf("[%s] Window title not configured", wa.Config.ID)
		return
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	log.Printf("[%s] Watching window title: %q", wa.Config.ID, title)

	// Delay initial check to let the window appear.
	time.Sleep(3 * time.Second)
	w.checkWindowOnce(wa)

	for {
		select {
		case <-wa.stopCh:
			return
		case <-ticker.C:
			w.checkWindowOnce(wa)
		}
	}
}

// ---------------------------------------------------------------------------
// Schedule helpers
// ---------------------------------------------------------------------------

func parseHHMM(s string) (int, int, error) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid time format %q", s)
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil || h < 0 || h > 23 {
		return 0, 0, fmt.Errorf("invalid hour in %q", s)
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil || m < 0 || m > 59 {
		return 0, 0, fmt.Errorf("invalid minute in %q", s)
	}
	return h, m, nil
}

func isDayMatch(days []string, now time.Time) bool {
	if len(days) == 0 {
		return true
	}
	today := now.Weekday().String()
	for _, d := range days {
		if strings.EqualFold(d, today) {
			return true
		}
	}
	return false
}

func isInSchedule(sched *ScheduleConfig, now time.Time) bool {
	if sched == nil || sched.StartTime == "" || sched.StopTime == "" {
		return true // no schedule = always on
	}
	if !isDayMatch(sched.Days, now) {
		return false
	}
	startH, startM, err := parseHHMM(sched.StartTime)
	if err != nil {
		return true
	}
	stopH, stopM, err := parseHHMM(sched.StopTime)
	if err != nil {
		return true
	}
	startMin := startH*60 + startM
	stopMin := stopH*60 + stopM
	nowMin := now.Hour()*60 + now.Minute()

	if startMin <= stopMin {
		return nowMin >= startMin && nowMin < stopMin
	}
	// Overnight: e.g. 22:00 - 06:00
	return nowMin >= startMin || nowMin < stopMin
}

// ---------------------------------------------------------------------------
// Timeout checker (shared for all watch methods)
// ---------------------------------------------------------------------------

// restartHealthyReset is how long an app must stay responsive before earlier
// restarts are forgotten and the backoff starts over.
const restartHealthyReset = 10 * time.Minute

// restartBackoff spaces out repeated automatic restarts. The first one is
// immediate — a real crash should be recovered at once — but a restart that does
// not fix anything backs off, up to 5 minutes, so a mis-detected crash can never
// become a launch storm.
func restartBackoff(consecutive int) time.Duration {
	if consecutive <= 0 {
		return 0
	}
	d := 30 * time.Second << uint(min(consecutive-1, 4))
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	return d
}

func (w *Watchdog) watchTimeout(wa *WatchedApp) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-wa.stopCh:
			return
		case <-ticker.C:
			wa.mu.Lock()
			status := wa.Status
			last := wa.LastHeartbeat
			timeout := wa.Config.TimeoutSec
			wa.mu.Unlock()

			if status != StatusRunning || timeout <= 0 {
				continue
			}

			if time.Since(last) <= time.Duration(timeout)*time.Second {
				// Responsive: once it has held up for a while, forget past restarts.
				wa.mu.Lock()
				if wa.restarts > 0 && time.Since(wa.lastRestart) > restartHealthyReset {
					wa.restarts = 0
				}
				wa.mu.Unlock()
				continue
			}

			wa.mu.Lock()
			restarts := wa.restarts
			backoff := restartBackoff(restarts)
			deferred := restarts > 0 && time.Since(wa.lastRestart) < backoff
			logDeferred := deferred && !wa.restartDeferLogged
			if logDeferred {
				wa.restartDeferLogged = true
			}
			wa.mu.Unlock()

			if deferred {
				if logDeferred {
					log.Printf("[%s] Heartbeat timeout (%ds), but %d restart(s) did not help; waiting %s before retrying",
						wa.Config.ID, timeout, restarts, backoff)
				}
				continue
			}

			log.Printf("[%s] Heartbeat timeout (%ds). Restarting ...",
				wa.Config.ID, timeout)
			w.killAndRestart(wa)
		}
	}
}

// ---------------------------------------------------------------------------
// Schedule-based app start/stop
// ---------------------------------------------------------------------------

func (w *Watchdog) watchSchedule(wa *WatchedApp) {
	sched := wa.Config.Schedule
	if sched == nil || sched.StartTime == "" || sched.StopTime == "" {
		return
	}

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	log.Printf("[%s] Schedule active: %s-%s days=%v",
		wa.Config.ID, sched.StartTime, sched.StopTime, sched.Days)

	// If the app is currently outside its schedule window, treat it as
	// already "scheduled-stopped" so the next window opening triggers a
	// start. Otherwise an app registered before its start time (started
	// outside the window, never launched) would never start when 10:00 hits.
	scheduledStop := !isInSchedule(sched, time.Now())

	for {
		select {
		case <-wa.stopCh:
			return
		case <-ticker.C:
			now := time.Now()
			inSched := isInSchedule(sched, now)

			wa.mu.Lock()
			status := wa.Status
			wa.mu.Unlock()

			if !inSched && status == StatusRunning && !scheduledStop {
				log.Printf("[%s] Outside schedule (%s-%s), stopping",
					wa.Config.ID, sched.StartTime, sched.StopTime)
				wa.mu.Lock()
				pid := wa.PID
				wa.Status = StatusStopped
				wa.PID = 0
				wa.mu.Unlock()
				stopProcess(wa.Config, pid)
				scheduledStop = true
			} else if inSched && scheduledStop {
				log.Printf("[%s] Inside schedule (%s-%s), starting",
					wa.Config.ID, sched.StartTime, sched.StopTime)
				if err := w.startApp(wa); err != nil {
					log.Printf("[%s] Schedule restart failed: %v", wa.Config.ID, err)
				}
				scheduledStop = false
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Start / stop watched apps
// ---------------------------------------------------------------------------

// findExistingPID tries to locate an already-running process by exe name
// (and optionally args). Used when auto_start=false.
func findExistingPID(cfg AppConfig) int {
	return discoverPID(cfg)
}

func (w *Watchdog) addAndStart(cfg AppConfig) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if _, exists := w.apps[cfg.ID]; exists {
		return fmt.Errorf("app %q already running", cfg.ID)
	}

	wa := &WatchedApp{
		Config: cfg,
		Status: StatusStopped,
		stopCh: make(chan struct{}),
	}

	hasSchedule := cfg.Schedule != nil && cfg.Schedule.StartTime != "" && cfg.Schedule.StopTime != ""

	// If outside schedule, don't start the app — just register and wait.
	// The watcher goroutines are still launched so that once watchSchedule
	// starts the app at its start_time, crashes are detected and recovered
	// (the watchers idle while Status != running).
	if hasSchedule && !isInSchedule(cfg.Schedule, time.Now()) {
		log.Printf("[%s] Outside schedule, not starting", cfg.ID)
		w.apps[cfg.ID] = wa
		w.startWatchers(wa)
		go w.watchSchedule(wa)
		return nil
	}

	if cfg.AutoStart {
		// Launch the process.
		if err := w.startApp(wa); err != nil {
			return err
		}
	} else {
		// Watch-only mode: discover existing process.
		log.Printf("[%s] Watch-only mode (auto_start=false)", cfg.ID)
		pid := findExistingPID(cfg)
		wa.mu.Lock()
		if pid > 0 {
			wa.PID = pid
			wa.Status = StatusRunning
			wa.LastHeartbeat = time.Now()
			wa.StartedAt = time.Now()
			log.Printf("[%s] Found existing PID %d", cfg.ID, pid)
		} else {
			wa.Status = StatusRunning
			wa.LastHeartbeat = time.Now()
			log.Printf("[%s] No existing process found, monitoring anyway", cfg.ID)
		}
		wa.mu.Unlock()
	}

	w.apps[cfg.ID] = wa

	w.startWatchers(wa)
	if hasSchedule {
		go w.watchSchedule(wa)
	}
	return nil
}

// startWatchers launches the watch-method goroutine plus the timeout checker
// for an app. Safe to call before the app is running: each watcher idles while
// Status != running. Must be called exactly once per app.
func (w *Watchdog) startWatchers(wa *WatchedApp) {
	switch wa.Config.WatchMethod {
	case WatchUDP:
		go w.listenHeartbeatUDP(wa)
	case WatchProcess:
		go w.watchProcess(wa)
	case WatchFile:
		go w.watchFile(wa)
	case WatchHTTP:
		go w.watchHTTP(wa)
	case WatchWindow:
		go w.watchWindow(wa)
	default:
		log.Printf("[%s] Unknown watch method %q, falling back to process", wa.Config.ID, wa.Config.WatchMethod)
		go w.watchProcess(wa)
	}

	go w.watchTimeout(wa)
}

func (w *Watchdog) stopApp(id string) {
	w.mu.Lock()
	wa, ok := w.apps[id]
	if !ok {
		w.mu.Unlock()
		return
	}
	delete(w.apps, id)
	w.mu.Unlock()

	wa.mu.Lock()
	if !wa.stopped {
		wa.stopped = true
		close(wa.stopCh)
	}
	pid := wa.PID
	cfg := wa.Config
	if wa.udpConn != nil {
		wa.udpConn.Close()
	}
	wa.mu.Unlock()

	if pid > 0 {
		log.Printf("[%s] Stopping PID %d ...", id, pid)
		stopProcess(cfg, pid)
	}
}

// setAppEnabled turns an app's monitoring on or off, persisting the flag and
// then starting or stopping the app (honoring its stop_mode). This is the single
// place that behaviour lives: the Web UI toggle and the tray menu both call it.
func (w *Watchdog) setAppEnabled(id string, enable bool) (bool, error) {
	w.mu.Lock()
	idx := -1
	for i, a := range w.config.Apps {
		if a.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		w.mu.Unlock()
		return false, fmt.Errorf("app %q not found", id)
	}
	if w.config.Apps[idx].Enabled == enable {
		w.mu.Unlock()
		return enable, nil
	}
	w.config.Apps[idx].Enabled = enable
	cfg := w.config.Apps[idx]
	w.saveConfig()
	w.mu.Unlock()

	if !enable {
		w.stopApp(id)
		log.Printf("[%s] Disabled", id)
		return false, nil
	}
	if err := w.addAndStart(cfg); err != nil {
		log.Printf("[%s] Enable failed: %v", id, err)
		return true, err
	}
	log.Printf("[%s] Enabled", id)
	return true, nil
}

// startAppNow launches an app that is registered but not running — typically one
// waiting for its schedule window. It leaves the enabled flag alone; a disabled
// app is enabled instead, which starts it.
func (w *Watchdog) startAppNow(id string) error {
	w.mu.RLock()
	wa, registered := w.apps[id]
	w.mu.RUnlock()

	if !registered {
		_, err := w.setAppEnabled(id, true)
		return err
	}

	wa.mu.Lock()
	status := wa.Status
	wa.mu.Unlock()
	if status == StatusRunning || status == StatusStarting {
		return nil
	}
	log.Printf("[%s] Manual start", id)
	return w.startApp(wa)
}

// restartAppNow stops an app (saving first if its stop_mode says so) and starts
// it again.
func (w *Watchdog) restartAppNow(id string) error {
	w.mu.RLock()
	_, registered := w.apps[id]
	w.mu.RUnlock()

	if registered {
		log.Printf("[%s] Manual restart", id)
		if _, err := w.setAppEnabled(id, false); err != nil {
			return err
		}
		time.Sleep(1 * time.Second)
	}
	_, err := w.setAppEnabled(id, true)
	return err
}

func (w *Watchdog) startAll() {
	// Collect enabled apps and sort by start_order.
	enabled := make([]AppConfig, 0)
	for _, cfg := range w.config.Apps {
		if !cfg.Enabled {
			log.Printf("[%s] Skipped (disabled)", cfg.ID)
			continue
		}
		enabled = append(enabled, cfg)
	}
	sort.Slice(enabled, func(i, j int) bool {
		return enabled[i].StartOrder < enabled[j].StartOrder
	})

	// Launch in order, respecting start_delay_sec.
	for i, cfg := range enabled {
		if i > 0 && cfg.StartDelaySec > 0 {
			log.Printf("[%s] Waiting %ds before start ...", cfg.ID, cfg.StartDelaySec)
			time.Sleep(time.Duration(cfg.StartDelaySec) * time.Second)
		}
		if err := w.addAndStart(cfg); err != nil {
			log.Printf("[%s] Failed to start: %v", cfg.ID, err)
		}
	}
}

func (w *Watchdog) stopAll() {
	w.mu.RLock()
	ids := make([]string, 0, len(w.apps))
	for id := range w.apps {
		ids = append(ids, id)
	}
	w.mu.RUnlock()
	for _, id := range ids {
		w.stopApp(id)
	}
}

// ---------------------------------------------------------------------------
// Scheduled network commands: UDP / OSC / PJLINK
// ---------------------------------------------------------------------------

// sendUDPRaw sends a raw UDP payload (text, or hex-decoded when isHex).
func sendUDPRaw(host string, port int, payload string, isHex bool) error {
	var data []byte
	if isHex {
		clean := strings.NewReplacer(" ", "", "\t", "", "\r", "", "\n", "").Replace(payload)
		b, err := hex.DecodeString(clean)
		if err != nil {
			return fmt.Errorf("invalid hex payload: %w", err)
		}
		data = b
	} else {
		data = []byte(payload)
	}
	raddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", host, port))
	if err != nil {
		return err
	}
	conn, err := net.DialUDP("udp", nil, raddr)
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write(data)
	return err
}

// sendOSCMessage builds and sends an OSC message with auto-typed arguments
// (integer -> i, float -> f, otherwise string).
func sendOSCMessage(host string, port int, addr, argStr string) error {
	if addr == "" {
		return fmt.Errorf("OSC address is empty")
	}
	tags := ","
	var argBytes []byte
	for _, a := range strings.Fields(argStr) {
		if iv, err := strconv.ParseInt(a, 10, 32); err == nil {
			tags += "i"
			var b [4]byte
			binary.BigEndian.PutUint32(b[:], uint32(int32(iv)))
			argBytes = append(argBytes, b[:]...)
		} else if fv, err := strconv.ParseFloat(a, 32); err == nil {
			tags += "f"
			var b [4]byte
			binary.BigEndian.PutUint32(b[:], math.Float32bits(float32(fv)))
			argBytes = append(argBytes, b[:]...)
		} else {
			tags += "s"
			argBytes = append(argBytes, oscPad(a)...)
		}
	}
	msg := append(oscPad(addr), oscPad(tags)...)
	msg = append(msg, argBytes...)

	raddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", host, port))
	if err != nil {
		return err
	}
	conn, err := net.DialUDP("udp", nil, raddr)
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write(msg)
	return err
}

// sendPJLink connects to a PJLink (class 1) device over TCP, performs the
// greeting / optional MD5 authentication handshake, sends the command and
// returns the device's response line.
func sendPJLink(host string, port int, password, command string) (string, error) {
	if port == 0 {
		port = 4352
	}
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, port), 5*time.Second)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	reader := bufio.NewReader(conn)
	greeting, err := reader.ReadString('\r')
	if err != nil {
		return "", fmt.Errorf("read greeting: %w", err)
	}
	var authPrefix string
	fields := strings.Fields(strings.TrimRight(greeting, "\r\n"))
	if len(fields) >= 2 && strings.EqualFold(fields[0], "PJLINK") {
		if fields[1] == "1" { // authentication required
			if len(fields) < 3 {
				return "", fmt.Errorf("auth required but no nonce in greeting")
			}
			sum := md5.Sum([]byte(fields[2] + password))
			authPrefix = hex.EncodeToString(sum[:])
		}
	}

	cmd := strings.TrimSpace(command)
	if !strings.HasPrefix(cmd, "%") {
		cmd = "%1" + cmd // class-1 prefix
	}
	if _, err := conn.Write([]byte(authPrefix + cmd + "\r")); err != nil {
		return "", err
	}
	resp, err := reader.ReadString('\r')
	if err != nil {
		return strings.TrimRight(resp, "\r\n"), fmt.Errorf("read response: %w", err)
	}
	return strings.TrimRight(resp, "\r\n"), nil
}

// fireCommand executes a command and returns a human-readable result string.
func fireCommand(cmd CommandConfig) (string, error) {
	switch cmd.Type {
	case "udp":
		if err := sendUDPRaw(cmd.Host, cmd.Port, cmd.Payload, cmd.PayloadHex); err != nil {
			return "", err
		}
		return fmt.Sprintf("UDP sent to %s:%d", cmd.Host, cmd.Port), nil
	case "osc":
		if err := sendOSCMessage(cmd.Host, cmd.Port, cmd.OSCAddr, cmd.OSCArgs); err != nil {
			return "", err
		}
		return fmt.Sprintf("OSC %s sent to %s:%d", cmd.OSCAddr, cmd.Host, cmd.Port), nil
	case "pjlink":
		resp, err := sendPJLink(cmd.Host, cmd.Port, cmd.PJPassword, cmd.PJCommand)
		if err != nil {
			return resp, err
		}
		return "PJLINK response: " + resp, nil
	default:
		return "", fmt.Errorf("unknown command type %q", cmd.Type)
	}
}

// watchCommands fires enabled scheduled commands at their configured time/days.
func (w *Watchdog) watchCommands() {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()

	lastFired := make(map[string]string) // id -> "2006-01-02 15:04"

	for {
		select {
		case <-w.shutdownCh:
			return
		case <-ticker.C:
			now := time.Now()
			stamp := now.Format("2006-01-02 15:04")

			w.mu.RLock()
			cmds := make([]CommandConfig, len(w.config.Commands))
			copy(cmds, w.config.Commands)
			w.mu.RUnlock()

			for _, c := range cmds {
				if !c.Enabled || c.Time == "" {
					continue
				}
				ch, cm, err := parseHHMM(c.Time)
				if err != nil || now.Hour() != ch || now.Minute() != cm {
					continue
				}
				if !isDayMatch(c.Days, now) || lastFired[c.ID] == stamp {
					continue
				}
				lastFired[c.ID] = stamp
				go func(cmd CommandConfig) {
					if res, err := fireCommand(cmd); err != nil {
						log.Printf("[cmd:%s] fire failed: %v", cmd.ID, err)
					} else {
						log.Printf("[cmd:%s] fired (%s): %s", cmd.ID, cmd.Type, res)
					}
				}(c)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Web UI – HTTP handlers
// ---------------------------------------------------------------------------

type AppStatusView struct {
	ID            string
	Name          string
	ExePath       string
	Args          []string
	UseShellOpen  bool
	WatchMethod   string
	WatchConfig   WatchConfig
	TimeoutSec    int
	Enabled       bool
	AutoStart     bool
	StartOrder    int
	StartDelaySec int
	Schedule      *ScheduleConfig
	StopMode      string
	OSCHost       string
	OSCPort       int
	OSCSaveAddr   string
	OSCQuitAddr   string
	PID           int
	Status        string
	LastHeartbeat string
	StartedAt     string
	// Restarts is the number of consecutive automatic restarts; a climbing
	// number is the signal that recovery is not working.
	Restarts int
}

// watchMethodLabel returns a human-readable description of the watch method.
func watchMethodLabel(method string) string {
	switch method {
	case WatchUDP:
		return "UDP"
	case WatchProcess:
		return "Process"
	case WatchFile:
		return "File"
	case WatchHTTP:
		return "HTTP"
	case WatchWindow:
		return "Window"
	default:
		return method
	}
}

// watchDetail returns method-specific detail string for display.
func watchDetail(cfg AppConfig) string {
	switch cfg.WatchMethod {
	case WatchUDP:
		return fmt.Sprintf(":%d", cfg.WatchConfig.UDPPort)
	case WatchFile:
		return cfg.WatchConfig.FilePath
	case WatchHTTP:
		return cfg.WatchConfig.URL
	case WatchWindow:
		return fmt.Sprintf("%q", cfg.WatchConfig.WindowTitle)
	case WatchProcess:
		return "PID check"
	default:
		return "-"
	}
}

func (w *Watchdog) getStatusViews() []AppStatusView {
	w.mu.RLock()
	defer w.mu.RUnlock()

	views := make([]AppStatusView, 0, len(w.config.Apps))
	for _, cfg := range w.config.Apps {
		v := AppStatusView{
			ID:            cfg.ID,
			Name:          cfg.Name,
			ExePath:       cfg.ExePath,
			Args:          cfg.Args,
			UseShellOpen:  cfg.UseShellOpen,
			WatchMethod:   cfg.WatchMethod,
			WatchConfig:   cfg.WatchConfig,
			TimeoutSec:    cfg.TimeoutSec,
			Enabled:       cfg.Enabled,
			AutoStart:     cfg.AutoStart,
			StartOrder:    cfg.StartOrder,
			StartDelaySec: cfg.StartDelaySec,
			Schedule:      cfg.Schedule,
			StopMode:      cfg.StopMode,
			OSCHost:       cfg.OSCHost,
			OSCPort:       cfg.OSCPort,
			OSCSaveAddr:   cfg.OSCSaveAddr,
			OSCQuitAddr:   cfg.OSCQuitAddr,
			Status:        "disabled",
		}
		if !cfg.Enabled {
			views = append(views, v)
			continue
		}
		v.Status = "stopped"
		if wa, ok := w.apps[cfg.ID]; ok {
			wa.mu.Lock()
			v.PID = wa.PID
			v.Status = string(wa.Status)
			v.Restarts = wa.restarts
			if !wa.LastHeartbeat.IsZero() {
				v.LastHeartbeat = wa.LastHeartbeat.Format("2006-01-02 15:04:05")
			}
			if !wa.StartedAt.IsZero() {
				v.StartedAt = wa.StartedAt.Format("2006-01-02 15:04:05")
			}
			wa.mu.Unlock()
		}
		views = append(views, v)
	}
	return views
}

func (w *Watchdog) handleIndex(rw http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(rw, r)
		return
	}
	data := struct{ Apps []AppStatusView }{Apps: w.getStatusViews()}
	w.templates.Execute(rw, data)
}

// settingsView is everything the Web UI shows under "global settings",
// including the Windows autostart state, which lives in the registry.
func (w *Watchdog) settingsView() map[string]interface{} {
	autoStart, registered := autoStartState()
	w.mu.RLock()
	defer w.mu.RUnlock()
	return map[string]interface{}{
		"log_dir":           w.config.LogDir,
		"reboot_time":       w.config.RebootTime,
		"reboot_days":       w.config.RebootDays,
		"web_port":          w.config.WebPort,
		"tray":              !w.config.DisableTray,
		"autostart":         autoStart,
		"autostart_command": registered,
	}
}

func (w *Watchdog) handleAPIStatus(rw http.ResponseWriter, r *http.Request) {
	w.mu.RLock()
	cmds := make([]CommandConfig, len(w.config.Commands))
	copy(cmds, w.config.Commands)
	w.mu.RUnlock()

	rw.Header().Set("Content-Type", "application/json")
	json.NewEncoder(rw).Encode(map[string]interface{}{
		"apps":     w.getStatusViews(),
		"settings": w.settingsView(),
		"commands": cmds,
	})
}

func (w *Watchdog) handleAddApp(rw http.ResponseWriter, r *http.Request) {
	var cfg AppConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	if cfg.ID == "" || cfg.ExePath == "" || cfg.TimeoutSec == 0 {
		http.Error(rw, "id, exe_path, timeout_sec are required", http.StatusBadRequest)
		return
	}
	if cfg.WatchMethod == "" {
		cfg.WatchMethod = WatchProcess
	}
	cfg.Enabled = true
	// AutoStart defaults come from the JSON payload; no override needed.

	w.mu.RLock()
	for _, a := range w.config.Apps {
		if a.ID == cfg.ID {
			w.mu.RUnlock()
			http.Error(rw, "duplicate id", http.StatusConflict)
			return
		}
	}
	w.mu.RUnlock()

	w.mu.Lock()
	w.config.Apps = append(w.config.Apps, cfg)
	w.saveConfig()
	w.mu.Unlock()

	if err := w.addAndStart(cfg); err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}

	rw.WriteHeader(http.StatusCreated)
}

func (w *Watchdog) handleEditApp(rw http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/app/")
	if id == "" {
		http.Error(rw, "missing id", http.StatusBadRequest)
		return
	}

	var cfg AppConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	cfg.ID = id

	w.stopApp(id)

	w.mu.Lock()
	found := false
	for i, a := range w.config.Apps {
		if a.ID == id {
			cfg.Enabled = a.Enabled // preserve toggle state
			w.config.Apps[i] = cfg
			found = true
			break
		}
	}
	if !found {
		w.mu.Unlock()
		http.Error(rw, "not found", http.StatusNotFound)
		return
	}
	w.saveConfig()
	w.mu.Unlock()

	if cfg.Enabled {
		if err := w.addAndStart(cfg); err != nil {
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	rw.WriteHeader(http.StatusOK)
}

func (w *Watchdog) handleDeleteApp(rw http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/app/")
	if id == "" {
		http.Error(rw, "missing id", http.StatusBadRequest)
		return
	}

	w.stopApp(id)

	w.mu.Lock()
	newApps := make([]AppConfig, 0, len(w.config.Apps))
	for _, a := range w.config.Apps {
		if a.ID != id {
			newApps = append(newApps, a)
		}
	}
	w.config.Apps = newApps
	w.saveConfig()
	w.mu.Unlock()

	rw.WriteHeader(http.StatusOK)
}

func (w *Watchdog) handleToggleApp(rw http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/app/")
	id = strings.TrimSuffix(id, "/toggle")
	if id == "" {
		http.Error(rw, "missing id", http.StatusBadRequest)
		return
	}

	w.mu.RLock()
	wasEnabled, found := false, false
	for _, a := range w.config.Apps {
		if a.ID == id {
			wasEnabled, found = a.Enabled, true
			break
		}
	}
	w.mu.RUnlock()
	if !found {
		http.Error(rw, "not found", http.StatusNotFound)
		return
	}

	nowEnabled, err := w.setAppEnabled(id, !wasEnabled)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}

	rw.Header().Set("Content-Type", "application/json")
	json.NewEncoder(rw).Encode(map[string]bool{"enabled": nowEnabled})
}

// handlePick opens a native file/folder dialog on the server (= local) machine
// and returns the selected absolute path as JSON {"path": "..."}.
func (w *Watchdog) handlePick(rw http.ResponseWriter, r *http.Request) {
	var path string
	switch r.URL.Query().Get("type") {
	case "folder":
		path = pickFolder("フォルダを選択")
	case "exe":
		path = pickFile("実行ファイルを選択",
			[]string{"Programs (*.exe;*.bat;*.cmd)", "*.exe;*.bat;*.cmd", "All Files (*.*)", "*.*"})
	default:
		path = pickFile("ファイルを選択", []string{"All Files (*.*)", "*.*"})
	}
	rw.Header().Set("Content-Type", "application/json")
	json.NewEncoder(rw).Encode(map[string]string{"path": path})
}

func (w *Watchdog) handleShutdown(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	log.Println("Shutdown requested via Web UI")
	rw.Header().Set("Content-Type", "application/json")
	json.NewEncoder(rw).Encode(map[string]string{"status": "shutting_down"})

	go func() {
		time.Sleep(500 * time.Millisecond) // let the response reach the browser
		w.requestShutdown()
	}()
}

// tailLines reads at most the last n lines of a file, looking at no more than
// the final 256 KiB so a log that grew large stays cheap to display.
func tailLines(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	const maxTail = 256 << 10
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	var offset int64
	if info.Size() > maxTail {
		offset = info.Size() - maxTail
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}

	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if offset > 0 {
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:] // drop the partial first line
		}
	}
	text = strings.Trim(text, "\n")
	if text == "" {
		return []string{}, nil
	}
	lines := strings.Split(text, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}

// handleLogs returns the tail of today's log file, so the Web UI can show what
// the watchdog is actually doing without anyone opening a file on the machine.
func (w *Watchdog) handleLogs(rw http.ResponseWriter, r *http.Request) {
	n := 120
	if v := r.URL.Query().Get("lines"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			n = parsed
		}
	}
	if n > 1000 {
		n = 1000
	}

	var path string
	if w.logWriter != nil {
		path = w.logWriter.CurrentPath()
	}

	out := map[string]interface{}{"file": "", "lines": []string{}}
	rw.Header().Set("Content-Type", "application/json")
	if path == "" {
		json.NewEncoder(rw).Encode(out)
		return
	}
	out["file"] = filepath.Base(path)

	lines, err := tailLines(path, n)
	if err != nil {
		out["error"] = err.Error()
	} else {
		out["lines"] = lines
	}
	json.NewEncoder(rw).Encode(out)
}

// validateConfig performs sanity checks on a config before it is accepted
// (used by the import endpoint).
func validateConfig(c *Config) error {
	seen := make(map[string]bool)
	for i, a := range c.Apps {
		if a.ID == "" {
			return fmt.Errorf("apps[%d]: id is required", i)
		}
		if seen[a.ID] {
			return fmt.Errorf("duplicate app id %q", a.ID)
		}
		seen[a.ID] = true
		if a.ExePath == "" {
			return fmt.Errorf("app %q: exe_path is required", a.ID)
		}
		if a.TimeoutSec <= 0 {
			return fmt.Errorf("app %q: timeout_sec must be > 0", a.ID)
		}
		if a.Schedule != nil {
			if a.Schedule.StartTime != "" {
				if _, _, err := parseHHMM(a.Schedule.StartTime); err != nil {
					return fmt.Errorf("app %q schedule: %v", a.ID, err)
				}
			}
			if a.Schedule.StopTime != "" {
				if _, _, err := parseHHMM(a.Schedule.StopTime); err != nil {
					return fmt.Errorf("app %q schedule: %v", a.ID, err)
				}
			}
		}
	}
	if c.RebootTime != "" {
		if _, _, err := parseHHMM(c.RebootTime); err != nil {
			return fmt.Errorf("reboot_time: %v", err)
		}
	}
	return nil
}

// handleConfigExport streams the full current config as a downloadable JSON
// file, for backup or cloning onto another machine.
func (w *Watchdog) handleConfigExport(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.mu.RLock()
	data, err := json.MarshalIndent(w.config, "", "  ")
	w.mu.RUnlock()
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	filename := "watchdog-config-" + time.Now().Format("2006-01-02") + ".json"
	rw.Header().Set("Content-Type", "application/json")
	rw.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	rw.Write(data)
}

// handleConfigImport replaces the entire config with an uploaded JSON document,
// persists it, and restarts all watched apps. Changes to web_port /
// show_console only take effect after a process restart (the HTTP server and
// console window are already bound).
func (w *Watchdog) handleConfigImport(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20)) // 4 MiB cap
	if err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	var newCfg Config
	if err := json.Unmarshal(body, &newCfg); err != nil {
		http.Error(rw, "invalid config JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateConfig(&newCfg); err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}

	// Stop everything currently running, swap in the new config, persist,
	// then relaunch from the new config.
	w.stopAll()

	w.mu.Lock()
	prevPort := w.config.WebPort
	w.config = newCfg
	saveErr := w.saveConfig()
	w.mu.Unlock()
	if saveErr != nil {
		http.Error(rw, "failed to save config: "+saveErr.Error(), http.StatusInternalServerError)
		return
	}

	w.startAll()

	log.Printf("Config imported: %d app(s)", len(newCfg.Apps))
	rw.Header().Set("Content-Type", "application/json")
	json.NewEncoder(rw).Encode(map[string]interface{}{
		"status":           "imported",
		"apps":             len(newCfg.Apps),
		"restart_required": newCfg.WebPort != 0 && newCfg.WebPort != prevPort,
	})
}

func (w *Watchdog) handleSettings(rw http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		rw.Header().Set("Content-Type", "application/json")
		json.NewEncoder(rw).Encode(w.settingsView())
	case http.MethodPut:
		var payload struct {
			LogDir     string   `json:"log_dir"`
			RebootTime string   `json:"reboot_time"`
			RebootDays []string `json:"reboot_days"`
			// AutoStart is Windows logon autostart, which lives in the registry
			// rather than in config.json. Absent = leave it alone.
			AutoStart *bool `json:"autostart"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(rw, err.Error(), http.StatusBadRequest)
			return
		}
		if payload.RebootTime != "" {
			if _, _, err := parseHHMM(payload.RebootTime); err != nil {
				http.Error(rw, "invalid reboot_time: "+err.Error(), http.StatusBadRequest)
				return
			}
		}
		if payload.AutoStart != nil {
			if err := setAutoStart(*payload.AutoStart); err != nil {
				http.Error(rw, "autostart: "+err.Error(), http.StatusInternalServerError)
				return
			}
			log.Printf("Windows autostart %s", map[bool]string{true: "enabled", false: "disabled"}[*payload.AutoStart])
		}
		w.mu.Lock()
		w.config.LogDir = payload.LogDir
		w.config.RebootTime = payload.RebootTime
		w.config.RebootDays = payload.RebootDays
		w.saveConfig()
		w.mu.Unlock()

		rw.Header().Set("Content-Type", "application/json")
		json.NewEncoder(rw).Encode(w.settingsView())
	default:
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ---------------------------------------------------------------------------
// Scheduled PC reboot
// ---------------------------------------------------------------------------

func (w *Watchdog) watchReboot() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	triggered := false

	for {
		select {
		case <-w.shutdownCh:
			return
		case <-ticker.C:
			w.mu.RLock()
			rebootTime := w.config.RebootTime
			rebootDays := w.config.RebootDays
			w.mu.RUnlock()

			if rebootTime == "" {
				triggered = false
				continue
			}
			rebootH, rebootM, err := parseHHMM(rebootTime)
			if err != nil {
				continue
			}

			now := time.Now()
			if now.Hour() == rebootH && now.Minute() == rebootM {
				if triggered {
					continue
				}
				if !isDayMatch(rebootDays, now) {
					continue
				}
				triggered = true
				log.Printf("[reboot] Reboot time reached. Stopping all apps ...")
				w.stopAll()
				log.Printf("[reboot] All apps stopped. Rebooting PC ...")
				time.Sleep(2 * time.Second)
				cmd := exec.Command("shutdown", "/r", "/t", "0")
				if err := cmd.Run(); err != nil {
					log.Printf("[reboot] shutdown command failed: %v", err)
				}
				return
			} else {
				triggered = false
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Scheduled command handlers
// ---------------------------------------------------------------------------

func (w *Watchdog) handleFireCommand(rw http.ResponseWriter, r *http.Request) {
	var cmd CommandConfig
	if err := json.NewDecoder(r.Body).Decode(&cmd); err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	res, err := fireCommand(cmd)
	rw.Header().Set("Content-Type", "application/json")
	out := map[string]interface{}{"ok": err == nil, "result": res}
	if err != nil {
		out["error"] = err.Error()
	}
	json.NewEncoder(rw).Encode(out)
}

func (w *Watchdog) handleAddCommand(rw http.ResponseWriter, r *http.Request) {
	var cmd CommandConfig
	if err := json.NewDecoder(r.Body).Decode(&cmd); err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	if cmd.ID == "" || cmd.Name == "" || cmd.Type == "" || cmd.Host == "" {
		http.Error(rw, "id, name, type, host are required", http.StatusBadRequest)
		return
	}
	w.mu.Lock()
	for _, c := range w.config.Commands {
		if c.ID == cmd.ID {
			w.mu.Unlock()
			http.Error(rw, "duplicate id", http.StatusConflict)
			return
		}
	}
	w.config.Commands = append(w.config.Commands, cmd)
	w.saveConfig()
	w.mu.Unlock()
	rw.WriteHeader(http.StatusCreated)
}

func (w *Watchdog) handleEditCommand(rw http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/command/")
	if id == "" {
		http.Error(rw, "missing id", http.StatusBadRequest)
		return
	}
	var cmd CommandConfig
	if err := json.NewDecoder(r.Body).Decode(&cmd); err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	cmd.ID = id
	w.mu.Lock()
	found := false
	for i, c := range w.config.Commands {
		if c.ID == id {
			w.config.Commands[i] = cmd
			found = true
			break
		}
	}
	if !found {
		w.mu.Unlock()
		http.Error(rw, "not found", http.StatusNotFound)
		return
	}
	w.saveConfig()
	w.mu.Unlock()
	rw.WriteHeader(http.StatusOK)
}

func (w *Watchdog) handleDeleteCommand(rw http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/command/")
	if id == "" {
		http.Error(rw, "missing id", http.StatusBadRequest)
		return
	}
	w.mu.Lock()
	newCmds := make([]CommandConfig, 0, len(w.config.Commands))
	for _, c := range w.config.Commands {
		if c.ID != id {
			newCmds = append(newCmds, c)
		}
	}
	w.config.Commands = newCmds
	w.saveConfig()
	w.mu.Unlock()
	rw.WriteHeader(http.StatusOK)
}

func (w *Watchdog) apiCommandRouter(rw http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		if r.URL.Path == "/api/command/fire" {
			w.handleFireCommand(rw, r)
			return
		}
		if r.URL.Path == "/api/command" || r.URL.Path == "/api/command/" {
			w.handleAddCommand(rw, r)
			return
		}
	case http.MethodPut:
		if strings.HasPrefix(r.URL.Path, "/api/command/") {
			w.handleEditCommand(rw, r)
			return
		}
	case http.MethodDelete:
		if strings.HasPrefix(r.URL.Path, "/api/command/") {
			w.handleDeleteCommand(rw, r)
			return
		}
	}
	http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
}

func (w *Watchdog) apiAppRouter(rw http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		if r.URL.Path == "/api/app" || r.URL.Path == "/api/app/" {
			w.handleAddApp(rw, r)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/toggle") {
			w.handleToggleApp(rw, r)
			return
		}
	case http.MethodPut:
		if strings.HasPrefix(r.URL.Path, "/api/app/") {
			w.handleEditApp(rw, r)
			return
		}
	case http.MethodDelete:
		if strings.HasPrefix(r.URL.Path, "/api/app/") {
			w.handleDeleteApp(rw, r)
			return
		}
	}
	http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

func main() {
	// --wait-pid=N is set when we relaunch ourselves (tray > "restart"): wait for
	// the previous instance to exit so it can release the port and its apps.
	for _, arg := range os.Args[1:] {
		if v, ok := strings.CutPrefix(arg, "--wait-pid="); ok {
			if pid, err := strconv.Atoi(v); err == nil && pid > 0 {
				waitForProcessExit(pid, 30*time.Second)
			}
		}
	}

	exePath, _ := os.Executable()
	baseDir := filepath.Dir(exePath)

	if _, err := os.Stat(filepath.Join(baseDir, "config.json")); err != nil {
		baseDir, _ = os.Getwd()
	}

	configPath := filepath.Join(baseDir, "config.json")
	log.Printf("Watchdog %s", Version)
	log.Printf("Config: %s", configPath)

	wd, err := NewWatchdog(configPath)
	if err != nil {
		log.Fatalf("Init error: %v", err)
	}

	// Setup file logging.
	logDir := wd.config.LogDir
	if logDir == "" {
		logDir = "logs"
	}
	if !filepath.IsAbs(logDir) {
		logDir = filepath.Join(baseDir, logDir)
	}
	logWriter, err := newDateRotatingWriter(logDir, wd.config.ShowConsole)
	if err != nil {
		log.Fatalf("Log init error: %v", err)
	}
	defer logWriter.Close()
	wd.logWriter = logWriter
	log.SetOutput(logWriter)
	// Repeat these into the file: the lines above went to stderr, which nobody
	// sees when Watchdog runs without a console — and the first question about a
	// site problem is always "which version is running?".
	log.Printf("Watchdog %s starting (config: %s)", Version, configPath)

	if !wd.config.ShowConsole {
		hideConsoleWindow()
	}

	// A second instance for the same config would fight the first one over the
	// apps and the port. Treat launching it as "show me the dashboard" — which is
	// what someone double-clicking watchdog.exe again actually wants.
	if _, first := acquireSingleInstance(configPath); !first {
		log.Printf("Another Watchdog is already running for this config; opening its Web UI instead")
		if err := openURL(fmt.Sprintf("http://localhost:%d/", wd.webPort())); err != nil {
			log.Printf("could not open the Web UI: %v", err)
		}
		return
	}

	if !wd.config.DisableTray {
		go wd.runTray()
	}

	wd.startAll()
	go wd.watchReboot()
	go wd.watchCommands()

	mux := http.NewServeMux()
	mux.HandleFunc("/", wd.handleIndex)
	mux.HandleFunc("/api/status", wd.handleAPIStatus)
	mux.HandleFunc("/api/logs", wd.handleLogs)
	mux.HandleFunc("/api/settings", wd.handleSettings)
	mux.HandleFunc("/api/config/export", wd.handleConfigExport)
	mux.HandleFunc("/api/config/import", wd.handleConfigImport)
	mux.HandleFunc("/api/app", wd.apiAppRouter)
	mux.HandleFunc("/api/app/", wd.apiAppRouter)
	mux.HandleFunc("/api/pick", wd.handlePick)
	mux.HandleFunc("/api/command", wd.apiCommandRouter)
	mux.HandleFunc("/api/command/", wd.apiCommandRouter)
	mux.HandleFunc("/api/shutdown", wd.handleShutdown)

	addr := fmt.Sprintf(":%d", wd.config.WebPort)
	wd.httpServer = &http.Server{Addr: addr, Handler: mux}

	log.Printf("Web UI: http://localhost%s", addr)

	go func() {
		if err := wd.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	<-wd.shutdownCh
	log.Println("Shutting down ...")
	wd.stopAll()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wd.httpServer.Shutdown(ctx)
	log.Println("Watchdog stopped.")
}
