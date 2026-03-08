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
	Config     = browser.Config
	Directive  = browser.Directive
	Action     = browser.Action
	ActionType = browser.ActionType
	Result     = browser.Result
	StepResult = browser.StepResult
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

// ParseSleepDuration parses a sleep value like "2s", "500ms" into time.Duration.
func ParseSleepDuration(val string) (time.Duration, error) {
	return browser.ParseSleepDuration(val)
}
