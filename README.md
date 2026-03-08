# shell-browser

Headless Chrome automation via chromedp. Execute browser directives and return results.

Part of the [Ghost in the Shell](https://github.com/rcliao?tab=repositories&q=shell-) ecosystem.

## Install

```bash
go install github.com/rcliao/shell-browser/cmd/shell-browser@latest
```

## Usage

```bash
# Navigate and take a screenshot
shell-browser "https://example.com" --actions "screenshot"

# Extract content from a page
shell-browser "https://example.com" --actions "extract .main-content"

# Multiple actions
shell-browser "https://example.com" --actions "click #login,type #email user@test.com,screenshot"
```

## Actions

| Action | Description | Example |
|--------|-------------|---------|
| `navigate` | Go to URL | `navigate https://...` |
| `click` | Click element | `click #button` |
| `type` | Type into element | `type #input some text` |
| `wait` | Wait for element | `wait .loaded` |
| `screenshot` | Capture page | `screenshot` |
| `extract` | Extract text by selector | `extract .content` |
| `js` | Execute JavaScript | `js document.title` |
| `sleep` | Wait duration | `sleep 2s` |

## Library Usage

```go
import browser "github.com/rcliao/shell-browser"

cfg := browser.Config{Headless: true, Timeout: 30 * time.Second}
directive := browser.ParseDirective("https://example.com", "screenshot\nextract .content")
result := browser.Execute(ctx, cfg, directive)
fmt.Println(browser.FormatResults(result))
```

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--headless` | true | Run Chrome in headless mode |
| `--timeout` | 30s | Action timeout |
| `--actions` | | Comma-separated actions to execute |

## Build

```bash
make build    # Build binary
make test     # Run tests
make vet      # Run go vet
```

## License

MIT
