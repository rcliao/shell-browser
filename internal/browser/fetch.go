package browser

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Fetch-first tuning knobs.
const (
	// maxFetchBytes caps how much of a response body is read.
	maxFetchBytes = 4 << 20 // 4 MiB
	// minUsableText is the amount of extracted text below which a
	// script-bearing page is assumed to be client-rendered.
	minUsableText = 400
	// hardMinText is the amount below which a page is treated as empty
	// regardless of how it is built.
	hardMinText = 64
)

// FetchResult is the outcome of the plain-HTTP fast path.
type FetchResult struct {
	URL         string    // final URL after redirects
	Status      int       // HTTP status code
	ContentType string    //  Content-Type header
	Text        string    // extracted, whitespace-collapsed page text
	Retrieved   time.Time // when the body was read
	Truncated   bool      // body hit maxFetchBytes
	HasScripts  bool      // the document contains <script> elements
}

// FetchText retrieves url over plain HTTP (no browser) and extracts text.
// Every hop, including each redirect target, is checked against the policy.
func FetchText(ctx context.Context, policy *Policy, rawURL string, timeout time.Duration) (*FetchResult, error) {
	if policy == nil {
		policy = DefaultPolicy()
	}
	if err := policy.Check(rawURL); err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = 20 * time.Second
	}

	client := &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("stopped after 10 redirects")
			}
			// Re-check on every redirect: an allowed host must not be able to
			// bounce the fetch into a private-network destination.
			return policy.CheckURL(req.URL)
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", defaultUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// The final URL is authoritative; check it once more.
	if resp.Request != nil && resp.Request.URL != nil {
		if err := policy.CheckURL(resp.Request.URL); err != nil {
			return nil, err
		}
	}

	limited := io.LimitReader(resp.Body, maxFetchBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	truncated := len(body) > maxFetchBytes
	if truncated {
		body = body[:maxFetchBytes]
	}

	res := &FetchResult{
		URL:         rawURL,
		Status:      resp.StatusCode,
		ContentType: resp.Header.Get("Content-Type"),
		Retrieved:   time.Now(),
		Truncated:   truncated,
	}
	if resp.Request != nil && resp.Request.URL != nil {
		res.URL = resp.Request.URL.String()
	}

	res.HasScripts = scriptRe.Match(body)

	ct := strings.ToLower(res.ContentType)
	switch {
	case strings.Contains(ct, "html"), strings.Contains(ct, "xml"), ct == "":
		res.Text = HTMLToText(string(body))
	case strings.HasPrefix(ct, "text/"), strings.Contains(ct, "json"):
		res.Text = collapseWhitespace(string(body))
	default:
		res.Text = "" // binary payload: nothing useful without a browser
	}
	return res, nil
}

// ShouldEscalate reports whether the plain-HTTP result is too thin to be
// trusted as the page's content, meaning Chrome should render it instead.
func ShouldEscalate(r *FetchResult, err error) (bool, string) {
	if err != nil {
		return true, fmt.Sprintf("http fetch failed (%v)", err)
	}
	if r == nil {
		return true, "no http response"
	}
	if r.Status >= 400 {
		return true, fmt.Sprintf("http status %d", r.Status)
	}
	if len(r.Text) < hardMinText {
		return true, fmt.Sprintf("only %d chars of text in the raw response", len(r.Text))
	}
	// A small page with no scripts is simply a small page; a small page full of
	// scripts is almost certainly client-rendered and worth a real browser.
	if len(r.Text) < minUsableText && r.HasScripts {
		return true, fmt.Sprintf("only %d chars of text (threshold %d) and the page ships JavaScript, so it is probably client-rendered", len(r.Text), minUsableText)
	}
	if looksJSGated(r.Text) {
		return true, "page says it requires JavaScript"
	}
	return false, ""
}

var scriptRe = regexp.MustCompile(`(?i)<script[\s>]`)

var jsGateRe = regexp.MustCompile(`(?i)(enable javascript|javascript is (required|disabled)|requires javascript|please enable js|<noscript>)`)

func looksJSGated(text string) bool {
	// Only consider the gate marker meaningful on short pages: long pages that
	// merely mention JavaScript are fine.
	if len(text) > 4000 {
		return false
	}
	return jsGateRe.MatchString(text)
}

// NeedsBrowser reports whether any action in the list requires a real browser.
// A run of only text-shaped actions can be served by the HTTP fast path.
func NeedsBrowser(actions []Action) bool {
	for _, a := range actions {
		switch a.Type {
		case ActionNavigate, ActionText:
			// text-only, servable over plain HTTP
		default:
			return true
		}
	}
	return false
}

var (
	// RE2 has no backreferences, so each dropped element gets its own pattern.
	dropElems  = []string{"script", "style", "svg", "head", "template", "iframe"}
	dropElemRe = compileDropElems()
	commentRe  = regexp.MustCompile(`(?s)<!--.*?-->`)
	breakRe    = regexp.MustCompile(`(?i)</?(p|div|br|li|tr|h[1-6]|section|article|header|footer|ul|ol|table|blockquote)\b[^>]*>`)
	tagRe      = regexp.MustCompile(`(?s)<[^>]*>`)
	wsRe       = regexp.MustCompile(`[ \t\x{00a0}]+`)
	blankRe    = regexp.MustCompile(`\n\s*\n\s*\n+`)
)

// HTMLToText converts an HTML document to readable plain text using only the
// standard library (no HTML parser dependency). It is deliberately lossy: the
// goal is "is there usable content here", not fidelity.
func HTMLToText(doc string) string {
	s := commentRe.ReplaceAllString(doc, " ")
	for _, re := range dropElemRe {
		s = re.ReplaceAllString(s, " ")
	}
	s = breakRe.ReplaceAllString(s, "\n")
	s = tagRe.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	return collapseWhitespace(s)
}

func compileDropElems() []*regexp.Regexp {
	out := make([]*regexp.Regexp, 0, len(dropElems))
	for _, tag := range dropElems {
		out = append(out, regexp.MustCompile(`(?is)<`+tag+`\b[^>]*>.*?</\s*`+tag+`\s*>`))
	}
	return out
}

func collapseWhitespace(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = wsRe.ReplaceAllString(s, " ")
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = strings.TrimSpace(ln)
	}
	s = strings.Join(lines, "\n")
	s = blankRe.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}
