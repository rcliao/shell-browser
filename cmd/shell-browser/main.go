// Command shell-browser drives a headless Chrome browser (or, when the page
// allows it, a plain HTTP fetch) and prints the results for a calling agent.
//
//	shell-browser [flags] <url> [action...]
//
// Page content is untrusted input: everything read off a page is printed
// inside <untrusted-page-content> markers, navigation is gated by a domain
// policy, and caller-supplied JavaScript is disabled unless --allow-js.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	sb "github.com/rcliao/shell-browser/internal/browser"
	"github.com/rcliao/shell-browser/liveview"
	"github.com/rcliao/shell-browser/session"
	"github.com/spf13/cobra"
)

// exitLocked is the exit status when a human holds the session's tab.
const exitLocked = 3

func main() {
	var (
		headless   bool
		timeout    time.Duration
		chromePath string
		policyPath string
		allow      []string
		allowJS    bool
		render     bool
		profile    string
		sessName   string
		listSess   bool
		closeSess  bool
		serveLive  string
	)

	root := &cobra.Command{
		Use:   "shell-browser [flags] <url> [action...]",
		Short: "Headless browser automation with a domain policy and untrusted-content markers",
		Long: `shell-browser <url> [action...]

actions:
  snapshot              list interactable elements with refs (e1, e2, ...)
  text                  extract the whole page as plain text
  extract "<selector>"  extract text of an element
  click "<sel|ref>"     click a CSS selector or a snapshot ref
  type "<sel|ref>" "<v>" clear and type into an element
  wait "<selector>"     wait for an element to become visible
  screenshot            capture a full-page screenshot
  js "<expr>"           evaluate JavaScript (requires --allow-js)
  sleep "<duration>"    wait, e.g. "2s"

sessions (--session <name>):
  Chrome stays open between runs, so the tab, cookies and page carry over.
  Pass "-" as the url to keep working on the page the tab already shows.
  Exit status 3 means a human currently holds the tab (a handoff is open).`,
		Args:          cobra.ArbitraryArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if listSess {
				return listSessions()
			}
			if serveLive != "" {
				if sessName == "" {
					return errors.New("--serve-live needs --session <name>")
				}
				return serveLiveView(sessName, serveLive, policyPath)
			}
			if closeSess {
				if sessName == "" {
					return errors.New("--close-session needs --session <name>")
				}
				return closeSession(sessName)
			}
			if len(args) < 1 {
				return errors.New("missing <url> (or \"-\" with --session)")
			}
			url := args[0]
			if url == sb.StayURL && sessName == "" {
				return errors.New(`url "-" (stay on the current page) needs --session <name>`)
			}
			body := strings.Join(args[1:], "\n")

			if policyPath == "" {
				policyPath = sb.PolicyPath()
			}
			policy, err := sb.LoadPolicy(policyPath)
			if err != nil {
				return err
			}
			for _, a := range allow {
				for _, d := range strings.Split(a, ",") {
					policy.AllowDomain(d)
				}
			}
			if allowJS {
				policy.AllowJS = true
			}

			if sessName != "" && !cmd.Flags().Changed("headless") {
				// A session is where a human may take over, and captchas
				// reject headless Chrome: default to a real window.
				headless = false
			}
			if v := os.Getenv("BROWSER_HEADLESS"); v == "false" || v == "0" {
				headless = false
			}
			if chromePath == "" {
				chromePath = os.Getenv("CHROME_PATH")
			}

			cfg := sb.Config{
				Enabled:        true,
				Headless:       headless,
				TimeoutSeconds: int(timeout.Seconds()),
				ChromePath:     chromePath,
				AllowJS:        allowJS || policy.AllowJS,
				Policy:         policy,
				Profile:        profile,
				Session:        sessName,
			}

			d := sb.ParseDirective(url, body)
			if len(d.Unknown) > 0 {
				// Fail loudly: an unrecognized action used to fall through to
				// the default text read, handing back page content that looks
				// like success.
				return fmt.Errorf("unrecognized action(s): %q — run with --help for the action list; nothing was executed", d.Unknown)
			}
			// A session is a live tab: always use Chrome, never the HTTP fast path.
			res := sb.ExecuteFetchFirst(context.Background(), cfg, d, render || sessName != "")
			if sessName != "" {
				fmt.Fprintf(os.Stderr, "[session %s: tab left open at %s]\n", sessName, res.URL)
			}
			return report(res)
		},
	}

	f := root.Flags()
	f.SetInterspersed(false) // actions can look like flags; keep them positional
	f.BoolVar(&headless, "headless", true, "run Chrome in headless mode")
	f.DurationVar(&timeout, "timeout", 30*time.Second, "overall run timeout")
	f.StringVar(&chromePath, "chrome-path", "", "path to the Chrome binary (default $CHROME_PATH)")
	f.StringVar(&policyPath, "policy", "", "domain policy file (default ~/.shell/browser-policy.json)")
	f.StringArrayVar(&allow, "allow", nil, "explicitly allow a domain (repeatable, comma-separated); overrides the private-network guard")
	f.BoolVar(&allowJS, "allow-js", false, "enable the caller-supplied `js` action (off by default)")
	f.BoolVar(&render, "render", false, "always render in Chrome; skip the HTTP fast path")
	f.StringVar(&profile, "profile", "", "persistent profile name under ~/.shell/browser-profiles/<name>")
	f.StringVar(&sessName, "session", "", "run in a named Chrome that stays open between runs (url \"-\" = stay on the current page)")
	f.BoolVar(&listSess, "list-sessions", false, "list sessions and whether each is running")
	f.BoolVar(&closeSess, "close-session", false, "close the --session Chrome")
	f.StringVar(&serveLive, "serve-live", "", "serve a live, interactive view of the --session tab on this address (e.g. 127.0.0.1:8765) until Done or Ctrl-C")

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		if errors.Is(err, session.ErrLocked) {
			os.Exit(exitLocked)
		}
		os.Exit(2)
	}
}

