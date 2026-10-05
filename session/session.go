// Package session keeps a named Chrome alive between shell-browser runs, so a
// tab can outlive the command that opened it: the agent drives it in one run,
// a human takes it over in a live view, and the agent picks it up again in the
// next run.
//
// A session is a directory under the sessions root:
//
//	<root>/<name>/profile/            Chrome user-data-dir (cookies, logins)
//	<root>/<name>/profile/DevToolsActivePort   written by Chrome at startup
//	<root>/<name>/target              the tab id the session works in
//	<root>/<name>/lock.json           present while a human holds the tab
//	<root>/<name>/last_used           touched by every run (idle reaping)
//	<root>/<name>/chrome.log          Chrome's stdout/stderr
//
// Chrome is started detached (its own process group) with
// --remote-debugging-port=0 on loopback; the chosen port is read back from
// DevToolsActivePort. Nothing here tracks pids: liveness is "the debug port
// answers".
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	cdpbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// EnvRoot overrides the sessions root. The shell daemon sets it per agent so
// two agents never share a Chrome profile (Chrome locks its user-data-dir).
const EnvRoot = "SHELL_BROWSER_SESSIONS"

// DefaultChromePath is used when no Chrome binary is configured.
const DefaultChromePath = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"

// ErrLocked is returned when a human currently holds the session's tab.
var ErrLocked = errors.New("session is held by a human")

// Root returns the sessions root: $SHELL_BROWSER_SESSIONS, else
// ~/.shell/browser-sessions.
func Root() string {
	if v := os.Getenv(EnvRoot); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "shell-browser-sessions")
	}
	return filepath.Join(home, ".shell", "browser-sessions")
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// ValidName reports whether name is usable as a session name.
func ValidName(name string) bool { return nameRe.MatchString(name) }

// Session is one named, persistent Chrome.
type Session struct {
	Name string
	Dir  string
}

// Open returns the session called name under root, creating its directory.
func Open(root, name string) (*Session, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("invalid session name %q: use letters, digits, '-' or '_' (max 64)", name)
	}
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(dir, "profile"), 0o700); err != nil {
		return nil, fmt.Errorf("create session dir: %w", err)
	}
	return &Session{Name: name, Dir: dir}, nil
}

