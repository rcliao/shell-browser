package browser

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// PolicyFileName is the default on-disk location (relative to the shell config
// dir) of the browser domain policy.
const PolicyFileName = "browser-policy.json"

// Policy describes which destinations this tool is allowed to reach.
//
// On-disk shape (~/.shell/browser-policy.json):
//
//	{
//	  "version": 1,
//	  "default": "allow",
//	  "allow": ["example.com", "*.shop.example"],
//	  "deny": ["ads.example.com", "*.internal"],
//	  "allow_schemes": ["http", "https"],
//	  "block_private_networks": true,
//	  "resolve_dns": true,
//	  "allow_js": false
//	}
//
// Matching rules for entries in "allow"/"deny":
//   - "example.com"   matches example.com AND any subdomain of it
//   - "*.example.com" matches subdomains only (not the apex)
//   - "*"             matches everything
//
// Evaluation order (first match wins):
//  1. scheme must be in allow_schemes
//  2. deny list  -> blocked (deny always wins)
//  3. allow list -> allowed (explicit opt-in; overrides the private-network guard)
//  4. private/loopback/link-local destination + block_private_networks -> blocked
//  5. default ("allow" or "deny")
type Policy struct {
	Version              int      `json:"version"`
	Default              string   `json:"default"`
	Allow                []string `json:"allow"`
	Deny                 []string `json:"deny"`
	AllowSchemes         []string `json:"allow_schemes"`
	BlockPrivateNetworks *bool    `json:"block_private_networks"`
	ResolveDNS           *bool    `json:"resolve_dns"`
	AllowJS              bool     `json:"allow_js"`

	// lookupIP is swappable in tests. nil means net.LookupIP.
	lookupIP func(host string) ([]net.IP, error)
}

// DefaultPolicy returns the built-in policy: allow everything except
// SSRF-shaped destinations (loopback, RFC1918, link-local/cloud metadata).
// The family browses arbitrary shopping/travel sites, so a global allowlist
// would be unusable; the guard is on the dangerous shapes instead.
func DefaultPolicy() *Policy {
	t := true
	return &Policy{
		Version:              1,
		Default:              "allow",
		AllowSchemes:         []string{"http", "https"},
		BlockPrivateNetworks: &t,
		ResolveDNS:           &t,
		Deny: []string{
			"metadata.google.internal",
			"metadata.goog",
			"*.internal",
			"*.local",
		},
	}
}

// PolicyPath returns the default policy file path (~/.shell/browser-policy.json).
func PolicyPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".shell", PolicyFileName)
}

// LoadPolicy reads the policy at path. A missing file yields DefaultPolicy.
// Fields absent from the file fall back to the default policy's values, so a
// file containing only {"allow_js": true} still keeps the SSRF guards.
func LoadPolicy(path string) (*Policy, error) {
	p := DefaultPolicy()
	if path == "" {
		return p, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read browser policy %s: %w", path, err)
	}

	var file Policy
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse browser policy %s: %w", path, err)
	}

	if file.Default != "" {
		p.Default = file.Default
	}
	if len(file.AllowSchemes) > 0 {
		p.AllowSchemes = file.AllowSchemes
	}
	if file.BlockPrivateNetworks != nil {
		p.BlockPrivateNetworks = file.BlockPrivateNetworks
	}
	if file.ResolveDNS != nil {
		p.ResolveDNS = file.ResolveDNS
	}
	if file.Version != 0 {
		p.Version = file.Version
	}
	p.Allow = append(p.Allow, file.Allow...)
	// A file deny list is additive to the built-in one; entries are never removed.
	p.Deny = append(p.Deny, file.Deny...)
	p.AllowJS = file.AllowJS
	return p, nil
}

// AllowDomain adds an explicit opt-in entry (from --allow).
func (p *Policy) AllowDomain(d string) {
	d = strings.TrimSpace(strings.ToLower(d))
	if d != "" {
		p.Allow = append(p.Allow, d)
	}
}

func (p *Policy) blockPrivate() bool {
	return p.BlockPrivateNetworks == nil || *p.BlockPrivateNetworks
}

func (p *Policy) resolveDNS() bool {
	return p.ResolveDNS == nil || *p.ResolveDNS
}

func (p *Policy) lookup(host string) ([]net.IP, error) {
	if p.lookupIP != nil {
		return p.lookupIP(host)
	}
	return net.LookupIP(host)
}

// PolicyError is returned when a destination is blocked. It names the policy
// rule that produced the decision so the caller can act on it.
type PolicyError struct {
	URL    string
	Host   string
	Rule   string // the matched rule, e.g. `deny "*.internal"`
	Reason string
}

