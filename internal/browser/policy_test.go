package browser

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPolicy_DefaultAllowsPublicSites(t *testing.T) {
	p := DefaultPolicy()
	p.lookupIP = func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("93.184.216.34")}, nil }

	for _, u := range []string{
		"https://example.com/products",
		"http://shop.example.co.uk/cart?x=1",
		"https://maps.google.com/",
	} {
		if err := p.Check(u); err != nil {
			t.Errorf("Check(%q) = %v, want allowed", u, err)
		}
	}
}

func TestPolicy_BlocksSSRFShapes(t *testing.T) {
	p := DefaultPolicy()
	p.lookupIP = func(string) ([]net.IP, error) { return nil, errors.New("no dns in test") }

	cases := []struct {
		url  string
		want string // substring expected in the reason
	}{
		{"http://localhost:8080/admin", "loopback"},
		{"http://127.0.0.1/", "loopback"},
		{"http://127.1.2.3/", "loopback"},
		{"http://[::1]/", "loopback"},
		{"http://169.254.169.254/latest/meta-data/", "link-local"},
		{"http://10.0.0.5/", "private"},
		{"http://172.16.4.4/", "private"},
		{"http://192.168.1.1/", "private"},
		{"http://100.64.3.9/", "carrier-grade"},
		{"http://0.0.0.0/", "unspecified"},
		{"http://metadata.google.internal/", "deny list"},
		{"http://printer.local/", "deny list"},
	}
	for _, tc := range cases {
		err := p.Check(tc.url)
		if err == nil {
			t.Errorf("Check(%q) = nil, want blocked", tc.url)
			continue
		}
		var pe *PolicyError
		if !errors.As(err, &pe) {
			t.Errorf("Check(%q) error type = %T, want *PolicyError", tc.url, err)
			continue
		}
		if !strings.Contains(pe.Reason, tc.want) {
			t.Errorf("Check(%q) reason = %q, want it to mention %q", tc.url, pe.Reason, tc.want)
		}
		if !strings.Contains(err.Error(), "policy") {
			t.Errorf("Check(%q) message %q should name the policy", tc.url, err)
		}
	}
}

func TestPolicy_BlocksNonHTTPSchemes(t *testing.T) {
	p := DefaultPolicy()
	for _, u := range []string{
		"file:///etc/passwd",
		"chrome://settings",
		"data:text/html,<h1>hi</h1>",
		"ftp://example.com/x",
	} {
		if err := p.Check(u); err == nil {
			t.Errorf("Check(%q) = nil, want blocked", u)
		}
	}
}

func TestPolicy_HostnameResolvingToPrivateIsBlocked(t *testing.T) {
	p := DefaultPolicy()
	p.lookupIP = func(host string) ([]net.IP, error) {
		if host == "rebind.example.com" {
			return []net.IP{net.ParseIP("192.168.0.9")}, nil
		}
		return []net.IP{net.ParseIP("1.2.3.4")}, nil
	}
	if err := p.Check("https://rebind.example.com/"); err == nil {
		t.Fatal("hostname resolving to RFC1918 should be blocked")
	}
	if err := p.Check("https://ok.example.com/"); err != nil {
		t.Fatalf("public hostname should be allowed, got %v", err)
	}
}

func TestPolicy_ExplicitAllowOverridesPrivateGuard(t *testing.T) {
	p := DefaultPolicy()
	if err := p.Check("http://192.168.1.50:8080/"); err == nil {
		t.Fatal("private address should be blocked by default")
	}
	p.AllowDomain("192.168.1.50")
	if err := p.Check("http://192.168.1.50:8080/"); err != nil {
		t.Fatalf("explicit --allow should permit it, got %v", err)
	}
}

func TestPolicy_DenyBeatsAllow(t *testing.T) {
	p := DefaultPolicy()
	p.Allow = []string{"example.com"}
	p.Deny = append(p.Deny, "ads.example.com")
	if err := p.Check("https://ads.example.com/x"); err == nil {
		t.Fatal("deny must win over allow")
	}
	if err := p.Check("https://www.example.com/x"); err != nil {
		t.Fatalf("allowed host blocked: %v", err)
	}
}