// List returns every session directory under root, sorted by name.
func List(root string) ([]*Session, error) {
	ents, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*Session
	for _, e := range ents {
		if e.IsDir() && ValidName(e.Name()) {
			out = append(out, &Session{Name: e.Name(), Dir: filepath.Join(root, e.Name())})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Session) profileDir() string   { return filepath.Join(s.Dir, "profile") }
func (s *Session) path(f string) string { return filepath.Join(s.Dir, f) }

// Endpoint is a running session's DevTools address.
type Endpoint struct {
	Port      int
	BrowserWS string // ws://127.0.0.1:<port>/devtools/browser/<id>
}

// HTTPBase is the DevTools HTTP root, e.g. http://127.0.0.1:9222.
func (e *Endpoint) HTTPBase() string { return "http://127.0.0.1:" + strconv.Itoa(e.Port) }

// Running returns the session's endpoint if its Chrome answers, else nil.
func (s *Session) Running(ctx context.Context) *Endpoint {
	b, err := os.ReadFile(filepath.Join(s.profileDir(), "DevToolsActivePort"))
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	port, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil || port <= 0 {
		return nil
	}
	ep := &Endpoint{Port: port}
	var ver struct {
		WS string `json:"webSocketDebuggerUrl"`
	}
	if err := getJSON(ctx, ep.HTTPBase()+"/json/version", &ver); err != nil || ver.WS == "" {
		return nil
	}
	ep.BrowserWS = ver.WS
	return ep
}

// LaunchOptions controls how Ensure starts Chrome when it is not running.
type LaunchOptions struct {
	ChromePath string // default DefaultChromePath
	Headless   bool   // default false: a real window passes bot checks more often
	Width      int    // default 1280
	Height     int    // default 900
}

// Ensure returns the running session's endpoint, starting Chrome if needed.
// Concurrent callers are serialised with a lock file so only one launches.
func (s *Session) Ensure(ctx context.Context, opt LaunchOptions) (*Endpoint, error) {
	if ep := s.Running(ctx); ep != nil {
		return ep, nil
	}
	unlock, err := s.flock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	if ep := s.Running(ctx); ep != nil { // another caller won the race
		return ep, nil
	}

	bin := opt.ChromePath
	if bin == "" {
		bin = DefaultChromePath
	}
	w, h := opt.Width, opt.Height
	if w <= 0 {
		w = 1280
	}
	if h <= 0 {
		h = 900
	}
	_ = os.Remove(filepath.Join(s.profileDir(), "DevToolsActivePort"))
	_ = os.Remove(s.path("target"))

	args := []string{
		"--user-data-dir=" + s.profileDir(),
		"--remote-debugging-port=0",
		"--remote-debugging-address=127.0.0.1",
		"--no-first-run",
		"--no-default-browser-check",
		// The daemon is not in the login session's Keychain context; without
		// these Chrome cannot encrypt cookies and logins do not persist.
		"--use-mock-keychain",
		"--password-store=basic",
		fmt.Sprintf("--window-size=%d,%d", w, h),
		// Keep rendering when the window is covered, so screencast frames
		// keep arriving for a human watching from a phone.
		"--disable-backgrounding-occluded-windows",
		"--disable-renderer-backgrounding",
		"--disable-background-timer-throttling",
		"--disable-blink-features=AutomationControlled",
	}
	if opt.Headless {
		args = append(args, "--headless=new")
	}
	args = append(args, "about:blank")

	logf, err := os.OpenFile(s.path("chrome.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	defer logf.Close()
	cmd := exec.Command(bin, args...)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // outlive the caller
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start chrome: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-exited:
			return nil, fmt.Errorf("chrome exited during startup (%v); see %s", err, s.path("chrome.log"))
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
		if ep := s.Running(ctx); ep != nil {
			s.Touch()
			return ep, nil
		}
	}
	return nil, fmt.Errorf("chrome did not open its debug port within 20s; see %s", s.path("chrome.log"))
}

func (s *Session) flock() (func(), error) {
	f, err := os.OpenFile(s.path(".launch.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// Target is one DevTools target.
type Target struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	URL   string `json:"url"`
	Title string `json:"title"`
}

// Pages lists the session's open tabs.
func (e *Endpoint) Pages(ctx context.Context) ([]Target, error) {
	var all []Target
	if err := getJSON(ctx, e.HTTPBase()+"/json/list", &all); err != nil {
		return nil, err
	}
	var pages []Target
	for _, t := range all {
		if t.Type == "page" && !strings.HasPrefix(t.URL, "chrome://") && !strings.HasPrefix(t.URL, "devtools://") {
			pages = append(pages, t)
		}
	}
	return pages, nil
}

// CurrentTarget returns the tab the session works in: the one last recorded
// if it is still open, else the first open tab, else a new blank tab.
func (s *Session) CurrentTarget(ctx context.Context, ep *Endpoint) (string, error) {
	pages, err := ep.Pages(ctx)
	if err != nil {
		return "", err
	}
	if b, err := os.ReadFile(s.path("target")); err == nil {
		want := strings.TrimSpace(string(b))
		for _, p := range pages {
			if p.ID == want {
				return want, nil
			}
		}
	}
	if len(pages) > 0 {
		_ = s.SetTarget(pages[0].ID)
		return pages[0].ID, nil
	}
	var t Target
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, ep.HTTPBase()+"/json/new?about:blank", nil)
	if err := doJSON(req, &t); err != nil {
		return "", fmt.Errorf("open new tab: %w", err)
	}
	_ = s.SetTarget(t.ID)
	return t.ID, nil
}

// SetTarget records the tab the session works in.
func (s *Session) SetTarget(id string) error {
	return os.WriteFile(s.path("target"), []byte(id+"\n"), 0o600)
}

// Touch records use, for idle reaping.
func (s *Session) Touch() {
	_ = os.WriteFile(s.path("last_used"), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600)
}

// LastUsed is when the session was last driven (zero if never).
func (s *Session) LastUsed() time.Time {
	fi, err := os.Stat(s.path("last_used"))
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

// Lock marks the session's tab as held by a human.
type Lock struct {
	HandoffID int64     `json:"handoff_id"`
	Reason    string    `json:"reason,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
}

// SetLock records that a human holds the tab until l.ExpiresAt.
func (s *Session) SetLock(l Lock) error {
	b, _ := json.Marshal(l)
	return os.WriteFile(s.path("lock.json"), b, 0o600)
}

// ClearLock releases the tab.
func (s *Session) ClearLock() { _ = os.Remove(s.path("lock.json")) }

// HeldBy returns the lock if a human holds the tab now. An expired lock is
// treated as released: the daemon that set it is responsible for clean-up,
// and a dead daemon must not wedge the agent forever.
func (s *Session) HeldBy(now time.Time) (*Lock, bool) {
	b, err := os.ReadFile(s.path("lock.json"))
	if err != nil {
		return nil, false
	}
	var l Lock
	if json.Unmarshal(b, &l) != nil || !now.Before(l.ExpiresAt) {
		return nil, false
	}
	return &l, true
}

// CheckFree returns ErrLocked (wrapped with detail) when a human holds the tab.
func (s *Session) CheckFree(now time.Time) error {
	if l, ok := s.HeldBy(now); ok {
		return fmt.Errorf("%w: handoff #%d is open until %s — wait for the human to finish; you will get a message when they do",
			ErrLocked, l.HandoffID, l.ExpiresAt.Local().Format("15:04"))
	}
	return nil
}

func getJSON(ctx context.Context, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	return doJSON(req, v)
}

var httpClient = &http.Client{Timeout: 3 * time.Second}

func doJSON(req *http.Request, v any) error {
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s %s: HTTP %d", req.Method, req.URL.Path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// Close shuts the session's Chrome down (Browser.close over CDP). The profile
// stays on disk, so the next Ensure starts it again with the same logins.
func Close(ctx context.Context, ep *Endpoint) error {
	allocCtx, cancelAlloc := chromedp.NewRemoteAllocator(ctx, ep.BrowserWS, chromedp.NoModifyURL)
	defer cancelAlloc()
	// Browser-level command: no tab needs attaching. Run it on the browser
	// executor of a context that has connected.
	c, cancel := chromedp.NewContext(allocCtx)
	defer cancel()
	if err := chromedp.Run(c, chromedp.ActionFunc(func(ctx context.Context) error {
		return cdpbrowser.Close().Do(cdp.WithExecutor(ctx, chromedp.FromContext(ctx).Browser))
	})); err != nil && !strings.Contains(err.Error(), "closed") {
		return fmt.Errorf("close chrome: %w", err)
	}
	return nil
}

// Attach connects to one tab of a running session and returns a chromedp
// context for it. release disconnects WITHOUT closing the tab.
//
// Why release is not just the CancelFunc: chromedp treats every context on a
// RemoteAllocator as a non-first tab (chromedp.go NewContext sets first=false),
// so cancelling it sends Target.closeTarget and the human's or agent's tab
// disappears. Clearing Context.Target before cancelling makes chromedp's close
// hook find nothing to close; the websocket still shuts down cleanly. Never
// call chromedp.Cancel on these contexts either (it closes the browser).
func Attach(ctx context.Context, ep *Endpoint, targetID string, opts ...chromedp.ContextOption) (context.Context, func()) {
	allocCtx, cancelAlloc := chromedp.NewRemoteAllocator(ctx, ep.BrowserWS, chromedp.NoModifyURL)
	opts = append([]chromedp.ContextOption{chromedp.WithTargetID(target.ID(targetID))}, opts...)
	tabCtx, cancelTab := chromedp.NewContext(allocCtx, opts...)
	return tabCtx, func() {
		if c := chromedp.FromContext(tabCtx); c != nil {
			c.Target = nil
		}
		cancelTab()
		cancelAlloc()
	}
}
