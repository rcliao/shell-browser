// Package liveview streams one session tab to a person's browser and replays
// their taps, typing and navigation into it. It is how an agent hands a tab
// to a human (captcha, approval, login) or lets them watch it work.
//
// It speaks plain HTTP so it can sit behind a path-mapped reverse proxy
// (tailscale serve): every URL in the page is relative.
//
//	GET  ./          the viewer page
//	GET  ./events    Server-Sent Events: "frame" (JPEG + size), "state", "ended"
//	POST ./input     one input event as JSON (handoff mode only)
//	POST ./done      the human hands the tab back (handoff mode only)
//
// Frames come from the DevTools screencast (Page.startScreencast); input goes
// back through Input.dispatchMouseEvent / dispatchKeyEvent / insertText. No
// VNC, no X server: only the tab is visible, never the desktop.
package liveview

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/rcliao/shell-browser/internal/browser"
	"github.com/rcliao/shell-browser/session"
)

// Mode selects what the person may do.
type Mode string

const (
	// ModeHandoff: the person drives the tab and taps Done to hand it back.
	ModeHandoff Mode = "handoff"
	// ModeWatch: the person only watches; the agent keeps driving.
	ModeWatch Mode = "watch"
)

// Result is reported once, when the person taps Done.
type Result struct {
	URL   string
	Title string
	By    string // tailnet user name/login from tailscale serve headers, if any
}

// Options configures a Viewer.
type Options struct {
	Session   *session.Session
	Endpoint  *session.Endpoint
	Mode      Mode
	Title     string    // headline, e.g. "Pika needs a hand"
	Reason    string    // what the person should do
	ExpiresAt time.Time // shown on the page; enforcement is the caller's
	// Policy gates URLs typed into the address bar. nil = DefaultPolicy.
	Policy *browser.Policy
	// OnDone runs (once, in its own goroutine) when the person taps Done.
	OnDone func(Result)
}

// Viewer is one live view of a session tab.
type Viewer struct {
	opt    Options
	policy *browser.Policy
	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	tabCtx   context.Context
	release  func()
	targetID string
	frame    []byte // last "frame" SSE event, replayed to new subscribers
	devW     float64
	devH     float64
	url      string
	title    string
	lastX    float64
	lastY    float64
	subs     map[chan []byte]struct{}
	ended    bool
	doneOnce sync.Once

	inputMu sync.Mutex // serialises dispatch so a tap's press/release never interleave
}

// New attaches to the session's current tab and starts the screencast.
func New(parent context.Context, opt Options) (*Viewer, error) {
	if opt.Session == nil || opt.Endpoint == nil {
		return nil, errors.New("liveview: Session and Endpoint are required")
	}
	if opt.Mode == "" {
		opt.Mode = ModeHandoff
	}
	pol := opt.Policy
	if pol == nil {
		pol = browser.DefaultPolicy()
	}
	ctx, cancel := context.WithCancel(parent)
	v := &Viewer{opt: opt, policy: pol, ctx: ctx, cancel: cancel, subs: map[chan []byte]struct{}{}}
	tid, err := opt.Session.CurrentTarget(ctx, opt.Endpoint)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("liveview: find tab: %w", err)
	}
	if err := v.attach(tid); err != nil {
		cancel()
		return nil, err
	}
	return v, nil
}

// attach switches the view to tab tid: detach the old one (leaving it open),
// listen to the new one and start its screencast.
func (v *Viewer) attach(tid string) error {
	tabCtx, release, err := session.Attach(v.ctx, v.opt.Endpoint, tid, chromedp.WithLogf(slog.Info))
	if err != nil {
		return fmt.Errorf("liveview: %w", err)
	}
	chromedp.ListenTarget(tabCtx, func(ev any) { v.onTabEvent(tabCtx, ev) })
	var loc, title string
	err = chromedp.Run(tabCtx,
		page.BringToFront(),
		chromedp.Location(&loc),
		chromedp.Title(&title),
		page.StartScreencast().
			WithFormat(page.ScreencastFormatJpeg).
			WithQuality(60).
			WithMaxWidth(1280).
			WithMaxHeight(1280).
			WithEveryNthFrame(1),
	)
	if err != nil {
		release()
		return fmt.Errorf("liveview: attach tab: %w", err)
	}
	v.mu.Lock()
	old := v.release
	v.tabCtx, v.release, v.targetID = tabCtx, release, tid
	v.url, v.title = loc, title
	v.mu.Unlock()
	if old != nil {
		old()
	}
	_ = v.opt.Session.SetTarget(tid) // the agent resumes in whatever tab the human ended on
	v.broadcastState()
	// Each attach has its own DevTools connection, which ends when we switch
	// away; the tab-following listener has to live on the current one.
	go v.followTabs(tabCtx)
	return nil
}

