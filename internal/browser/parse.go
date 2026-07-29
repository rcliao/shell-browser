// Package browser provides headless Chrome automation via chromedp.
package browser

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// ActionType identifies a browser action.
type ActionType int

const (
	ActionNavigate ActionType = iota
	ActionClick
	ActionType_
	ActionWait
	ActionScreenshot
	ActionExtract
	ActionJS
	ActionSleep
	// ActionSnapshot lists the interactable elements of the current page with
	// stable per-run refs (e1, e2, ...) that click/type accept.
	ActionSnapshot
	// ActionText extracts the whole page as plain text. Servable by the
	// HTTP fast path without launching Chrome.
	ActionText
)

// Action represents a single browser step.
type Action struct {
	Type     ActionType
	Selector string // CSS selector (click, type, wait, extract)
	Value    string // typed text, JS expression, sleep duration, or URL
}

func (a Action) String() string {
	switch a.Type {
	case ActionNavigate:
		return fmt.Sprintf("navigate %q", a.Value)
	case ActionClick:
		return fmt.Sprintf("click %q", a.Selector)
	case ActionType_:
		return fmt.Sprintf("type %q %q", a.Selector, a.Value)
	case ActionWait:
		return fmt.Sprintf("wait %q", a.Selector)
	case ActionScreenshot:
		return "screenshot"
	case ActionExtract:
		return fmt.Sprintf("extract %q", a.Selector)
	case ActionJS:
		return fmt.Sprintf("js %q", a.Value)
	case ActionSleep:
		return fmt.Sprintf("sleep %q", a.Value)
	case ActionSnapshot:
		return "snapshot"
	case ActionText:
		return "text"
	default:
		return "unknown"
	}
}

// Directive holds the parsed URL and action list from a [browser] block.
type Directive struct {
	URL     string
	Actions []Action
	// Unknown holds lines that matched no action. Callers must surface these:
	// a mistyped action word used to fall through to the default text read, so
	// the caller got page content back and believed its action had run.
	Unknown []string
}

// BrowserRe matches [browser url="..."]...[/browser] blocks.
var BrowserRe = regexp.MustCompile(`(?s)\[browser url="([^"]+)"\]\s*(.*?)\s*\[/browser\]`)

// quoted captures a double-quoted string.
var quotedRe = regexp.MustCompile(`"([^"]*)"`)

// ParseDirective extracts the URL and actions from a browser block body.
func ParseDirective(url, body string) Directive {
	d := Directive{URL: url}

	lines := strings.Split(body, "\n")
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}

		switch {
		case line == "navigate":
			d.Actions = append(d.Actions, Action{Type: ActionNavigate, Value: url})

		case line == "snapshot":
			d.Actions = append(d.Actions, Action{Type: ActionSnapshot})

		case line == "text":
			d.Actions = append(d.Actions, Action{Type: ActionText})

		case line == "screenshot":
			d.Actions = append(d.Actions, Action{Type: ActionScreenshot})

		case strings.HasPrefix(line, "click "):
			if v, ok := arg1(line, "click "); ok {
				d.Actions = append(d.Actions, Action{Type: ActionClick, Selector: v})
			}

		case strings.HasPrefix(line, "type "):
			qs := quotedRe.FindAllStringSubmatch(line, 2)
			if len(qs) >= 2 {
				d.Actions = append(d.Actions, Action{Type: ActionType_, Selector: qs[0][1], Value: qs[1][1]})
			} else if sel, val, ok := arg2(line, "type "); ok {
				d.Actions = append(d.Actions, Action{Type: ActionType_, Selector: sel, Value: val})
			}

		case strings.HasPrefix(line, "wait "):
			if v, ok := arg1(line, "wait "); ok {
				d.Actions = append(d.Actions, Action{Type: ActionWait, Selector: v})
			}

		case strings.HasPrefix(line, "extract "):
			if v, ok := arg1(line, "extract "); ok {
				d.Actions = append(d.Actions, Action{Type: ActionExtract, Selector: v})
			}

		case strings.HasPrefix(line, "js "):
			if v, ok := arg1(line, "js "); ok {
				d.Actions = append(d.Actions, Action{Type: ActionJS, Value: v})
			}

		case strings.HasPrefix(line, "sleep "):
			if v, ok := arg1(line, "sleep "); ok {
				d.Actions = append(d.Actions, Action{Type: ActionSleep, Value: v})
			}

		default:
			d.Unknown = append(d.Unknown, line)
		}
	}

	return d
}

// arg1 returns the single argument of a one-argument action line. A
// double-quoted argument is preferred (the historical form); an unquoted
// remainder is accepted so `click e12` works as well as `click "e12"`.
func arg1(line, prefix string) (string, bool) {
	if qs := quotedRe.FindStringSubmatch(line); len(qs) >= 2 {
		return qs[1], true
	}
	v := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	return v, v != ""
}

// arg2 splits an unquoted two-argument action line on the first space.
func arg2(line, prefix string) (string, string, bool) {
	rest := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	sel, val, ok := strings.Cut(rest, " ")
	if !ok || sel == "" {
		return "", "", false
	}
	return sel, strings.TrimSpace(val), true
}

// ParseSleepDuration parses a sleep value like "2s", "500ms" into time.Duration.
func ParseSleepDuration(val string) (time.Duration, error) {
	d, err := time.ParseDuration(val)
	if err != nil {
		return 0, fmt.Errorf("invalid sleep duration %q: %w", val, err)
	}
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d, nil
}
