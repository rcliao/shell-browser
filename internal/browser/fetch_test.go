package browser

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func longHTML(paras int) string {
	var sb strings.Builder
	sb.WriteString("<html><head><title>t</title><style>body{color:red}</style></head><body>")
	for i := 0; i < paras; i++ {
		sb.WriteString("<p>The user asked for the opening hours and the address of the shop.</p>")
	}
	sb.WriteString("<script>var x = 1;</script></body></html>")
	return sb.String()
}

func TestHTMLToText(t *testing.T) {
	got := HTMLToText(`<html><head><style>.a{}</style></head><body>
		<h1>Hours &amp; Info</h1><script>alert(1)</script>
		<p>Open   10am</p><p>Closed Monday</p></body></html>`)
	if strings.Contains(got, "alert(1)") || strings.Contains(got, ".a{}") {
		t.Fatalf("script/style leaked into text: %q", got)
	}
	if !strings.Contains(got, "Hours & Info") {
		t.Fatalf("entity not unescaped: %q", got)
	}
	if !strings.Contains(got, "Open 10am") {
		t.Fatalf("whitespace not collapsed: %q", got)
	}
	if strings.Contains(got, "<") {
		t.Fatalf("tags left in text: %q", got)
	}
}

func TestShouldEscalate(t *testing.T) {
	cases := []struct {
		name string
		res  *FetchResult
		err  error
		want bool
	}{
		{"transport error", nil, errors.New("boom"), true},
		{"nil result", nil, nil, true},
		{"http 403", &FetchResult{Status: 403, Text: strings.Repeat("a", 5000)}, nil, true},
		{"empty body", &FetchResult{Status: 200, Text: "loading..."}, nil, true},
		{"thin body with scripts", &FetchResult{Status: 200, Text: strings.Repeat("x ", 60), HasScripts: true}, nil, true},
		{"small static page", &FetchResult{Status: 200, Text: strings.Repeat("x ", 60)}, nil, false},
		{"js gated", &FetchResult{Status: 200, Text: "You need to enable JavaScript to run this app. " + strings.Repeat("x ", 300)}, nil, true},
		{"good page", &FetchResult{Status: 200, Text: strings.Repeat("real content ", 100)}, nil, false},
	}
	for _, c := range cases {
		got, why := ShouldEscalate(c.res, c.err)
		if got != c.want {
			t.Errorf("%s: ShouldEscalate = %v (%s), want %v", c.name, got, why, c.want)
		}
		if got && why == "" {
			t.Errorf("%s: escalation without a reason", c.name)
		}
	}
}

func TestNeedsBrowser(t *testing.T) {
	textOnly := ParseDirective("https://example.com", "text")
	if NeedsBrowser(textOnly.Actions) {
		t.Error("a text-only run should not need a browser")
	}
	if NeedsBrowser(nil) {
		t.Error("an empty run should not need a browser")
	}
	for _, body := range []string{`click "#a"`, "screenshot", "snapshot", `extract ".x"`, `js "1"`, `wait "#a"`} {
		d := ParseDirective("https://example.com", body)
		if !NeedsBrowser(d.Actions) {
			t.Errorf("action %q should require a browser", body)
		}
	}
}

func TestFetchText_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(longHTML(20)))
	}))
	defer srv.Close()

	p := DefaultPolicy()
	p.AllowDomain("127.0.0.1") // the test server is on loopback by definition

	res, err := FetchText(t.Context(), p, srv.URL, 5*time.Second)
	if err != nil {
		t.Fatalf("FetchText: %v", err)
	}
	if res.Status != 200 {
		t.Fatalf("status = %d", res.Status)
	}
	if !strings.Contains(res.Text, "opening hours") {
		t.Fatalf("text missing content: %q", res.Text)
	}
	if escalate, why := ShouldEscalate(res, nil); escalate {
		t.Fatalf("a full HTML page should not escalate: %s", why)
	}
}

func TestFetchText_PolicyBlocksBeforeRequest(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer srv.Close()

	// Default policy blocks loopback, so the request must never be made.
	if _, err := FetchText(t.Context(), DefaultPolicy(), srv.URL, time.Second); err == nil {
		t.Fatal("expected a policy error")
	}
	if hit {
		t.Fatal("blocked fetch still reached the server")
	}
}

func TestFetchText_RedirectIsRechecked(t *testing.T) {
	// An allowed host redirecting to a private destination must be stopped.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer srv.Close()

	p := DefaultPolicy()
	p.AllowDomain("127.0.0.1")

	_, err := FetchText(t.Context(), p, srv.URL, 5*time.Second)
	if err == nil {
		t.Fatal("redirect to link-local should be blocked")
	}
	if !strings.Contains(err.Error(), "browser domain policy") {
		t.Fatalf("redirect block should name the policy, got %v", err)
	}
}

func TestExecuteFetchFirst_ServesTextWithoutChrome(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(longHTML(20)))
	}))
	defer srv.Close()

	p := DefaultPolicy()
	p.AllowDomain("127.0.0.1")
	cfg := Config{Headless: true, TimeoutSeconds: 10, Policy: p}

	r := ExecuteFetchFirst(t.Context(), cfg, ParseDirective(srv.URL, "text"), false)
	if r.Mode != "http" {
		t.Fatalf("mode = %q, want http (escalation: %s)", r.Mode, r.Escalation)
	}
	out := FormatResults(r)
	if !strings.Contains(out, "<untrusted-page-content") {
		t.Fatalf("fetched text must be wrapped:\n%s", out)
	}
}

func TestExecuteFetchFirst_PolicyBlockDoesNotEscalate(t *testing.T) {
	cfg := Config{Headless: true, TimeoutSeconds: 5, Policy: DefaultPolicy()}
	r := ExecuteFetchFirst(t.Context(), cfg, Directive{URL: "http://10.1.2.3/secret"}, false)
	if r.Mode != "http" {
		t.Fatalf("a policy block must not fall through to Chrome (mode=%q)", r.Mode)
	}
	if len(r.Steps) != 1 || r.Steps[0].Err == nil {
		t.Fatalf("expected one failed step, got %+v", r.Steps)
	}
}