func (v *Viewer) onTabEvent(tabCtx context.Context, ev any) {
	switch e := ev.(type) {
	case *page.EventScreencastFrame:
		// Ack off the event goroutine: chromedp listeners must not block on Run.
		go func(id int64) { _ = chromedp.Run(tabCtx, page.ScreencastFrameAck(id)) }(e.SessionID)
		v.mu.Lock()
		if tabCtx != v.tabCtx {
			v.mu.Unlock()
			return // a frame from a tab we already switched away from
		}
		if e.Metadata != nil {
			v.devW, v.devH = e.Metadata.DeviceWidth, e.Metadata.DeviceHeight
		}
		msg := sseEvent("frame", map[string]any{"img": e.Data, "w": v.devW, "h": v.devH})
		v.frame = msg
		v.mu.Unlock()
		v.broadcast(msg)
	case *page.EventFrameNavigated:
		if e.Frame != nil && e.Frame.ParentID == "" {
			v.mu.Lock()
			v.url = e.Frame.URL + e.Frame.URLFragment
			v.mu.Unlock()
			v.broadcastState()
			go v.refreshTitle(tabCtx)
		}
	case *page.EventNavigatedWithinDocument:
		v.mu.Lock()
		v.url = e.URL
		v.mu.Unlock()
		v.broadcastState()
	}
}

func (v *Viewer) refreshTitle(tabCtx context.Context) {
	time.Sleep(500 * time.Millisecond)
	var title string
	if chromedp.Run(tabCtx, chromedp.Title(&title)) == nil {
		v.mu.Lock()
		v.title = title
		v.mu.Unlock()
		v.broadcastState()
	}
}

// followTabs moves the view to a tab the current one opens (target=_blank,
// popups such as OAuth windows) and off a tab that closes.
func (v *Viewer) followTabs(tabCtx context.Context) {
	err := chromedp.Run(tabCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		return target.SetDiscoverTargets(true).Do(cdp.WithExecutor(ctx, chromedp.FromContext(ctx).Browser))
	}))
	if err != nil {
		slog.Warn("liveview: target discovery unavailable; new tabs will not be followed", "error", err)
		return
	}
	chromedp.ListenBrowser(tabCtx, func(ev any) {
		switch e := ev.(type) {
		case *target.EventTargetCreated:
			info := e.TargetInfo
			v.mu.Lock()
			cur, live := v.targetID, v.tabCtx == tabCtx
			v.mu.Unlock()
			if live && info != nil && info.Type == "page" && string(info.OpenerID) == cur {
				go v.switchTo(string(info.TargetID))
			}
		case *target.EventTargetDestroyed:
			v.mu.Lock()
			cur, live := v.targetID, v.tabCtx == tabCtx
			v.mu.Unlock()
			if live && string(e.TargetID) == cur {
				go v.switchToAny()
			}
		}
	})
}

func (v *Viewer) switchTo(tid string) {
	time.Sleep(300 * time.Millisecond) // let the new tab commit its first navigation
	if err := v.attach(tid); err != nil {
		slog.Warn("liveview: could not follow new tab", "error", err)
	}
}

func (v *Viewer) switchToAny() {
	ctx, cancel := context.WithTimeout(v.ctx, 5*time.Second)
	defer cancel()
	pages, err := v.opt.Endpoint.Pages(ctx)
	if err != nil || len(pages) == 0 {
		return
	}
	if err := v.attach(pages[0].ID); err != nil {
		slog.Warn("liveview: could not switch after tab closed", "error", err)
	}
}

// URL is the page the tab currently shows.
func (v *Viewer) URL() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.url
}

// Close stops the screencast, tells open pages the view ended, and detaches
// (the tab stays open for the agent).
func (v *Viewer) Close(message string) {
	v.mu.Lock()
	if v.ended {
		v.mu.Unlock()
		return
	}
	v.ended = true
	tabCtx, release := v.tabCtx, v.release
	v.mu.Unlock()
	if message == "" {
		message = "This view has ended."
	}
	v.broadcast(sseEvent("ended", map[string]string{"message": message}))
	if tabCtx != nil {
		sctx, cancel := context.WithTimeout(tabCtx, 2*time.Second)
		_ = chromedp.Run(sctx, page.StopScreencast())
		cancel()
	}
	if release != nil {
		release()
	}
	v.mu.Lock()
	for ch := range v.subs {
		close(ch)
		delete(v.subs, ch)
	}
	v.mu.Unlock()
	v.cancel()
}

