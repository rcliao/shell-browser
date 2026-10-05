package browser

import (
	"strings"
	"testing"
)

func TestClassifyWall(t *testing.T) {
	cases := []struct {
		name   string
		p      wallProbe
		vendor string // "" = not a wall
	}{
		{"akamai access denied", wallProbe{Title: "Access Denied", Text: "Access Denied\nYou don't have permission to access \"http://www.shop.example/\" on this server.\nReference #18.abc"}, "akamai"},
		{"perimeterx are you real", wallProbe{Text: "Are You Real?\nPlease Click Below To Continue."}, "perimeterx"},
		{"perimeterx press and hold", wallProbe{Text: "Before we continue...\nPress & Hold to confirm you are a human"}, "perimeterx"},
		{"perimeterx element", wallProbe{IDs: "px-captcha"}, "perimeterx"},
		{"cloudflare interstitial", wallProbe{Title: "Just a moment...", Text: "Checking your browser"}, "cloudflare"},
		{"cloudflare turnstile frame", wallProbe{Frames: "https://challenges.cloudflare.com/cdn-cgi/challenge-platform/x"}, "cloudflare"},
		{"datadome", wallProbe{Frames: "https://geo.captcha-delivery.com/captcha/?x=1"}, "datadome"},
		{"imperva", wallProbe{Text: "Request unsuccessful. Incapsula incident ID: 123"}, "imperva"},

		{"normal store page", wallProbe{Title: "Shop Online", Text: "Sign In\nCreate Account\nFree shipping on orders over $20"}, ""},
		{"page that mentions access", wallProbe{Title: "Account access", Text: "Access denied? Reset your password here."}, ""},
		{"login form with recaptcha", wallProbe{Title: "Sign in", Text: "Email\nPassword", Frames: "https://www.google.com/recaptcha/api2/anchor?k=x"}, ""},
		{"empty", wallProbe{}, ""},
	}
	for _, c := range cases {
		w := classifyWall(c.p)
		got := ""
		if w != nil {
			got = w.Vendor
		}
		if got != c.vendor {
			t.Errorf("%s: got %q want %q", c.name, got, c.vendor)
		}
	}
}

func TestWallNextStepNamesTheRung(t *testing.T) {
	w := &Wall{Vendor: "akamai"}
	if s := w.NextStep(""); !strings.Contains(s, "--session") || !strings.Contains(s, "shell_browser") {
		t.Errorf("no-session advice: %s", s)
	}
	if s := w.NextStep("shop"); !strings.Contains(s, `session="shop"`) || !strings.Contains(s, "handoff") {
		t.Errorf("session advice: %s", s)
	}
}
