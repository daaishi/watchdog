package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
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

//go:embed templates/index.html
var embeddedTemplate string

// ---------------------------------------------------------------------------
// Windows API for window-title monitoring
// ---------------------------------------------------------------------------

var (
	user32           = syscall.NewLazyDLL("user32.dll")
	procFindWindowW  = user32.NewProc("FindWindowW")
	procEnumWindows  = user32.NewProc("EnumWindows")
	procGetWindowTextW = user32.NewProc("GetWindowTextW")
)

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

type AppConfig struct {
	ID            string      `json:"id"`
	Name          string      `json:"name"`
	ExePath       string      `json:"exe_path"`
	Args          []string    `json:"args"`
	UseShellOpen  bool        `json:"use_shell_open"`
	WatchMethod   string      `json:"watch_method"`
	WatchConfig   WatchConfig `json:"watch_config"`
	TimeoutSec    int         `json:"timeout_sec"`
	Enabled       bool        `json:"enabled"`
	AutoStart     bool        `json:"auto_start"`
	StartOrder    int         `json:"start_order"`
	StartDelaySec int         `json:"start_delay_sec"`
}

type Config struct {
	WebPort int         `json:"web_port"`
	Apps    []AppConfig `json:"apps"`
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
	cmd     *exec.Cmd
	udpConn *net.UDPConn
	stopCh  chan struct{}
	stopped bool
}

// ---------------------------------------------------------------------------
// Watchdog – central coordinator
// ---------------------------------------------------------------------------

