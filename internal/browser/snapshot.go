package browser

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/chromedp"
)

// The accessibility result types are declared locally rather than taken from
// cdproto/accessibility on purpose: cdproto decodes AXProperty names into a
// closed enum, so a Chrome build that emits a newer value (e.g. the
// "uninteresting" ignored-reason) makes the whole getFullAXTree call fail to
// unmarshal. These lenient structs keep snapshots working across Chrome
// versions; unknown members are ignored by the decoder.
type axValue struct {
	Type  string `json:"type"`
	Value any    `json:"value"`
}

type axProperty struct {
	Name  string   `json:"name"`
	Value *axValue `json:"value"`
}

type axNode struct {
	NodeID           string            `json:"nodeId"`
	Ignored          bool              `json:"ignored"`
	Role             *axValue          `json:"role"`
	Name             *axValue          `json:"name"`
	Value            *axValue          `json:"value"`
	Properties       []*axProperty     `json:"properties"`
	BackendDOMNodeID cdp.BackendNodeID `json:"backendDOMNodeId"`
}

type getFullAXTreeReturns struct {
	Nodes []*axNode `json:"nodes"`
}

// Snapshot sizing targets: the point of a snapshot is a compact, greppable
// listing an agent can act on, not a DOM dump.
const (
	maxSnapshotElements = 120
	maxSnapshotBytes    = 5000
	maxNameLen          = 80
)

// refRe matches a snapshot element reference, e.g. "e12".
var refRe = regexp.MustCompile(`^e[0-9]+$`)

// IsRef reports whether sel is an element reference rather than a CSS selector.
func IsRef(sel string) bool { return refRe.MatchString(strings.TrimSpace(sel)) }

// Element is one interactable node from an accessibility snapshot.
type Element struct {
	Ref      string // stable within a single run, e.g. "e12"
	Role     string // ARIA role, e.g. "link", "button", "textbox"
	Name     string // accessible name
	Value    string // current value for inputs
	Disabled bool
	Backend  cdp.BackendNodeID
}

func (e Element) String() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s %s %q", e.Ref, e.Role, e.Name)
	if e.Value != "" {
		fmt.Fprintf(&sb, " value=%q", e.Value)
	}
	if e.Disabled {
		sb.WriteString(" [disabled]")
	}
	return sb.String()
}

// Snapshot is a compact listing of the interactable elements on a page.
type Snapshot struct {
	URL       string
	Elements  []Element
	byRef     map[string]Element
	Truncated bool
}

// Lookup resolves a ref to its element.
func (s *Snapshot) Lookup(ref string) (Element, bool) {
	if s == nil {
		return Element{}, false
	}
	e, ok := s.byRef[strings.TrimSpace(ref)]
	return e, ok
}

// Format renders the snapshot for the agent.
func (s *Snapshot) Format() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d interactable elements (refs are valid for this run only):\n", len(s.Elements))
	for _, e := range s.Elements {
		sb.WriteString(e.String())
		sb.WriteString("\n")
	}
	if s.Truncated {
		sb.WriteString("... (listing truncated; narrow the page or use a CSS selector)\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// interactableRoles are the roles worth surfacing to a caller that wants to
// click or type. Everything else is page furniture.
var interactableRoles = map[string]bool{
	"button": true, "link": true, "textbox": true, "searchbox": true,
	"checkbox": true, "radio": true, "combobox": true, "listbox": true,
	"menuitem": true, "menuitemcheckbox": true, "menuitemradio": true,
	"option": true, "switch": true, "slider": true, "spinbutton": true,
	"tab": true,
}

// contextRoles are non-interactable but cheap and highly orienting.
var contextRoles = map[string]bool{
	"heading": true,
}

// TakeSnapshot walks the accessibility tree of the current page and returns a
// compact listing of interactable elements.
//
// Uses CDP Accessibility.enable + Accessibility.getFullAXTree (via
// cdproto/accessibility); no page-supplied JavaScript is evaluated.
func TakeSnapshot(ctx context.Context, pageURL string) (*Snapshot, error) {
	var res getFullAXTreeReturns
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		if err := cdp.Execute(ctx, "Accessibility.enable", nil, nil); err != nil {
			return err
		}
		return cdp.Execute(ctx, "Accessibility.getFullAXTree", struct{}{}, &res)
	}))
	if err != nil {
		return nil, fmt.Errorf("accessibility snapshot failed: %w", err)
	}
	return buildSnapshot(pageURL, res.Nodes), nil
}