// report prints the step results and returns a non-nil error if any step failed.
func report(r *sb.Result) error {
	failed := 0
	var locked error
	for _, s := range r.Steps {
		switch {
		case s.Err != nil:
			failed++
			if errors.Is(s.Err, session.ErrLocked) {
				locked = s.Err
			}
			fmt.Fprintf(os.Stderr, "step %d (%s): ERROR: %v\n", s.Step, s.Description, s.Err)
		case s.Screenshot != nil:
			path, err := writeScreenshot(s.Screenshot)
			if err != nil {
				failed++
				fmt.Fprintf(os.Stderr, "step %d (%s): ERROR: %v\n", s.Step, s.Description, err)
				continue
			}
			fmt.Printf("[artifact type=\"image\" path=%q caption=%q]\n", path, "Screenshot of "+r.URL)
		case s.Untrusted:
			src := s.SourceURL
			if src == "" {
				src = r.URL
			}
			ts := s.Retrieved
			if ts.IsZero() {
				ts = time.Now()
			}
			fmt.Printf("step %d (%s):\n%s\n", s.Step, s.Description,
				sb.WrapUntrusted(src, ts, s.Output))
		default:
			fmt.Printf("step %d (%s): %s\n", s.Step, s.Description, s.Output)
		}
	}
	if r.Mode != "" {
		note := "engine: " + r.Mode
		if r.Escalation != "" {
			note += "; escalated because " + r.Escalation
		}
		fmt.Fprintf(os.Stderr, "[%s]\n", note)
	}
	if locked != nil {
		return locked
	}
	if failed > 0 {
		return fmt.Errorf("%d step(s) failed", failed)
	}
	return nil
}

func listSessions() error {
	all, err := session.List(session.Root())
	if err != nil {
		return err
	}
	if len(all) == 0 {
		fmt.Println("no sessions")
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range all {
		state := "stopped"
		if ep := s.Running(ctx); ep != nil {
			state = "running"
			if pages, err := ep.Pages(ctx); err == nil {
				state = fmt.Sprintf("running, %d tab(s)", len(pages))
			}
		}
		if l, ok := s.HeldBy(time.Now()); ok {
			state += fmt.Sprintf(", held by a human (handoff #%d)", l.HandoffID)
		}
		last := "never"
		if t := s.LastUsed(); !t.IsZero() {
			last = t.Local().Format("2006-01-02 15:04")
		}
		fmt.Printf("%s\t%s\tlast used %s\n", s.Name, state, last)
	}
	return nil
}

func closeSession(name string) error {
	s, err := session.Open(session.Root(), name)
	if err != nil {
		return err
	}
	if err := s.CheckFree(time.Now()); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ep := s.Running(ctx)
	if ep == nil {
		fmt.Printf("session %s is not running\n", name)
		return nil
	}
	if err := session.Close(ctx, ep); err != nil {
		return err
	}
	fmt.Printf("closed session %s\n", name)
	return nil
}

func writeScreenshot(data []byte) (string, error) {
	f, err := os.CreateTemp("", "shell-browser-*.png")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return "", err
	}
	return f.Name(), nil
}

// serveLiveView is the manual/debug entry to the live view: the shell daemon
// embeds package liveview directly and publishes it over the tailnet.
func serveLiveView(name, addr, policyPath string) error {
	if policyPath == "" {
		policyPath = sb.PolicyPath()
	}
	policy, err := sb.LoadPolicy(policyPath)
	if err != nil {
		return err
	}
	s, err := session.Open(session.Root(), name)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ep, err := s.Ensure(ctx, session.LaunchOptions{})
	if err != nil {
		return err
	}
	done := make(chan liveview.Result, 1)
	v, err := liveview.New(ctx, liveview.Options{
		Session: s, Endpoint: ep, Mode: liveview.ModeHandoff,
		Title: "Live browser: " + name, Policy: policy,
		OnDone: func(r liveview.Result) { done <- r },
	})
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: addr, Handler: v}
	go func() { _ = srv.ListenAndServe() }()
	fmt.Fprintf(os.Stderr, "live view of session %s on http://%s/ (Ctrl-C to stop)\n", name, addr)
	select {
	case r := <-done:
		fmt.Printf("done: %s\n", r.URL)
	case <-ctx.Done():
	}
	v.Close("")
	shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return srv.Shutdown(shutCtx)
}