type Watchdog struct {
	configPath string
	config     Config
	apps       map[string]*WatchedApp
	mu         sync.RWMutex
	templates  *template.Template
	httpServer *http.Server
	shutdownCh chan struct{}
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

func launchApp(cfg AppConfig) (*exec.Cmd, error) {
	if cfg.UseShellOpen {
		args := []string{"/C", "start", "/B", ""}
		args = append(args, cfg.ExePath)
		args = append(args, cfg.Args...)
		cmd := exec.Command("cmd", args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		return cmd, nil
	}

	cmd := exec.Command(cfg.ExePath, cfg.Args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

func findPIDByExeAndArgs(exePath string, args []string) (int, error) {
	exeName := filepath.Base(exePath)
	cmd := exec.Command("wmic", "process", "where",
		fmt.Sprintf("name='%s'", exeName),
		"get", "ProcessId,CommandLine", "/FORMAT:CSV")
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("wmic: %w", err)
	}

	var marker string
	if len(args) > 0 {
		marker = args[0]
	}

	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Node") {
			continue
		}
		if marker != "" && !strings.Contains(line, marker) {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) < 2 {
			continue
		}
		pidStr := strings.TrimSpace(parts[len(parts)-1])
		pid, err := strconv.Atoi(pidStr)
		if err != nil {
			continue
		}
		if pid > 0 {
			return pid, nil
		}
	}
	return 0, fmt.Errorf("process not found for %s with arg %q", exeName, marker)
}

func killPID(pid int) error {
	cmd := exec.Command("taskkill", "/F", "/PID", strconv.Itoa(pid))
	return cmd.Run()
}

// isProcessAlive checks whether a PID is still running using the Windows
// OpenProcess API. This is more reliable than parsing tasklist output which
// can vary by locale.
func isProcessAlive(pid int) bool {
	const PROCESS_QUERY_LIMITED_INFORMATION = 0x1000
	h, err := syscall.OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	syscall.CloseHandle(h)
	return true
}

// ---------------------------------------------------------------------------
// Per-app lifecycle
// ---------------------------------------------------------------------------

func (w *Watchdog) startApp(wa *WatchedApp) error {
	wa.mu.Lock()
	defer wa.mu.Unlock()

	wa.Status = StatusStarting
	log.Printf("[%s] Starting %s ...", wa.Config.ID, wa.Config.Name)

	cmd, err := launchApp(wa.Config)
	if err != nil {
		wa.Status = StatusStopped
		return fmt.Errorf("launch %s: %w", wa.Config.ID, err)
	}
	wa.cmd = cmd

	if wa.Config.UseShellOpen {
		go func() {
			time.Sleep(5 * time.Second)
			for i := 0; i < 6; i++ {
				pid, err := findPIDByExeAndArgs(wa.Config.ExePath, wa.Config.Args)
				if err == nil && pid > 0 {
					wa.mu.Lock()
					wa.PID = pid
					wa.Status = StatusRunning
					wa.StartedAt = time.Now()
					wa.LastHeartbeat = time.Now()
					wa.mu.Unlock()
					log.Printf("[%s] Discovered PID %d", wa.Config.ID, pid)
					return
				}
				time.Sleep(3 * time.Second)
			}
			log.Printf("[%s] WARNING: could not discover PID via WMIC", wa.Config.ID)
			wa.mu.Lock()
			wa.Status = StatusRunning
			wa.StartedAt = time.Now()
			wa.LastHeartbeat = time.Now()
			wa.mu.Unlock()
		}()
	} else {
		wa.PID = cmd.Process.Pid
		wa.Status = StatusRunning
		wa.StartedAt = time.Now()
		wa.LastHeartbeat = time.Now()
		log.Printf("[%s] Started with PID %d", wa.Config.ID, wa.PID)
	}

	return nil
}

func (w *Watchdog) killAndRestart(wa *WatchedApp) {
	wa.mu.Lock()
	pid := wa.PID
	autoStart := wa.Config.AutoStart
	wa.Status = StatusStopped
	wa.mu.Unlock()

	if pid > 0 {
		log.Printf("[%s] Killing PID %d ...", wa.Config.ID, pid)
		if err := killPID(pid); err != nil {
			log.Printf("[%s] taskkill: %v (process may have already exited)", wa.Config.ID, err)
		}
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

func (w *Watchdog) checkProcessOnce(wa *WatchedApp) {
	wa.mu.Lock()
	pid := wa.PID
	status := wa.Status
	wa.mu.Unlock()

	if status != StatusRunning || pid == 0 {
		return
	}

	if isProcessAlive(pid) {
		wa.mu.Lock()
		wa.LastHeartbeat = time.Now()
		wa.mu.Unlock()
	}
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
// Timeout checker (shared for all watch methods)
// ---------------------------------------------------------------------------

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

			if status != StatusRunning {
				continue
			}

			if time.Since(last) > time.Duration(timeout)*time.Second {
				log.Printf("[%s] Heartbeat timeout (%ds). Restarting ...",
					wa.Config.ID, timeout)
				w.killAndRestart(wa)
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
	// Try with args first (precise match).
	if len(cfg.Args) > 0 {
		pid, err := findPIDByExeAndArgs(cfg.ExePath, cfg.Args)
		if err == nil && pid > 0 {
			return pid
		}
	}
	// Fallback: find any process matching the exe name.
	pid, err := findPIDByExeAndArgs(cfg.ExePath, nil)
	if err == nil && pid > 0 {
		return pid
	}
	return 0
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

	// Launch the appropriate watcher goroutine based on watch_method.
	switch cfg.WatchMethod {
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
		log.Printf("[%s] Unknown watch method %q, falling back to process", cfg.ID, cfg.WatchMethod)
		go w.watchProcess(wa)
	}

	go w.watchTimeout(wa)
	return nil
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
	if wa.udpConn != nil {
		wa.udpConn.Close()
	}
	wa.mu.Unlock()

	if pid > 0 {
		log.Printf("[%s] Stopping PID %d ...", id, pid)
		killPID(pid)
	}
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
// Web UI – HTTP handlers
// ---------------------------------------------------------------------------

type AppStatusView struct {
	ID            string
	Name          string
	ExePath       string
	Args          string
	UseShellOpen  bool
	WatchMethod   string
	WatchConfig   WatchConfig
	TimeoutSec    int
	Enabled       bool
	AutoStart     bool
	StartOrder    int
	StartDelaySec int
	PID           int
	Status        string
	LastHeartbeat string
	StartedAt     string
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
			Args:          strings.Join(cfg.Args, " "),
			UseShellOpen:  cfg.UseShellOpen,
			WatchMethod:   cfg.WatchMethod,
			WatchConfig:   cfg.WatchConfig,
			TimeoutSec:    cfg.TimeoutSec,
			Enabled:       cfg.Enabled,
			AutoStart:     cfg.AutoStart,
			StartOrder:    cfg.StartOrder,
			StartDelaySec: cfg.StartDelaySec,
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

func (w *Watchdog) handleAPIStatus(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("Content-Type", "application/json")
	json.NewEncoder(rw).Encode(w.getStatusViews())
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
		http.Error(rw, "not found", http.StatusNotFound)
		return
	}

	wasEnabled := w.config.Apps[idx].Enabled
	w.config.Apps[idx].Enabled = !wasEnabled
	nowEnabled := w.config.Apps[idx].Enabled
	cfg := w.config.Apps[idx]
	w.saveConfig()
	w.mu.Unlock()

	if wasEnabled && !nowEnabled {
		w.stopApp(id)
		log.Printf("[%s] Disabled", id)
	} else if !wasEnabled && nowEnabled {
		if err := w.addAndStart(cfg); err != nil {
			log.Printf("[%s] Enable failed: %v", id, err)
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}
		log.Printf("[%s] Enabled", id)
	}

	rw.Header().Set("Content-Type", "application/json")
	json.NewEncoder(rw).Encode(map[string]bool{"enabled": nowEnabled})
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
		time.Sleep(500 * time.Millisecond)
		close(w.shutdownCh)
	}()
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
	exePath, _ := os.Executable()
	baseDir := filepath.Dir(exePath)

	if _, err := os.Stat(filepath.Join(baseDir, "config.json")); err != nil {
		baseDir, _ = os.Getwd()
	}

	configPath := filepath.Join(baseDir, "config.json")
	log.Printf("Config: %s", configPath)

	wd, err := NewWatchdog(configPath)
	if err != nil {
		log.Fatalf("Init error: %v", err)
	}

	wd.startAll()

	mux := http.NewServeMux()
	mux.HandleFunc("/", wd.handleIndex)
	mux.HandleFunc("/api/status", wd.handleAPIStatus)
	mux.HandleFunc("/api/app", wd.apiAppRouter)
	mux.HandleFunc("/api/app/", wd.apiAppRouter)
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
