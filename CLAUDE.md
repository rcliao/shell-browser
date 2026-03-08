# shell-browser

Headless Chrome automation via chromedp.

## Architecture

- `cmd/shell-browser/main.go` — Cobra CLI entrypoint
- `internal/browser/browser.go` — Core execution: chromedp context, action dispatch, screenshot capture
- `internal/browser/parse.go` — Directive and action parsing from text
- `internal/browser/browser_test.go` — Tests for parsing and sleep duration
- `browser.go` — Public API: type aliases and function re-exports

## Build & Test

```bash
make build    # Build binary
make test     # Run tests
make vet      # Run go vet
```

## Key Patterns

- `BrowserRe` regex matches `[browser url="..."]...[/browser]` blocks
- `ParseDirective(url, body)` extracts URL and actions from block body
- `Execute(ctx, cfg, directive)` runs actions sequentially, returns `*Result`
- `FormatResults(result)` formats output for LLM consumption
- Screenshots returned as `[]byte` in `StepResult.Data`
- Actions: navigate, click, type, wait, screenshot, extract, js, sleep
