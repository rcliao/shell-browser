package liveview

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rcliao/shell-browser/internal/browser"
)

// bare builds a Viewer with no browser behind it, for handler tests.
func bare(mode Mode) *Viewer {
	ctx, cancel := context.WithCancel(context.Background())
	return &Viewer{opt: Options{Mode: mode, Title: "T <b>", Reason: "R"}, policy: browser.DefaultPolicy(),
		ctx: ctx, cancel: cancel, subs: map[chan []byte]struct{}{}}
}

// withTab gives a bare Viewer a (browserless) tab context so input reaches
// validation; nothing here may actually talk to Chrome.
func withTab(v *Viewer) *Viewer {
	v.tabCtx = context.Background()
	return v
}

func do(v *Viewer, method, path, ctype, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	w := httptest.NewRecorder()
	v.ServeHTTP(w, req)
	return w
}

func TestPageEscapesAndModes(t *testing.T) {
	w := do(bare(ModeHandoff), "GET", "/", "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "T &lt;b&gt;") {
		t.Fatalf("page: %d, title not escaped", w.Code)
	}
	if !strings.Contains(w.Body.String(), `id="done"`) {
		t.Fatal("handoff page has no Done button")
	}
	if strings.Contains(do(bare(ModeWatch), "GET", "/", "", "").Body.String(), `id="done"`) {
		t.Fatal("watch page shows a Done button")
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
}

func TestInputGuards(t *testing.T) {
	cases := []struct {
		name  string
		v     *Viewer
		ctype string
		body  string
		want  int
	}{
		{"watch mode refuses input", bare(ModeWatch), "application/json", `{"t":"back"}`, http.StatusForbidden},
		{"form post refused (no CORS-free JSON)", bare(ModeHandoff), "application/x-www-form-urlencoded", "t=back", http.StatusUnsupportedMediaType},
		{"bad json", bare(ModeHandoff), "application/json", "{", http.StatusBadRequest},
		{"bad layout", withTab(bare(ModeHandoff)), "application/json", `{"t":"layout","layout":"tablet"}`, http.StatusBadRequest},
		{"unknown event", withTab(bare(ModeHandoff)), "application/json", `{"t":"fly"}`, http.StatusBadRequest},
		{"unsupported key", withTab(bare(ModeHandoff)), "application/json", `{"t":"key","k":"F13"}`, http.StatusBadRequest},
	}
	for _, c := range cases {
		if w := do(c.v, "POST", "/input", c.ctype, c.body); w.Code != c.want {
			t.Errorf("%s: got %d want %d (%s)", c.name, w.Code, c.want, w.Body.String())
		}
	}
}

func TestNavPolicyBeforeBrowser(t *testing.T) {
	v := bare(ModeHandoff)
	v.tabCtx = context.Background() // never reached: policy refuses first
	w := do(v, "POST", "/input", "application/json", `{"t":"nav","url":"http://169.254.169.254/latest"}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("metadata address: got %d want 403 (%s)", w.Code, w.Body.String())
	}
}

func TestDoneOnceAndWatchRefused(t *testing.T) {
	v := bare(ModeHandoff)
	v.url = "https://example.com/x"
	got := make(chan Result, 2)
	v.opt.OnDone = func(r Result) { got <- r }
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("POST", "/done", strings.NewReader(`{"note":"  take the blue one  "}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Tailscale-User-Name", "Someone")
		w := httptest.NewRecorder()
		v.ServeHTTP(w, req)
		if w.Code != http.StatusNoContent {
			t.Fatalf("done: %d", w.Code)
		}
	}
	r := <-got
	if r.URL != "https://example.com/x" || r.By != "Someone" || r.Note != "take the blue one" {
		t.Fatalf("result = %+v", r)
	}
	select {
	case <-got:
		t.Fatal("OnDone ran twice")
	default:
	}
	if w := do(bare(ModeWatch), "POST", "/done", "application/json", "{}"); w.Code != http.StatusForbidden {
		t.Fatalf("watch done: %d", w.Code)
	}
}

func TestEndedStreamSaysSo(t *testing.T) {
	v := bare(ModeHandoff)
	v.ended = true
	w := do(v, "GET", "/events", "", "")
	if !strings.Contains(w.Body.String(), "event: ended") {
		t.Fatalf("ended stream body = %q", w.Body.String())
	}
}

func TestBroadcastLatestWins(t *testing.T) {
	v := bare(ModeWatch)
	ch := make(chan []byte, 1)
	v.subs[ch] = struct{}{}
	v.broadcast([]byte("a"))
	v.broadcast([]byte("b")) // subscriber has not read "a": it is replaced
	if got := string(<-ch); got != "b" {
		t.Fatalf("got %q want latest b", got)
	}
}

func TestNormalizeURL(t *testing.T) {
	ok := map[string]string{
		"example.com":          "https://example.com",
		" https://a.b/c?d=1 ":  "https://a.b/c?d=1",
		"http://plain.example": "http://plain.example",
	}
	for in, want := range ok {
		if got, err := normalizeURL(in); err != nil || got != want {
			t.Errorf("normalizeURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "javascript://alert(1)", "file:///etc/passwd", "chrome://settings"} {
		if got, err := normalizeURL(bad); err == nil {
			t.Errorf("normalizeURL(%q) = %q; want error", bad, got)
		}
	}
}

func TestStateCarriesLayout(t *testing.T) {
	v := bare(ModeHandoff)
	v.layout = LayoutPhone
	if got := string(v.stateEventLocked()); !strings.Contains(got, `"layout":"phone"`) {
		t.Fatalf("state = %s", got)
	}
}
