package browser

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/rcliao/shell-browser/session"
)

// StayURL, given as the URL with a session, means "work on whatever page the
// session's tab is already showing" — e.g. after a human finished a handoff.
const StayURL = "-"

// launchEphemeral starts a throwaway Chrome for one run (the original mode).
func launchEphemeral(ctx context.Context, cfg Config) (context.Context, func(), error) {
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
			return nil, nil, fmt.Errorf("profile %q: %w", cfg.Profile, err)
		}
		opts = append(opts, chromedp.UserDataDir(dir))
		slog.Info("browser: using persistent profile", "profile", sanitizeProfileName(cfg.Profile))
	}

	allocCtx, allocCancel := chromedp.NewExecAllocator(ctx, opts...)
	// WithLogf only, deliberately: chromedp's WithDebugf dumps raw CDP traffic,
	// which carries cookies, form values and page content. Do not add it.
	browserCtx, browserCancel := chromedp.NewContext(allocCtx, chromedp.WithLogf(slog.Info))
	return browserCtx, func() { browserCancel(); allocCancel() }, nil
}

// attachSession connects to the named session's Chrome (starting it if
// needed) and attaches to its working tab. The returned close func only
// detaches: the tab and the browser stay up for the next run or a human.
//
// Never call chromedp.Cancel on this context: it closes the whole browser.
func attachSession(ctx context.Context, cfg Config) (context.Context, func(), error) {
	root := cfg.SessionRoot
	if root == "" {
		root = session.Root()
	}
	s, err := session.Open(root, cfg.Session)
	if err != nil {
		return nil, nil, err
	}
	if err := s.CheckFree(time.Now()); err != nil {
		return nil, nil, err
	}
	ep, err := s.Ensure(ctx, session.LaunchOptions{ChromePath: cfg.ChromePath, Headless: cfg.Headless})
	if err != nil {
		return nil, nil, err
	}
	tid, err := s.CurrentTarget(ctx, ep)
	if err != nil {
		return nil, nil, fmt.Errorf("find the session's tab: %w", err)
	}
	browserCtx, release, err := session.Attach(ctx, ep, tid, chromedp.WithLogf(slog.Info))
	if err != nil {
		return nil, nil, err
	}
	s.Touch() // a long run must not look idle to the reaper while it runs
	slog.Info("browser: attached to session", "session", s.Name, "port", ep.Port)
	return browserCtx, func() {
		release()
		s.Touch()
	}, nil
}
