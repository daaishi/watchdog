package main

// Slack notifications for problems.
//
// The point of these messages is that nobody watches the dashboard: an
// installation can sit in a restart loop for a whole day before anyone notices.
// So the rules are shaped by that reality:
//
//   - Throttled per app and per event. A loop that restarts every 65 seconds
//     would otherwise post hundreds of messages; instead one message goes out
//     and the next one reports how many were suppressed.
//   - A problem is followed by a recovery message, so a thread of alerts always
//     ends with "it is running again" instead of leaving people guessing.
//   - Every message names the site and the machine, so one channel can receive
//     alerts from several installations.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// notifyEvent is a kind of message; each one can be switched off individually
// and each one throttles separately.
type notifyEvent string

const (
	evRestart      notifyEvent = "restart"
	evStuck        notifyEvent = "stuck"
	evLaunchError  notifyEvent = "launch_error"
	evRecovered    notifyEvent = "recovered"
	evWatchdogLife notifyEvent = "watchdog_life"
	evSchedule     notifyEvent = "schedule"
	evReboot       notifyEvent = "reboot"
)

// NotifyConfig is the "notify" block of config.json.
type NotifyConfig struct {
	SlackWebhookURL string `json:"slack_webhook_url,omitempty"`
	// SiteName is the title of every message — the project or site this
	// installation belongs to. The hostname alone does not tell anyone which job
	// an alert came from, so this is the line people actually read first.
	SiteName string `json:"site_name,omitempty"`
	// MinIntervalSec is the per-app, per-event quiet period. 0 = default.
	MinIntervalSec int  `json:"min_interval_sec,omitempty"`
	OnRestart      bool `json:"on_restart"`
	OnStuck        bool `json:"on_stuck"`
	OnLaunchError  bool `json:"on_launch_error"`
	OnRecovered    bool `json:"on_recovered"`
	OnWatchdogLife bool `json:"on_watchdog_life"`
	OnSchedule     bool `json:"on_schedule"`
	OnReboot       bool `json:"on_reboot"`
}

// defaultNotifyConfig is what an install with no "notify" block behaves like, so
// that pasting a webhook URL into the UI is enough to start getting the alerts
// that matter.
func defaultNotifyConfig() NotifyConfig {
	return NotifyConfig{
		MinIntervalSec: 300,
		OnRestart:      true,
		OnStuck:        true,
		OnLaunchError:  true,
		OnRecovered:    true,
		OnReboot:       true,
	}
}

func (n NotifyConfig) enabledFor(ev notifyEvent) bool {
	switch ev {
	case evRestart:
		return n.OnRestart
	case evStuck:
		return n.OnStuck
	case evLaunchError:
		return n.OnLaunchError
	case evRecovered:
		return n.OnRecovered
	case evWatchdogLife:
		return n.OnWatchdogLife
	case evSchedule:
		return n.OnSchedule
	case evReboot:
		return n.OnReboot
	}
	return false
}

func (n NotifyConfig) interval() time.Duration {
	if n.MinIntervalSec <= 0 {
		return 300 * time.Second
	}
	return time.Duration(n.MinIntervalSec) * time.Second
}

// notifyConfig returns the effective settings (defaults when the block is absent).
//
// It deliberately does NOT take w.mu: notifications are raised from paths that
// already hold that lock — a failed launch happens inside addAndStart — and
// sync.RWMutex is not reentrant, so reading the config through it would deadlock.
// The settings are kept in an atomic snapshot instead, refreshed by
// setNotifyConfig whenever the configuration changes.
func (w *Watchdog) notifyConfig() NotifyConfig {
	if p := w.notifyCfg.Load(); p != nil {
		return *p
	}
	return defaultNotifyConfig()
}

// setNotifyConfig publishes the notification settings for notifyConfig to read.
// Call it after anything that changes config.Notify.
func (w *Watchdog) setNotifyConfig(nc *NotifyConfig) {
	if nc == nil {
		d := defaultNotifyConfig()
		w.notifyCfg.Store(&d)
		return
	}
	copied := *nc
	w.notifyCfg.Store(&copied)
}

