package browser

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/chromedp"
)

// Realistic Chrome user agent string.
const defaultUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/137.0.0.0 Safari/537.36"

// JS snippet to remove automation indicators after page load.
//
// This is OUR code, injected by the tool. It is unrelated to the `js` action,
// which evaluates caller-supplied JavaScript and is gated behind --allow-js.
const stealthJS = `
// Remove webdriver flag
Object.defineProperty(navigator, 'webdriver', {get: () => undefined});
// Fake plugins
Object.defineProperty(navigator, 'plugins', {get: () => [1, 2, 3, 4, 5]});
// Fake languages
Object.defineProperty(navigator, 'languages', {get: () => ['en-US', 'en']});
// Spoof chrome runtime
window.chrome = {runtime: {}};
`

// elementTimeout bounds how long a single element-targeting action waits for
// its selector or ref to match.
const elementTimeout = 10 * time.Second

// ProfilesDirName is the directory (under ~/.shell) holding persistent
// per-site Chrome profiles.
const ProfilesDirName = "browser-profiles"

// Config holds browser automation settings.
type Config struct {
	Enabled        bool   `json:"enabled"`
	Headless       bool   `json:"headless"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	ChromePath     string `json:"chrome_path"`

	// AllowJS enables the caller-supplied `js` action. Off by default:
	// arbitrary JS in a page context is the widest blast radius this tool has.
	AllowJS bool `json:"allow_js"`

	// Policy gates every navigation and redirect. nil means DefaultPolicy.
	Policy *Policy `json:"-"`

	// Profile, when non-empty, names a persistent Chrome profile under
	// ~/.shell/browser-profiles/<profile> so logins survive between runs.
	// Empty means an ephemeral profile (the default).
	Profile string `json:"profile"`
}

// StepResult holds the outcome of a single action.
type StepResult struct {
	Step        int
	Description string
	Output      string // text output ("OK", extracted text, snapshot listing)
	Screenshot  []byte // non-nil only for screenshot actions
	Err         error

	// Untrusted marks output that came from the page itself and must be
	// wrapped in provenance markers when printed.
	Untrusted bool
	// SourceURL is the page the untrusted output came from.
	SourceURL string
	// Retrieved is when the untrusted output was read.
	Retrieved time.Time
}

// Result holds the outcome of executing a browser directive.
type Result struct {
	URL   string
	Steps []StepResult

	// Mode records which engine served the run: "http" or "chrome".
	Mode string
	// Escalation explains why the HTTP fast path was abandoned, if it was.
	Escalation string
}

func (r *Result) fail(step int, desc string, err error) {
	r.Steps = append(r.Steps, StepResult{Step: step, Description: desc, Err: err})
}

// ProfileDir returns the on-disk user-data-dir for a named profile, creating
// it if needed. The name is sanitised so it cannot escape the profiles root.
func ProfileDir(name string) (string, error) {
	clean := sanitizeProfileName(name)
	if clean == "" {
		return "", fmt.Errorf("invalid profile name %q: use letters, digits, '-' or '_'", name)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".shell", ProfilesDirName, clean)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create profile dir: %w", err)
	}
	return dir, nil
}

var profileNameRe = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

func sanitizeProfileName(name string) string {
	return profileNameRe.ReplaceAllString(strings.TrimSpace(name), "")
}

// Execute runs a browser directive: navigates to the URL and performs each action.
func Execute(ctx context.Context, cfg Config, d Directive) *Result {
	result := &Result{URL: d.URL, Mode: "chrome"}

	policy := cfg.Policy
	if policy == nil {
		policy = DefaultPolicy()
	}
	cfg.AllowJS = effectiveAllowJS(cfg, policy)

	// Policy gate BEFORE any browser is launched.
	if err := policy.Check(d.URL); err != nil {
		result.fail(1, fmt.Sprintf("navigate %q", d.URL), err)
		return result
	}

	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Build chromedp options with anti-detection measures.
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		// Anti-detection: remove automation indicators
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("exclude-switches", "enable-automation"),
		chromedp.Flag("disable-features", "AutomationControlled"),

		// Realistic user agent
		chromedp.UserAgent(defaultUserAgent),

		// Realistic window size
		chromedp.WindowSize(1920, 1080),

		// Disable automation-related infobars
		chromedp.Flag("disable-infobars", true),
		chromedp.Flag("disable-extensions", true),
	)
	if cfg.Headless {
		// Use new headless mode which is harder to detect
		opts = append(opts, chromedp.Flag("headless", "new"))
	}
	if cfg.ChromePath != "" {
		opts = append(opts, chromedp.ExecPath(cfg.ChromePath))
	}
	if cfg.Profile != "" {
		dir, err := ProfileDir(cfg.Profile)
		if err != nil {
			result.fail(1, fmt.Sprintf("profile %q", cfg.Profile), err)
			return result
		}
		opts = append(opts, chromedp.UserDataDir(dir))
		slog.Info("browser: using persistent profile", "profile", sanitizeProfileName(cfg.Profile))
	}

	allocCtx, allocCancel := chromedp.NewExecAllocator(ctx, opts...)
	defer allocCancel()

	// WithLogf only, deliberately: chromedp's WithDebugf dumps raw CDP traffic,
	// which carries cookies, form values and page content. Do not add it.
	browserCtx, browserCancel := chromedp.NewContext(allocCtx, chromedp.WithLogf(slog.Info))
	defer browserCancel()

	ex := &executor{ctx: browserCtx, cfg: cfg, policy: policy, currentURL: d.URL}

	// Inject stealth JS before navigation to patch navigator properties.
	if err := chromedp.Run(browserCtx, chromedp.Evaluate(stealthJS, nil)); err != nil {
		slog.Warn("browser: stealth injection failed (non-fatal)", "error", err)
	}

	// Always navigate first.
	slog.Info("browser: navigating", "url", redactURL(d.URL))
	if err := chromedp.Run(browserCtx, chromedp.Navigate(d.URL)); err != nil {
		result.fail(1, fmt.Sprintf("navigate %q", d.URL), err)
		return result
	}
	navStep := StepResult{Step: 1, Description: fmt.Sprintf("navigate %q", d.URL), Output: "OK"}
	// Re-check the landing URL: redirects can move us off the approved host.
	if err := ex.syncLocation(); err != nil {
		navStep.Err = err
		result.Steps = append(result.Steps, navStep)
		return result
	}
	result.Steps = append(result.Steps, navStep)

	// Re-inject stealth after navigation (page load resets JS state).
	_ = chromedp.Run(browserCtx, chromedp.Evaluate(stealthJS, nil))

	// Execute each action.
	for i, action := range d.Actions {
		stepNum := i + 2 // navigate is step 1
		if action.Type == ActionNavigate {
			// Already navigated above; skip duplicate navigate actions.
			continue
		}

		sr := ex.execute(stepNum, action)
		result.Steps = append(result.Steps, sr)

		if sr.Err != nil {
			slog.Warn("browser: action failed", "step", stepNum, "action", action.String(), "error", sr.Err)
			// Continue executing remaining actions so the caller sees partial results.
			continue
		}

		// An action may have triggered a navigation; enforce policy on the new URL.
		switch action.Type {
		case ActionClick, ActionType_, ActionWait, ActionSleep:
			if err := ex.syncLocation(); err != nil {
				result.fail(stepNum, "post-action URL check", err)
				return result
			}
		}
	}

	return result
}

// effectiveAllowJS resolves the js gate from the flag and the policy file.
func effectiveAllowJS(cfg Config, p *Policy) bool {
	return cfg.AllowJS || (p != nil && p.AllowJS)
}

// jsGate returns the actionable refusal for a caller-supplied `js` action when
// page-context JavaScript has not been explicitly enabled.
func jsGate(allow bool) error {
	if allow {
		return nil
	}
	return errors.New("the `js` action is disabled by policy. " +
		"Page-context JavaScript is the widest blast radius this tool has, so it is off by default. " +
		"Use the higher-level actions instead (`snapshot` to list interactable elements, `text`/`extract` to read " +
		"content, `click`/`type` to interact) — or re-run with --allow-js (or set \"allow_js\": true in " +
		"~/.shell/browser-policy.json) if you genuinely need to evaluate an expression")
}

// executor carries the per-run state shared across actions.
type executor struct {
	ctx        context.Context
	cfg        Config
	policy     *Policy
	currentURL string
	snapshot   *Snapshot
}

// syncLocation reads the browser's current URL and re-applies the domain
// policy to it. This is the redirect / client-side-navigation guard.
func (e *executor) syncLocation() error {
	var loc string
	if err := chromedp.Run(e.ctx, chromedp.Location(&loc)); err != nil {
		return nil // location unavailable; not a policy decision
	}
	if loc == "" || loc == "about:blank" || loc == e.currentURL {
		return nil
	}
	e.currentURL = loc
	if err := e.policy.Check(loc); err != nil {
		return fmt.Errorf("navigation left the approved destination: %w", err)
	}
	return nil
}

func (e *executor) execute(step int, a Action) StepResult {
	sr := StepResult{Step: step, Description: a.String()}
	ctx := e.ctx

	switch a.Type {
	case ActionClick:
		sel, opts, err := e.target(a.Selector)
		if err != nil {
			sr.Err = err
			break
		}
		// Bound the wait: a selector that never matches must fail fast with
		// suggestions rather than eating the whole run timeout.
		clickCtx, cancel := context.WithTimeout(ctx, elementTimeout)
		defer cancel()
		if err := chromedp.Run(clickCtx, chromedp.Click(sel, opts...)); err != nil {
			sr.Err = e.enrich(a.Selector, err)
		} else {
			sr.Output = "OK"
		}

	case ActionType_:
		sel, opts, err := e.target(a.Selector)
		if err != nil {
			sr.Err = err
			break
		}
		typeCtx, cancel := context.WithTimeout(ctx, elementTimeout)
		defer cancel()
		err = chromedp.Run(typeCtx,
			chromedp.Clear(sel, opts...),
			chromedp.SendKeys(sel, a.Value, opts...),
		)
		if err != nil {
			sr.Err = e.enrich(a.Selector, err)
		} else {
			sr.Output = "OK"
		}

	case ActionWait:
		waitCtx, cancel := context.WithTimeout(ctx, elementTimeout)
		defer cancel()
		sel, opts, err := e.target(a.Selector)
		if err != nil {
			sr.Err = err
			break
		}
		if err := chromedp.Run(waitCtx, chromedp.WaitVisible(sel, opts...)); err != nil {
			sr.Err = e.enrich(a.Selector, err)
		} else {
			sr.Output = "OK"
		}

	case ActionScreenshot:
		var buf []byte
		if err := chromedp.Run(ctx, chromedp.FullScreenshot(&buf, 90)); err != nil {
			sr.Err = err
		} else {
			sr.Screenshot = buf
			sr.Output = "[sent to chat]"
		}

	case ActionExtract:
		var text string
		if err := chromedp.Run(ctx, chromedp.Text(a.Selector, &text, chromedp.ByQuery)); err != nil {
			sr.Err = e.enrich(a.Selector, err)
		} else {
			e.markUntrusted(&sr, strings.TrimSpace(text))
		}

	case ActionText:
		var text string
		if err := chromedp.Run(ctx, chromedp.Text("body", &text, chromedp.ByQuery)); err != nil {
			sr.Err = err
		} else {
			e.markUntrusted(&sr, collapseWhitespace(text))
		}

	case ActionSnapshot:
		snap, err := TakeSnapshot(ctx, e.currentURL)
		if err != nil {
			sr.Err = err
			break
		}
		e.snapshot = snap
		e.markUntrusted(&sr, snap.Format())

	case ActionJS:
		if err := jsGate(e.cfg.AllowJS); err != nil {
			sr.Err = err
			break
		}
		var res interface{}
		if err := chromedp.Run(ctx, chromedp.Evaluate(a.Value, &res)); err != nil {
			sr.Err = err
		} else {
			// A JS result is page-derived data like any extraction.
			e.markUntrusted(&sr, fmt.Sprintf("%v", res))
		}

	case ActionSleep:
		dur, err := ParseSleepDuration(a.Value)
		if err != nil {
			sr.Err = err
		} else {
			time.Sleep(dur)
			sr.Output = "OK"
		}

	default:
		sr.Err = fmt.Errorf("unknown action type %d", a.Type)
	}

	return sr
}

func (e *executor) markUntrusted(sr *StepResult, text string) {
	sr.Output = text
	sr.Untrusted = true
	sr.SourceURL = e.currentURL
	sr.Retrieved = time.Now()
}

// target turns a selector-or-ref into arguments for the chromedp query actions.
func (e *executor) target(sel string) (interface{}, []chromedp.QueryOption, error) {
	if !IsRef(sel) {
		return sel, []chromedp.QueryOption{chromedp.ByQuery}, nil
	}
	el, ok := e.snapshot.Lookup(sel)
	if !ok {
		if e.snapshot == nil {
			return nil, nil, fmt.Errorf("%q looks like a snapshot ref but no snapshot has been taken in this run; run the `snapshot` action first", sel)
		}
		return nil, nil, fmt.Errorf("ref %q is not in the current snapshot (it has %d elements).%s",
			sel, len(e.snapshot.Elements), formatCandidates(e.snapshot, sel))
	}
	ids, err := resolveRef(e.ctx, el)
	if err != nil {
		return nil, nil, err
	}
	return []cdp.NodeID(ids), []chromedp.QueryOption{chromedp.ByNodeID}, nil
}

// enrich appends snapshot-derived retry hints to a selector failure.
func (e *executor) enrich(target string, err error) error {
	if err == nil {
		return nil
	}
	if e.snapshot == nil {
		return fmt.Errorf("%w — no snapshot available for suggestions; run the `snapshot` action to list interactable elements and retry with a ref (e.g. e3)", err)
	}
	if hint := formatCandidates(e.snapshot, target); hint != "" {
		return fmt.Errorf("%w —%s", err, hint)
	}
	return err
}

// ExecuteFetchFirst runs the directive over plain HTTP when possible and falls
// back to Chrome otherwise. forceRender skips the fast path entirely.
func ExecuteFetchFirst(ctx context.Context, cfg Config, d Directive, forceRender bool) *Result {
	policy := cfg.Policy
	if policy == nil {
		policy = DefaultPolicy()
	}
	if forceRender || NeedsBrowser(d.Actions) {
		r := Execute(ctx, cfg, d)
		if forceRender {
			r.Escalation = "--render requested"
		} else if len(d.Actions) > 0 {
			r.Escalation = "actions require a browser"
		}
		return r
	}

	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	fr, err := FetchText(ctx, policy, d.URL, timeout)

	// A policy block is a decision, not a transport failure: do not escalate.
	var pe *PolicyError
	if errors.As(err, &pe) {
		r := &Result{URL: d.URL, Mode: "http"}
		r.fail(1, fmt.Sprintf("navigate %q", d.URL), err)
		return r
	}

	if escalate, why := ShouldEscalate(fr, err); escalate {
		slog.Info("browser: escalating to chrome", "reason", why)
		r := Execute(ctx, cfg, d)
		r.Escalation = why
		return r
	}

	r := &Result{URL: fr.URL, Mode: "http"}
	r.Steps = append(r.Steps, StepResult{
		Step:        1,
		Description: fmt.Sprintf("fetch %q (no browser)", d.URL),
		Output:      fmt.Sprintf("HTTP %d, %d chars of text", fr.Status, len(fr.Text)),
	})
	r.Steps = append(r.Steps, StepResult{
		Step:        2,
		Description: "text",
		Output:      fr.Text,
		Untrusted:   true,
		SourceURL:   fr.URL,
		Retrieved:   fr.Retrieved,
	})
	return r
}

// FormatResults formats the result for feeding back to the calling agent.
// Page-derived output is wrapped in untrusted-content markers.
func FormatResults(r *Result) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "[Browser results for %s", r.URL)
	if r.Mode != "" {
		fmt.Fprintf(&sb, " (engine: %s", r.Mode)
		if r.Escalation != "" {
			fmt.Fprintf(&sb, "; escalated: %s", r.Escalation)
		}
		sb.WriteString(")")
	}
	sb.WriteString("]\n")

	for _, s := range r.Steps {
		switch {
		case s.Err != nil:
			fmt.Fprintf(&sb, "Step %d: %s → ERROR: %s\n", s.Step, s.Description, s.Err)
		case s.Untrusted:
			fmt.Fprintf(&sb, "Step %d: %s →\n", s.Step, s.Description)
			src := s.SourceURL
			if src == "" {
				src = r.URL
			}
			ts := s.Retrieved
			if ts.IsZero() {
				ts = time.Now()
			}
			sb.WriteString(WrapUntrusted(src, ts, s.Output))
			sb.WriteString("\n")
		default:
			fmt.Fprintf(&sb, "Step %d: %s → %s\n", s.Step, s.Description, s.Output)
		}
	}
	sb.WriteString("[End of browser results]")
	return sb.String()
}

// redactURL strips credentials and common secret-bearing query parameters so
// URLs are safe to log. The tool never logs environment variables or cookies.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "[unparseable url]"
	}
	if u.User != nil {
		u.User = url.User("redacted")
	}
	q := u.Query()
	changed := false
	for k := range q {
		lk := strings.ToLower(k)
		for _, bad := range []string{"token", "key", "secret", "password", "passwd", "auth", "session", "sig"} {
			if strings.Contains(lk, bad) {
				q.Set(k, "REDACTED")
				changed = true
				break
			}
		}
	}
	if changed {
		u.RawQuery = q.Encode()
	}
	return u.String()
}
