package browser

import (
	"context"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
)

// A bot wall is a page a bot-protection service shows instead of the site.
// It used to come back as ordinary page text with exit 0, so agents read
// "Access Denied" as the site's answer and told the family to check
// themselves. Detecting it turns it into a failure that names the next step:
// a real-window session gets past most walls (Akamai: verified on two retail
// and booking sites), and a person can get past the rest through a handoff.

// Wall describes a detected bot wall.
type Wall struct {
	Vendor   string // akamai, perimeterx, cloudflare, datadome, imperva
	Evidence string // the signature that matched
}

// wallProbeJS gathers what the classifier needs. It is OUR code, run after
// the caller's actions; it only reads.
const wallProbeJS = `(() => {
  const body = document.body ? (document.body.innerText || "") : "";
  const frames = Array.from(document.querySelectorAll("iframe")).map(f => f.src || "").join(" ");
  const ids = ["px-captcha", "challenge-form", "cf-challenge-running", "challenge-running"]
    .filter(id => document.getElementById(id)).join(" ");
  return { title: document.title || "", text: body.slice(0, 2000), frames: frames, ids: ids };
})()`

type wallProbe struct {
	Title  string `json:"title"`
	Text   string `json:"text"`
	Frames string `json:"frames"`
	IDs    string `json:"ids"`
}

// wallSignatures are checked in order; each needs ALL its needles in the
// named field (case-insensitive). Kept specific so a store page that merely
// mentions "access" or has a reCAPTCHA on its login form is not a wall.
var wallSignatures = []struct {
	vendor, field string
	needles       []string
}{
	{"akamai", "text", []string{"access denied", "you don't have permission to access"}},
	{"akamai", "text", []string{"reference #", "errors.edgesuite.net"}},
	{"perimeterx", "text", []string{"are you real?"}},
	{"perimeterx", "text", []string{"press & hold"}},
	{"perimeterx", "ids", []string{"px-captcha"}},
	{"cloudflare", "title", []string{"just a moment"}},
	{"cloudflare", "title", []string{"attention required", "cloudflare"}},
	{"cloudflare", "ids", []string{"challenge"}},
	{"cloudflare", "frames", []string{"challenges.cloudflare.com"}},
	{"cloudflare", "text", []string{"verify you are human by completing the action below"}},
	{"datadome", "frames", []string{"captcha-delivery.com"}},
	{"imperva", "text", []string{"request unsuccessful", "incapsula incident id"}},
}

// classifyWall returns the wall the probe shows, or nil.
func classifyWall(p wallProbe) *Wall {
	fields := map[string]string{
		"title":  strings.ToLower(p.Title),
		"text":   strings.ToLower(p.Text),
		"frames": strings.ToLower(p.Frames),
		"ids":    strings.ToLower(p.IDs),
	}
	for _, sig := range wallSignatures {
		hay := fields[sig.field]
		ok := hay != ""
		for _, n := range sig.needles {
			if !strings.Contains(hay, n) {
				ok = false
				break
			}
		}
		if ok {
			return &Wall{Vendor: sig.vendor, Evidence: sig.field + ": " + strings.Join(sig.needles, " + ")}
		}
	}
	return nil
}

// detectWall probes the current page. A probe that fails is "no wall": this
// is advice, never a reason to fail a run on its own.
func (e *executor) detectWall() *Wall {
	ctx, cancel := context.WithTimeout(e.ctx, 3*time.Second)
	defer cancel()
	var p wallProbe
	if err := chromedp.Run(ctx, chromedp.Evaluate(wallProbeJS, &p)); err != nil {
		return nil
	}
	return classifyWall(p)
}

// NextStep is what the caller should try after hitting w.
func (w *Wall) NextStep(session string) string {
	if session == "" {
		return "retry with --session <name>: a real browser window gets past most bot walls. " +
			"If it is still blocked, hand the tab to a person with shell_browser(action=\"handoff\", session=<name>, reason=…)."
	}
	return "hand the tab to a person: shell_browser(action=\"handoff\", session=\"" + session +
		"\", reason=\"<what to do, e.g. press and hold the button>\"), then end your turn. Do not tell the family the site cannot be read."
}