// ServeHTTP implements the viewer's routes, relative to wherever it is mounted.
func (v *Viewer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Frame-Options", "DENY")
	switch {
	case r.Method == http.MethodGet && (r.URL.Path == "/" || r.URL.Path == ""):
		v.servePage(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/events":
		v.serveEvents(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/input":
		v.serveInput(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/done":
		v.serveDone(w, r)
	default:
		http.NotFound(w, r)
	}
}

//go:embed page.html
var pageHTML string

var pageTmpl = template.Must(template.New("page").Parse(pageHTML))

func (v *Viewer) servePage(w http.ResponseWriter, _ *http.Request) {
	expires := ""
	if !v.opt.ExpiresAt.IsZero() {
		expires = v.opt.ExpiresAt.Local().Format("15:04")
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = pageTmpl.Execute(w, map[string]any{
		"Title":       v.opt.Title,
		"Reason":      v.opt.Reason,
		"Interactive": v.opt.Mode == ModeHandoff,
		"Expires":     expires,
	})
}

func (v *Viewer) serveEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch := make(chan []byte, 1)
	v.mu.Lock()
	if v.ended {
		v.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(sseEvent("ended", map[string]string{"message": "This view has ended."}))
		return
	}
	v.subs[ch] = struct{}{}
	first := [][]byte{v.stateEventLocked()}
	if v.frame != nil {
		first = append(first, v.frame)
	}
	v.mu.Unlock()
	defer func() {
		v.mu.Lock()
		if _, ok := v.subs[ch]; ok {
			delete(v.subs, ch)
		}
		v.mu.Unlock()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	for _, m := range first {
		_, _ = w.Write(m)
	}
	fl.Flush()

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			_, _ = w.Write([]byte(": keepalive\n\n"))
			fl.Flush()
		case m, ok := <-ch:
			if !ok {
				return
			}
			if _, err := w.Write(m); err != nil {
				return
			}
			fl.Flush()
			// At most ~10 frames a second per viewer: a phone on cellular
			// cannot take a 60 fps JPEG stream, and the newest frame wins.
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// broadcast offers msg to every subscriber, replacing any message a slow one
// has not taken yet (latest wins). "ended" and "state" are small and rare;
// losing an intermediate frame is the point.
func (v *Viewer) broadcast(msg []byte) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for ch := range v.subs {
		select {
		case ch <- msg:
		default:
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- msg:
			default:
			}
		}
	}
}

func (v *Viewer) broadcastState() {
	v.mu.Lock()
	msg := v.stateEventLocked()
	v.mu.Unlock()
	v.broadcast(msg)
}

func (v *Viewer) stateEventLocked() []byte {
	return sseEvent("state", map[string]any{"url": v.url, "title": v.title})
}

func sseEvent(name string, data any) []byte {
	b, _ := json.Marshal(data)
	return []byte("event: " + name + "\ndata: " + string(b) + "\n\n")
}

// InputEvent is one action from the person. X and Y are fractions (0..1) of
// the displayed frame, so the page needs no knowledge of the real viewport.
type InputEvent struct {
	T   string  `json:"t"` // tap | wheel | text | key | nav | back | reload
	X   float64 `json:"x"`
	Y   float64 `json:"y"`
	DY  float64 `json:"dy"`
	S   string  `json:"s"`   // text
	K   string  `json:"k"`   // key name
	URL string  `json:"url"` // nav
}

func (v *Viewer) serveInput(w http.ResponseWriter, r *http.Request) {
	if v.opt.Mode != ModeHandoff {
		http.Error(w, "this view is watch-only", http.StatusForbidden)
		return
	}
	// JSON only: a cross-site form post cannot set this content type without
	// a CORS preflight, which this server never approves.
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		http.Error(w, "application/json required", http.StatusUnsupportedMediaType)
		return
	}
	var ev InputEvent
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&ev); err != nil {
		http.Error(w, "bad input: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := v.Dispatch(ev); err != nil {
		var pe *browser.PolicyError
		if errors.As(err, &pe) {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (v *Viewer) serveDone(w http.ResponseWriter, r *http.Request) {
	if v.opt.Mode != ModeHandoff {
		http.Error(w, "this view is watch-only", http.StatusForbidden)
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		http.Error(w, "application/json required", http.StatusUnsupportedMediaType)
		return
	}
	v.mu.Lock()
	res := Result{URL: v.url, Title: v.title, By: tailnetUser(r)}
	v.mu.Unlock()
	v.doneOnce.Do(func() {
		if v.opt.OnDone != nil {
			go v.opt.OnDone(res)
		}
	})
	w.WriteHeader(http.StatusNoContent)
}

// tailnetUser names who is on the other end, from the identity headers
// tailscale serve adds for requests from tailnet users.
func tailnetUser(r *http.Request) string {
	if n := r.Header.Get("Tailscale-User-Name"); n != "" {
		return n
	}
	return r.Header.Get("Tailscale-User-Login")
}

// keyDefs maps the special keys the page can send.
var keyDefs = map[string]struct {
	code string
	vk   int64
	text string
}{
	"Enter":      {"Enter", 13, "\r"},
	"Backspace":  {"Backspace", 8, ""},
	"Tab":        {"Tab", 9, ""},
	"Escape":     {"Escape", 27, ""},
	"ArrowLeft":  {"ArrowLeft", 37, ""},
	"ArrowUp":    {"ArrowUp", 38, ""},
	"ArrowRight": {"ArrowRight", 39, ""},
	"ArrowDown":  {"ArrowDown", 40, ""},
	"Delete":     {"Delete", 46, ""},
}

// Dispatch replays one input event into the tab.
func (v *Viewer) Dispatch(ev InputEvent) error {
	v.inputMu.Lock()
	defer v.inputMu.Unlock()
	v.mu.Lock()
	tabCtx, devW, devH, lx, ly, ended := v.tabCtx, v.devW, v.devH, v.lastX, v.lastY, v.ended
	v.mu.Unlock()
	if ended || tabCtx == nil {
		return errors.New("this view has ended")
	}
	ctx, cancel := context.WithTimeout(tabCtx, 10*time.Second)
	defer cancel()

	switch ev.T {
	case "tap":
		if devW <= 0 || devH <= 0 {
			return errors.New("no frame yet; wait for the picture to appear")
		}
		x, y := clamp01(ev.X)*devW, clamp01(ev.Y)*devH
		// Glide the pointer in from where it was: checkbox captchas look at
		// mouse movement, and a pointer that teleports onto the box reads as a bot.
		acts := []chromedp.Action{}
		const steps = 8
		for i := 1; i <= steps; i++ {
			f := float64(i) / steps
			acts = append(acts, input.DispatchMouseEvent(input.MouseMoved, lx+(x-lx)*f, ly+(y-ly)*f), chromedp.Sleep(12*time.Millisecond))
		}
		acts = append(acts,
			input.DispatchMouseEvent(input.MousePressed, x, y).WithButton(input.Left).WithClickCount(1),
			chromedp.Sleep(60*time.Millisecond),
			input.DispatchMouseEvent(input.MouseReleased, x, y).WithButton(input.Left).WithClickCount(1),
		)
		if err := chromedp.Run(ctx, acts...); err != nil {
			return err
		}
		v.mu.Lock()
		v.lastX, v.lastY = x, y
		v.mu.Unlock()
		return nil
	case "wheel":
		if devW <= 0 || devH <= 0 {
			return nil
		}
		dy := ev.DY
		if dy > 2000 {
			dy = 2000
		} else if dy < -2000 {
			dy = -2000
		}
		return chromedp.Run(ctx, input.DispatchMouseEvent(input.MouseWheel, clamp01(ev.X)*devW, clamp01(ev.Y)*devH).WithDeltaX(0).WithDeltaY(dy))
	case "text":
		if ev.S == "" {
			return nil
		}
		return chromedp.Run(ctx, input.InsertText(ev.S))
	case "key":
		k, ok := keyDefs[ev.K]
		if !ok {
			return fmt.Errorf("unsupported key %q", ev.K)
		}
		down := input.DispatchKeyEvent(input.KeyRawDown)
		if k.text != "" {
			down = input.DispatchKeyEvent(input.KeyDown).WithText(k.text).WithUnmodifiedText(k.text)
		}
		down = down.WithKey(ev.K).WithCode(k.code).WithWindowsVirtualKeyCode(k.vk).WithNativeVirtualKeyCode(k.vk)
		up := input.DispatchKeyEvent(input.KeyUp).WithKey(ev.K).WithCode(k.code).WithWindowsVirtualKeyCode(k.vk).WithNativeVirtualKeyCode(k.vk)
		return chromedp.Run(ctx, down, up)
	case "nav":
		u, err := normalizeURL(ev.URL)
		if err != nil {
			return err
		}
		if err := v.policy.Check(u); err != nil {
			return err
		}
		// Fire the navigation without waiting for load: the person watches it
		// happen, and a slow page must not time the request out.
		return chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
			_, _, _, _, err := page.Navigate(u).Do(ctx)
			return err
		}))
	case "back":
		return chromedp.Run(ctx, chromedp.NavigateBack())
	case "reload":
		return chromedp.Run(ctx, page.Reload())
	}
	return fmt.Errorf("unknown input %q", ev.T)
}

func clamp01(f float64) float64 {
	switch {
	case f < 0:
		return 0
	case f > 1:
		return 1
	}
	return f
}

// normalizeURL turns what a person types into an address bar into a URL:
// "example.com" → "https://example.com". Only http(s) is allowed.
func normalizeURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("empty address")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("not a web address: %q", raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("only http and https addresses can be opened here")
	}
	return u.String(), nil
}
