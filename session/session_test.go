package session

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestValidName(t *testing.T) {
	for _, ok := range []string{"a", "trip-booking", "Shop_2"} {
		if !ValidName(ok) {
			t.Errorf("ValidName(%q) = false", ok)
		}
	}
	for _, bad := range []string{"", "../x", "a/b", "-lead", "has space", string(make([]byte, 65))} {
		if ValidName(bad) {
			t.Errorf("ValidName(%q) = true", bad)
		}
	}
}

func TestOpenAndList(t *testing.T) {
	root := t.TempDir()
	if _, err := Open(root, "../escape"); err == nil {
		t.Fatal("Open accepted a path-escaping name")
	}
	for _, n := range []string{"b", "a"} {
		if _, err := Open(root, n); err != nil {
			t.Fatal(err)
		}
	}
	got, err := List(root)
	if err != nil || len(got) != 2 || got[0].Name != "a" || got[1].Name != "b" {
		t.Fatalf("List = %v, %v; want [a b]", got, err)
	}
	if none, err := List(filepath.Join(root, "missing")); err != nil || none != nil {
		t.Fatalf("List(missing) = %v, %v; want nil, nil", none, err)
	}
}

func TestLockExpires(t *testing.T) {
	s, _ := Open(t.TempDir(), "x")
	now := time.Now()
	if err := s.CheckFree(now); err != nil {
		t.Fatalf("fresh session not free: %v", err)
	}
	if err := s.SetLock(Lock{HandoffID: 7, ExpiresAt: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	err := s.CheckFree(now)
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("CheckFree while held = %v; want ErrLocked", err)
	}
	if l, ok := s.HeldBy(now); !ok || l.HandoffID != 7 {
		t.Fatalf("HeldBy = %v, %v", l, ok)
	}
	// A lock past its expiry no longer holds: a dead daemon must not wedge the agent.
	if err := s.CheckFree(now.Add(2 * time.Minute)); err != nil {
		t.Fatalf("expired lock still holds: %v", err)
	}
	s.ClearLock()
	if _, ok := s.HeldBy(now); ok {
		t.Fatal("ClearLock did not release")
	}
}

// fakeDevTools serves the DevTools HTTP endpoints Session reads.
func fakeDevTools(t *testing.T, pages *[]Target) (*Endpoint, *int) {
	t.Helper()
	created := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/json/version", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"webSocketDebuggerUrl": "ws://127.0.0.1/devtools/browser/x"})
	})
	mux.HandleFunc("/json/list", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(*pages)
	})
	mux.HandleFunc("/json/new", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.Error(w, "PUT only", http.StatusMethodNotAllowed)
			return
		}
		created++
		tg := Target{ID: "NEW" + strconv.Itoa(created), Type: "page", URL: "about:blank"}
		*pages = append(*pages, tg)
		json.NewEncoder(w).Encode(tg)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	_, portStr, _ := net.SplitHostPort(srv.Listener.Addr().String())
	port, _ := strconv.Atoi(portStr)
	return &Endpoint{Port: port}, &created
}

func TestCurrentTarget(t *testing.T) {
	ctx := context.Background()
	s, _ := Open(t.TempDir(), "x")
	pages := []Target{
		{ID: "UI", Type: "browser_ui", URL: "chrome://omnibox-popup.top-chrome/"},
		{ID: "A", Type: "page", URL: "https://a.example/"},
		{ID: "B", Type: "page", URL: "https://b.example/"},
	}
	ep, created := fakeDevTools(t, &pages)

	// No recorded tab: the first real page, recorded for next time.
	if id, err := s.CurrentTarget(ctx, ep); err != nil || id != "A" {
		t.Fatalf("CurrentTarget = %q, %v; want A", id, err)
	}
	// A recorded tab that is still open wins.
	s.SetTarget("B")
	if id, _ := s.CurrentTarget(ctx, ep); id != "B" {
		t.Fatalf("CurrentTarget = %q; want recorded B", id)
	}
	// Recorded tab gone → fall back to an open page.
	pages = pages[:2]
	if id, _ := s.CurrentTarget(ctx, ep); id != "A" {
		t.Fatalf("CurrentTarget = %q; want A after B closed", id)
	}
	// No pages at all → open a blank tab.
	pages = pages[:1]
	if id, err := s.CurrentTarget(ctx, ep); err != nil || id != "NEW1" || *created != 1 {
		t.Fatalf("CurrentTarget = %q, %v (created %d); want NEW1", id, err, *created)
	}
}

func TestRunningReadsDevToolsActivePort(t *testing.T) {
	ctx := context.Background()
	s, _ := Open(t.TempDir(), "x")
	if s.Running(ctx) != nil {
		t.Fatal("Running without a port file")
	}
	pages := []Target{}
	ep, _ := fakeDevTools(t, &pages)
	os.WriteFile(filepath.Join(s.Dir, "profile", "DevToolsActivePort"),
		[]byte(strconv.Itoa(ep.Port)+"\n/devtools/browser/x\n"), 0o600)
	got := s.Running(ctx)
	if got == nil || got.Port != ep.Port || got.BrowserWS == "" {
		t.Fatalf("Running = %+v", got)
	}
	// Stale port file (nothing listening) is "not running", not an error.
	os.WriteFile(filepath.Join(s.Dir, "profile", "DevToolsActivePort"), []byte("1\n"), 0o600)
	if s.Running(ctx) != nil {
		t.Fatal("stale port file reported as running")
	}
}