// buildSnapshot is the pure part of TakeSnapshot (unit-testable).
func buildSnapshot(pageURL string, nodes []*axNode) *Snapshot {
	s := &Snapshot{URL: pageURL, byRef: map[string]Element{}}
	n := 0
	for _, node := range nodes {
		if node == nil || node.Ignored || node.BackendDOMNodeID == 0 {
			continue
		}
		role := axString(node.Role)
		if !interactableRoles[role] && !contextRoles[role] {
			continue
		}
		name := truncate(strings.TrimSpace(axString(node.Name)), maxNameLen)
		value := truncate(strings.TrimSpace(axString(node.Value)), maxNameLen)
		if name == "" && value == "" && role != "textbox" && role != "searchbox" {
			// An unnamed link/button is unactionable noise.
			continue
		}
		if len(s.Elements) >= maxSnapshotElements {
			s.Truncated = true
			break
		}
		n++
		e := Element{
			Ref:      fmt.Sprintf("e%d", n),
			Role:     role,
			Name:     name,
			Value:    value,
			Disabled: axBool(node, "disabled"),
			Backend:  node.BackendDOMNodeID,
		}
		s.Elements = append(s.Elements, e)
		s.byRef[e.Ref] = e
	}

	// Hard byte cap so a pathological page cannot blow up the agent's context.
	for len(s.Format()) > maxSnapshotBytes && len(s.Elements) > 1 {
		last := s.Elements[len(s.Elements)-1]
		delete(s.byRef, last.Ref)
		s.Elements = s.Elements[:len(s.Elements)-1]
		s.Truncated = true
	}
	return s
}

// axString extracts the plain string form of an AXValue.
func axString(v *axValue) string {
	if v == nil || v.Value == nil {
		return ""
	}
	if s, ok := v.Value.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v.Value)
}

func axBool(n *axNode, prop string) bool {
	for _, p := range n.Properties {
		if p != nil && p.Name == prop && p.Value != nil {
			if b, ok := p.Value.Value.(bool); ok {
				return b
			}
		}
	}
	return false
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// resolveRef turns a snapshot ref into frontend node IDs usable with
// chromedp.ByNodeID (Click/SendKeys/Clear all accept []cdp.NodeID).
func resolveRef(ctx context.Context, e Element) ([]cdp.NodeID, error) {
	var ids []cdp.NodeID
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		ids, err = dom.PushNodesByBackendIDsToFrontend([]cdp.BackendNodeID{e.Backend}).Do(ctx)
		return err
	}))
	if err != nil {
		return nil, fmt.Errorf("resolve ref %s: %w", e.Ref, err)
	}
	if len(ids) == 0 || ids[0] == 0 {
		return nil, fmt.Errorf("ref %s no longer maps to a live element (the page probably re-rendered; take a new snapshot)", e.Ref)
	}
	return ids, nil
}

// SuggestCandidates returns the closest elements in the snapshot to the target
// the caller tried to use, so a failed click can be retried intelligently
// instead of guessed at again.
func SuggestCandidates(s *Snapshot, target string, n int) []Element {
	if s == nil || len(s.Elements) == 0 {
		return nil
	}
	want := tokenize(target)
	type scored struct {
		e Element
		v int
	}
	var out []scored
	for _, e := range s.Elements {
		out = append(out, scored{e, scoreElement(e, target, want)})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].v > out[j].v })
	var res []Element
	for _, sc := range out {
		if len(res) >= n {
			break
		}
		res = append(res, sc.e)
	}
	return res
}

func scoreElement(e Element, target string, want []string) int {
	hay := strings.ToLower(e.Role + " " + e.Name + " " + e.Value)
	score := 0
	lt := strings.ToLower(strings.Trim(target, `#.[]"'`))
	if lt != "" && strings.Contains(hay, lt) {
		score += 10
	}
	for _, w := range want {
		if len(w) < 3 {
			continue
		}
		if strings.Contains(hay, w) {
			score += 3
		}
	}
	return score
}

var tokenSplit = regexp.MustCompile(`[^a-z0-9]+`)

func tokenize(s string) []string {
	var out []string
	for _, t := range tokenSplit.Split(strings.ToLower(s), -1) {
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

// formatCandidates renders suggestions for an error message.
func formatCandidates(s *Snapshot, target string) string {
	cands := SuggestCandidates(s, target, 5)
	if len(cands) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(" Closest elements in the current snapshot: ")
	for i, c := range cands {
		if i > 0 {
			sb.WriteString("; ")
		}
		fmt.Fprintf(&sb, "%s %s %q", c.Ref, c.Role, c.Name)
	}
	sb.WriteString(". Retry with one of those refs.")
	return sb.String()
}