// ---------------------------------------------------------------------------
// throttling and problem tracking
// ---------------------------------------------------------------------------

// recoveryDelay is how long an app must look healthy before a problem is
// declared over — long enough that a restart loop's brief "running" moments
// don't produce a stream of false all-clears.
const recoveryDelay = 90 * time.Second

type notifier struct {
	mu         sync.Mutex
	lastSent   map[string]time.Time // event|app -> last delivery
	suppressed map[string]int       // event|app -> messages skipped since then
	problemAt  map[string]time.Time // app -> when a problem was last reported
}

func newNotifier() *notifier {
	return &notifier{
		lastSent:   make(map[string]time.Time),
		suppressed: make(map[string]int),
		problemAt:  make(map[string]time.Time),
	}
}

// allow reports whether this message should go out now. When it should not, the
// suppressed counter for that key advances instead. The returned count is how
// many were suppressed since the previous delivery.
func (n *notifier) allow(ev notifyEvent, appID string, min time.Duration) (bool, int) {
	key := string(ev) + "|" + appID
	n.mu.Lock()
	defer n.mu.Unlock()

	last, seen := n.lastSent[key]
	if seen && time.Since(last) < min {
		n.suppressed[key]++
		return false, 0
	}
	skipped := n.suppressed[key]
	n.suppressed[key] = 0
	n.lastSent[key] = time.Now()
	return true, skipped
}

// markProblem records that an app is in a bad state, which is what makes a later
// recovery message meaningful.
func (n *notifier) markProblem(appID string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, ongoing := n.problemAt[appID]; !ongoing {
		n.problemAt[appID] = time.Now()
	}
}

// clearProblem reports whether an app was in a bad state long enough ago that
// its return to health is worth announcing, and clears the state either way.
func (n *notifier) clearProblem(appID string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	since, ongoing := n.problemAt[appID]
	if !ongoing {
		return false
	}
	if time.Since(since) < recoveryDelay {
		return false // too soon to call it recovered; keep watching
	}
	delete(n.problemAt, appID)
	return true
}

// ---------------------------------------------------------------------------
// sending
// ---------------------------------------------------------------------------

var notifyClient = &http.Client{Timeout: 10 * time.Second}

// postSlack delivers one message to an Incoming Webhook. title, when set, is
// also sent as the sender name: workspaces that allow webhooks to override it
// then show which installation posted without anyone reading the message body.
// Slack ignores the field where overrides are not permitted, so it is safe.
func postSlack(webhookURL, text, title string) error {
	payload := map[string]string{"text": text}
	if title != "" {
		payload["username"] = title
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, webhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := notifyClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		buf := make([]byte, 256)
		n, _ := resp.Body.Read(buf)
		return fmt.Errorf("slack returned %s: %s", resp.Status, strings.TrimSpace(string(buf[:n])))
	}
	return nil
}

var notifyIcons = map[notifyEvent]string{
	evRestart:      ":warning:",
	evStuck:        ":rotating_light:",
	evLaunchError:  ":x:",
	evRecovered:    ":white_check_mark:",
	evWatchdogLife: ":information_source:",
	evSchedule:     ":clock3:",
	evReboot:       ":arrows_counterclockwise:",
}

// notify sends one Slack message about an app (appID may be "" for messages
// about Watchdog itself). It returns immediately; delivery happens in the
// background so no watcher is ever delayed by the network.
//
// headline is one line; detail may be empty.
func (w *Watchdog) notify(ev notifyEvent, appID, headline, detail string) {
	w.notifySend(ev, appID, headline, detail, false)
}

// notifySync delivers before returning. Used on the paths where the process (or
// the PC) is about to go away and a backgrounded send would never happen.
func (w *Watchdog) notifySync(ev notifyEvent, appID, headline, detail string) {
	w.notifySend(ev, appID, headline, detail, true)
}

