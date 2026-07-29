// Package shellbrowser provides headless Chrome automation via chromedp.
//
// This package re-exports the key types and functions from the internal browser
// package for use as a library.
package shellbrowser

import (
	"context"
	"regexp"
	"time"

	"github.com/rcliao/shell-browser/internal/browser"
)

// Type aliases for the key types.
type (
	Config      = browser.Config
	Directive   = browser.Directive
	Action      = browser.Action
	ActionType  = browser.ActionType
	Result      = browser.Result
	StepResult  = browser.StepResult
	Policy      = browser.Policy
	Snapshot    = browser.Snapshot
	Element     = browser.Element
	FetchResult = browser.FetchResult
)

// Action type constants.
const (
	ActionNavigate   = browser.ActionNavigate
	ActionClick      = browser.ActionClick
	ActionType_      = browser.ActionType_
	ActionWait       = browser.ActionWait
	ActionScreenshot = browser.ActionScreenshot
	ActionExtract    = browser.ActionExtract
	ActionJS         = browser.ActionJS
	ActionSleep      = browser.ActionSleep
	ActionSnapshot   = browser.ActionSnapshot
	ActionText       = browser.ActionText
)

// BrowserRe matches [browser url="..."]...[/browser] blocks.
var BrowserRe *regexp.Regexp = browser.BrowserRe

// Execute runs a browser directive: navigates to the URL and performs each action.
func Execute(ctx context.Context, cfg Config, d Directive) *Result {
	return browser.Execute(ctx, cfg, d)
}

// ParseDirective extracts the URL and actions from a browser block body.
func ParseDirective(url, body string) Directive {
	return browser.ParseDirective(url, body)
}

// FormatResults formats the result for feeding back to Claude.
func FormatResults(r *Result) string {
	return browser.FormatResults(r)
}

// ExecuteFetchFirst tries a plain HTTP fetch before falling back to Chrome.
func ExecuteFetchFirst(ctx context.Context, cfg Config, d Directive, forceRender bool) *Result {
	return browser.ExecuteFetchFirst(ctx, cfg, d, forceRender)
}

// DefaultPolicy returns the built-in domain policy.
func DefaultPolicy() *Policy { return browser.DefaultPolicy() }

// LoadPolicy loads a domain policy file (missing file yields DefaultPolicy).
func LoadPolicy(path string) (*Policy, error) { return browser.LoadPolicy(path) }

// PolicyPath is the default policy file location.
func PolicyPath() string { return browser.PolicyPath() }

// WrapUntrusted wraps page-derived text in provenance markers.
func WrapUntrusted(src string, retrieved time.Time, content string) string {
	return browser.WrapUntrusted(src, retrieved, content)
}

// FetchText retrieves a URL over plain HTTP and extracts its text.
func FetchText(ctx context.Context, p *Policy, url string, timeout time.Duration) (*FetchResult, error) {
	return browser.FetchText(ctx, p, url, timeout)
}

// ParseSleepDuration parses a sleep value like "2s", "500ms" into time.Duration.
func ParseSleepDuration(val string) (time.Duration, error) {
	return browser.ParseSleepDuration(val)
}
