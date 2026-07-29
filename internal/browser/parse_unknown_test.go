package browser

import "testing"

// A mistyped action must be reported, not silently dropped: before this, an
// unrecognized word fell through to the default text read and the caller got
// page content back that looked like a successful action.
func TestParseDirective_UnknownActionsReported(t *testing.T) {
	d := ParseDirective("https://example.com", "frobnicate foo\ntext")
	if len(d.Unknown) != 1 || d.Unknown[0] != "frobnicate foo" {
		t.Fatalf("want the bad line captured, got %q", d.Unknown)
	}
	if len(d.Actions) != 1 || d.Actions[0].Type != ActionText {
		t.Fatalf("valid actions must still parse, got %+v", d.Actions)
	}
	if clean := ParseDirective("https://example.com", "snapshot\ntext"); len(clean.Unknown) != 0 {
		t.Fatalf("valid directive must report no unknowns, got %q", clean.Unknown)
	}
}
