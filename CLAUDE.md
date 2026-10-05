# shell-browser

Headless Chrome automation via chromedp, driven by agents that also hold chat
and secret-store access. Treat every page as hostile input.

## Architecture

- `cmd/shell-browser/main.go` — CLI entrypoint (`<url> [action...]`), flag
  parsing, artifact markers for screenshots
- `internal/browser/browser.go` — execution: chromedp context, action dispatch,
  ref targeting, JS gate, post-navigation policy re-checks, result formatting
- `internal/browser/policy.go` — domain allow/deny policy + SSRF guards
- `internal/browser/fetch.go` — HTTP fast path, HTML→text, escalation decision
- `internal/browser/snapshot.go` — accessibility-tree snapshot, refs, candidate
  suggestions
- `internal/browser/untrusted.go` — `<untrusted-page-content>` envelope
- `internal/browser/parse.go` — directive/action parsing
- `browser.go` — public API: type aliases and function re-exports
- `session/` — named Chrome that outlives a run (`--session`): detached launch
  with a loopback debug port, tab selection, human-hold lock, `Attach`
- `liveview/` — streams a session tab (CDP screencast over SSE) and replays a
  person's taps/typing/navigation; used by the shell daemon for handoffs

## Build & Test

```bash
make build    # Build binary
make test     # Run tests
make vet      # Run go vet
```

## Key Patterns

- `BrowserRe` regex matches `[browser url="..."]...[/browser]` blocks
- `ParseDirective(url, body)` extracts URL and actions from block body
- `ExecuteFetchFirst(ctx, cfg, d, forceRender)` is the entrypoint: plain HTTP
  when the page allows it, Chrome otherwise. `Execute` is the Chrome-only path.
- `FormatResults(result)` formats output; page-derived steps get wrapped
- Screenshots returned as `[]byte` in `StepResult.Screenshot`
- Actions: navigate, click, type, wait, screenshot, extract, js, sleep,
  snapshot, text (the first eight are the original set — keep them working,
  the installed skill uses them)

## Invariants (do not regress)

- Session contexts are released with `session.Attach`'s release func, never a
  plain cancel and never `chromedp.Cancel`: chromedp closes RemoteAllocator
  tabs on cancel, and `chromedp.Cancel` closes the whole browser.
- The live view's phone layout resizes the real window; `Close` must restore
  it (`restoreWindow`) so the agent gets the tab back at the size it left it.
- The live view's address bar goes through `Policy.Check`, like every other
  navigation; `/input` and `/done` accept `application/json` only.

- `Policy.Check` runs **before** Chrome is launched and again after any
  navigating action; deny beats allow; explicit `allow` entries are the only
  way past the private-network guard.
- The `js` action is refused unless `--allow-js` or `"allow_js": true`. The
  internal `stealthJS` injection is ours and is unaffected.
- Anything read off a page (`extract`, `text`, `snapshot`, `js` results) is
  wrapped by `WrapUntrusted`, with marker sequences in the payload escaped.
- Never log environment variables, cookies or raw CDP traffic — `WithLogf`
  only, never `chromedp.WithDebugf`. Log URLs through `redactURL`.
- Snapshots are capped (120 elements / 5KB) so they stay usable in an agent's
  context.
- The accessibility result types in `snapshot.go` are deliberately local and
  lenient: cdproto's generated enums reject property values from newer Chrome
  builds and would break `getFullAXTree` entirely.