func TestPolicy_DefaultDenyMode(t *testing.T) {
	p := DefaultPolicy()
	p.Default = "deny"
	p.Allow = []string{"*.shop.example"}
	if err := p.Check("https://www.shop.example/"); err != nil {
		t.Fatalf("allow-listed subdomain blocked: %v", err)
	}
	if err := p.Check("https://shop.example/"); err == nil {
		t.Fatal("`*.` pattern must not match the apex under default deny")
	}
	if err := p.Check("https://other.example/"); err == nil {
		t.Fatal("non-allow-listed host should be denied under default deny")
	}
}

func TestMatchHost(t *testing.T) {
	cases := []struct {
		pat, host string
		want      bool
	}{
		{"example.com", "example.com", true},
		{"example.com", "www.example.com", true},
		{"example.com", "notexample.com", false},
		{"*.example.com", "a.example.com", true},
		{"*.example.com", "example.com", false},
		{"*", "anything.test", true},
		{"", "example.com", false},
	}
	for _, c := range cases {
		if got := matchHost(c.pat, c.host); got != c.want {
			t.Errorf("matchHost(%q, %q) = %v, want %v", c.pat, c.host, got, c.want)
		}
	}
}

func TestLoadPolicy_MissingFileUsesDefaults(t *testing.T) {
	p, err := LoadPolicy(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	if p.Default != "allow" || !p.blockPrivate() {
		t.Fatalf("missing file should yield defaults, got %+v", p)
	}
}

func TestLoadPolicy_MergesWithDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, PolicyFileName)
	body := `{"version":1,"allow":["intranet.example"],"deny":["tracker.example"],"allow_js":true}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := LoadPolicy(path)
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	if !p.AllowJS {
		t.Error("allow_js from file not honoured")
	}
	if !p.blockPrivate() {
		t.Error("a partial file must not disable the private-network guard")
	}
	if err := p.Check("https://tracker.example/"); err == nil {
		t.Error("file deny entry not applied")
	}
	// Built-in deny entries survive a file that only adds its own.
	if err := p.Check("https://metadata.google.internal/"); err == nil {
		t.Error("built-in deny entry was lost when merging the file")
	}
}

func TestLoadPolicy_BadJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), PolicyFileName)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPolicy(path); err == nil {
		t.Fatal("expected an error for malformed policy JSON")
	}
}

func TestExecute_BlockedURLNeverLaunchesChrome(t *testing.T) {
	// A blocked destination must fail before any Chrome process starts, which
	// also makes this test fast and hermetic.
	cfg := Config{Headless: true, TimeoutSeconds: 5, Policy: DefaultPolicy()}
	r := Execute(t.Context(), cfg, Directive{URL: "http://169.254.169.254/latest/meta-data/"})
	if len(r.Steps) != 1 || r.Steps[0].Err == nil {
		t.Fatalf("expected a single failed step, got %+v", r.Steps)
	}
	if !strings.Contains(r.Steps[0].Err.Error(), "browser domain policy") {
		t.Fatalf("error should name the policy: %v", r.Steps[0].Err)
	}
}

func TestRedactURL(t *testing.T) {
	got := redactURL("https://user:hunter2@example.com/x?api_token=abc&q=shoes")
	if strings.Contains(got, "hunter2") || strings.Contains(got, "abc") {
		t.Fatalf("redactURL leaked a secret: %s", got)
	}
	if !strings.Contains(got, "q=shoes") {
		t.Fatalf("redactURL dropped a harmless param: %s", got)
	}
}

func TestSanitizeProfileName(t *testing.T) {
	cases := map[string]string{
		"shop":           "shop",
		"../../etc":      "etc",
		"a b/c":          "abc",
		"my_profile-1":   "my_profile-1",
		"...":            "",
		"/absolute/path": "absolutepath",
	}
	for in, want := range cases {
		if got := sanitizeProfileName(in); got != want {
			t.Errorf("sanitizeProfileName(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := ProfileDir("...."); err == nil {
		t.Error("empty sanitised profile name should error")
	}
}