func (w *Watchdog) notifySend(ev notifyEvent, appID, headline, detail string, wait bool) {
	cfg := w.notifyConfig()
	if cfg.SlackWebhookURL == "" || !cfg.enabledFor(ev) {
		return
	}
	ok, skipped := w.notifier.allow(ev, appID, cfg.interval())
	if !ok {
		return
	}

	var b strings.Builder
	// The title goes on its own bold line: in a channel that receives several
	// installations, that is what makes an alert identifiable at a glance.
	if cfg.SiteName != "" {
		fmt.Fprintf(&b, "*%s*\n", cfg.SiteName)
	}
	fmt.Fprintf(&b, "%s %s", notifyIcons[ev], headline)
	if detail != "" {
		fmt.Fprintf(&b, "\n%s", detail)
	}
	if skipped > 0 {
		fmt.Fprintf(&b, "\n（直前の%d分間に同じ通知 %d 件を省略）", int(cfg.interval().Minutes()), skipped)
	}
	host, _ := os.Hostname()
	fmt.Fprintf(&b, "\n_Watchdog %s / %s / %s_", Version, host, time.Now().Format("2006-01-02 15:04:05"))

	text := b.String()
	send := func() {
		if err := postSlack(cfg.SlackWebhookURL, text, cfg.SiteName); err != nil {
			// Never log the URL itself — it is a secret.
			log.Printf("[notify] Slack send failed (%s): %v", ev, err)
		}
	}
	if wait {
		send()
		return
	}
	go send()
}

// notifyProblem is notify plus "remember that this app is unhealthy", so the
// recovery message later has something to close.
func (w *Watchdog) notifyProblem(ev notifyEvent, appID, headline, detail string) {
	w.notifier.markProblem(appID)
	w.notify(ev, appID, headline, detail)
}

// notifyRecovered announces that an app that had a problem is running normally
// again. It is a no-op unless a problem was reported and enough time has passed.
func (w *Watchdog) notifyRecovered(appID, name string) {
	if !w.notifier.clearProblem(appID) {
		return
	}
	w.notify(evRecovered, appID, fmt.Sprintf("*%s* は正常に稼働しています", name),
		"直前の異常から復旧しました。")
}

// sendTestNotification posts a sample message, using the supplied webhook URL and
// title or the saved ones. Used by the "test" button in the Web UI, which passes
// what is currently typed in the form so it can be checked before saving.
func (w *Watchdog) sendTestNotification(webhookURL, title string) error {
	cfg := w.notifyConfig()
	if webhookURL == "" {
		webhookURL = cfg.SlackWebhookURL
	}
	if webhookURL == "" {
		return fmt.Errorf("Webhook URL が設定されていません")
	}
	if title == "" {
		title = cfg.SiteName
	}
	host, _ := os.Hostname()

	var b strings.Builder
	if title != "" {
		fmt.Fprintf(&b, "*%s*\n", title)
	}
	fmt.Fprintf(&b, ":white_check_mark: Watchdog のテスト通知です\n通知はこの形式で届きます。")
	if title == "" {
		fmt.Fprintf(&b, "\n（通知タイトルが未設定です。設定するとこの上に案件名が入ります）")
	}
	fmt.Fprintf(&b, "\n_Watchdog %s / %s / %s_", Version, host, time.Now().Format("2006-01-02 15:04:05"))

	return postSlack(webhookURL, b.String(), title)
}

// maskWebhook renders a webhook URL safe to show in the UI: enough to recognise
// which one is configured, not enough to reuse.
func maskWebhook(url string) string {
	if url == "" {
		return ""
	}
	const keep = 6
	if len(url) <= keep+4 {
		return strings.Repeat("*", len(url))
	}
	head := len("https://hooks.slack.com/services/")
	if head > len(url)-keep {
		head = len(url) - keep
	}
	return url[:head] + "…" + url[len(url)-keep:]
}
