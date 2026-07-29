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
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/rcliao/shell-browser/internal/browser"
	"github.com/spf13/cobra"
)

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
  sleep "<duration>"    wait, e.g. "2s"`,
		Args:          cobra.MinimumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			url := args[0]
			body := strings.Join(args[1:], "\n")

			if policyPath == "" {
				policyPath = browser.PolicyPath()
			}
			policy, err := browser.LoadPolicy(policyPath)
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

			if v := os.Getenv("BROWSER_HEADLESS"); v == "false" || v == "0" {
				headless = false
			}
			if chromePath == "" {
				chromePath = os.Getenv("CHROME_PATH")
			}

			cfg := browser.Config{
				Enabled:        true,
				Headless:       headless,
				TimeoutSeconds: int(timeout.Seconds()),
				ChromePath:     chromePath,
				AllowJS:        allowJS || policy.AllowJS,
				Policy:         policy,
				Profile:        profile,
			}

			d := browser.ParseDirective(url, body)
			if len(d.Unknown) > 0 {
				// Fail loudly: an unrecognized action used to fall through to
				// the default text read, handing back page content that looks
				// like success.
				return fmt.Errorf("unrecognized action(s): %q — run with --help for the action list; nothing was executed", d.Unknown)
			}
			res := browser.ExecuteFetchFirst(context.Background(), cfg, d, render)
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

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
}

// report prints the step results and returns a non-nil error if any step failed.
func report(r *browser.Result) error {
	failed := 0
	for _, s := range r.Steps {
		switch {
		case s.Err != nil:
			failed++
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
				browser.WrapUntrusted(src, ts, s.Output))
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
	if failed > 0 {
		return fmt.Errorf("%d step(s) failed", failed)
	}
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
