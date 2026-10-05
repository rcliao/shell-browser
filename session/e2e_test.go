package session

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// TestE2ETabSurvivesDeadline guards the chromedp behaviour Attach works
// around: a context bound to a RemoteAllocator tab closes the tab when it
// ends. A run that hits its deadline must leave the tab open.
//
//	SHELL_E2E_BROWSER=1 go test ./session -run E2E -v
func TestE2ETabSurvivesDeadline(t *testing.T) {
	if os.Getenv("SHELL_E2E_BROWSER") != "1" {
		t.Skip("set SHELL_E2E_BROWSER=1 to run (starts Chrome)")
	}
	root, _ := os.MkdirTemp("", "session-e2e-*")
	defer os.RemoveAll(root)
	ctx := context.Background()
	s, _ := Open(root, "deadline")
	ep, err := s.Ensure(ctx, LaunchOptions{Headless: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		Close(ctx, ep)
		for i := 0; i < 40 && s.Running(ctx) != nil; i++ {
			time.Sleep(250 * time.Millisecond)
		}
	}()
	tid, err := s.CurrentTarget(ctx, ep)
	if err != nil {
		t.Fatal(err)
	}

	// Run 1: navigate, then blow the deadline mid-action.
	dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	runCtx, release, err := Attach(dctx, ep, tid)
	if err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(runCtx, chromedp.Navigate("data:text/html,<title>kept</title>")); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(runCtx, chromedp.Sleep(10*time.Second)); err == nil {
		t.Fatal("sleep outlived the deadline")
	}
	release()
	cancel()
	time.Sleep(500 * time.Millisecond)

	// Run 2: the same tab, same page.
	pages, _ := ep.Pages(ctx)
	found := false
	for _, p := range pages {
		found = found || p.ID == tid
	}
	if !found {
		t.Fatalf("tab %s closed after a deadline; open: %+v", tid, pages)
	}
	runCtx2, release2, err := Attach(ctx, ep, tid)
	if err != nil {
		t.Fatal(err)
	}
	defer release2()
	var title string
	if err := chromedp.Run(runCtx2, chromedp.Title(&title)); err != nil || title != "kept" {
		t.Fatalf("title = %q, %v; want kept", title, err)
	}
}