func (e *PolicyError) Error() string {
	return fmt.Sprintf("blocked by browser domain policy: %s (host %q, rule: %s). "+
		"Policy file: %s. To allow this destination explicitly, pass --allow %s or add it to the policy \"allow\" list.",
		e.Reason, e.Host, e.Rule, PolicyPath(), e.Host)
}

// Check evaluates rawURL against the policy. It returns a *PolicyError when the
// destination is blocked, and nil when navigation may proceed.
func (p *Policy) Check(rawURL string) error {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return &PolicyError{URL: rawURL, Rule: "url-parse", Reason: fmt.Sprintf("unparseable URL %q", rawURL)}
	}
	return p.CheckURL(u)
}

// CheckURL is Check for an already-parsed URL.
func (p *Policy) CheckURL(u *url.URL) error {
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())

	if scheme == "" || host == "" {
		return &PolicyError{URL: u.String(), Host: host, Rule: "url-parse",
			Reason: fmt.Sprintf("URL %q has no scheme or host", u.String())}
	}

	// 1. scheme allowlist (blocks file://, data:, chrome://, javascript: ...)
	if !containsFold(p.AllowSchemes, scheme) {
		return &PolicyError{URL: u.String(), Host: host,
			Rule:   fmt.Sprintf("allow_schemes %v", p.AllowSchemes),
			Reason: fmt.Sprintf("scheme %q is not permitted", scheme)}
	}

	// 2. deny list wins over everything.
	if pat, ok := matchAny(p.Deny, host); ok {
		return &PolicyError{URL: u.String(), Host: host,
			Rule:   fmt.Sprintf("deny %q", pat),
			Reason: "destination is on the policy deny list"}
	}

	// 3. explicit allow entries are an opt-in and bypass the private guard.
	if _, ok := matchAny(p.Allow, host); ok {
		return nil
	}

	// 4. SSRF-shaped destinations.
	if p.blockPrivate() {
		if reason, bad := p.privateReason(host); bad {
			return &PolicyError{URL: u.String(), Host: host,
				Rule:   "block_private_networks",
				Reason: reason}
		}
	}

	// 5. default.
	if strings.EqualFold(p.Default, "deny") {
		return &PolicyError{URL: u.String(), Host: host,
			Rule:   `default "deny"`,
			Reason: "destination is not on the policy allow list"}
	}
	return nil
}

// privateReason reports whether host is an SSRF-shaped destination.
func (p *Policy) privateReason(host string) (string, bool) {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return "loopback hostname (localhost)", true
	}

	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		if reason, bad := privateIPReason(ip); bad {
			return reason, true
		}
		return "", false
	}

	if !p.resolveDNS() {
		return "", false
	}
	ips, err := p.lookup(host)
	if err != nil {
		// DNS failure is not a policy decision; let the navigation fail naturally.
		return "", false
	}
	for _, ip := range ips {
		if reason, bad := privateIPReason(ip); bad {
			return fmt.Sprintf("hostname resolves to a %s (%s)", reason, ip), true
		}
	}
	return "", false
}

// cgnat is the RFC 6598 shared address space, not covered by IsPrivate.
var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

func privateIPReason(ip net.IP) (string, bool) {
	switch {
	case ip.IsLoopback():
		return "loopback address", true
	case ip.IsUnspecified():
		return "unspecified address", true
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
		// 169.254.169.254 (cloud instance metadata) lands here.
		return "link-local address", true
	case ip.IsPrivate():
		return "private (RFC1918/ULA) address", true
	case ip.IsInterfaceLocalMulticast(), ip.IsMulticast():
		return "multicast address", true
	case cgnat.Contains(ip):
		return "carrier-grade NAT address", true
	}
	return "", false
}

// matchAny returns the first pattern in pats matching host.
func matchAny(pats []string, host string) (string, bool) {
	for _, pat := range pats {
		if matchHost(pat, host) {
			return pat, true
		}
	}
	return "", false
}

// matchHost implements the pattern rules documented on Policy.
func matchHost(pat, host string) bool {
	pat = strings.TrimSpace(strings.ToLower(pat))
	host = strings.ToLower(host)
	if pat == "" || host == "" {
		return false
	}
	if pat == "*" {
		return true
	}
	if strings.HasPrefix(pat, "*.") {
		return strings.HasSuffix(host, pat[1:])
	}
	return host == pat || strings.HasSuffix(host, "."+pat)
}

func containsFold(list []string, want string) bool {
	for _, s := range list {
		if strings.EqualFold(s, want) {
			return true
		}
	}
	return false
}
