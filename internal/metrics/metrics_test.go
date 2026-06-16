package metrics

import (
	"bytes"
	"strings"
	"testing"
)

// TestRender_EscapesSourceLabel verifies that a binding-source string
// containing Prometheus-special characters is escaped in the label value.
// An unescaped quote or backslash would make node-exporter's textfile
// collector reject the entire file, silently dropping all metrics.
func TestRender_EscapesSourceLabel(t *testing.T) {
	out := Render(Snapshot{Source: `weird"src\back` + "\n" + "end"})
	var line string
	for _, l := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(l, "fan_controller_binding_source_info{") {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatal("binding_source_info line not found")
	}
	want := `fan_controller_binding_source_info{source="weird\"src\\back\nend"} 1`
	if line != want {
		t.Errorf("escaping wrong:\n got: %s\nwant: %s", line, want)
	}
}

func TestEscapeLabelValue(t *testing.T) {
	tests := []struct{ in, want string }{
		{"cpu", "cpu"},            // safe — passthrough
		{"pg_pf", "pg_pf"},        // safe
		{`a"b`, `a\"b`},           // quote
		{`a\b`, `a\\b`},           // backslash
		{"a\nb", `a\nb`},          // newline
		{`x"\` + "\n", `x\"\\\n`}, // all three
	}
	for _, tt := range tests {
		if got := escapeLabelValue(tt.in); got != tt.want {
			t.Errorf("escapeLabelValue(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestRender_SafeSourceUnchanged guards that the common-case source
// values (which are all safe literals) render with no escaping overhead
// or alteration — protecting the golden-file contract.
func TestRender_SafeSourceUnchanged(t *testing.T) {
	out := Render(Snapshot{Source: "cpu"})
	if !bytes.Contains(out, []byte(`fan_controller_binding_source_info{source="cpu"} 1`)) {
		t.Errorf("safe source 'cpu' should render verbatim:\n%s", out)
	}
}
