package replay

import "testing"

// TestExamplePresetsLoad keeps the shipped example loadable: a presets file
// that does not load is one nobody can start a server with.
func TestExamplePresetsLoad(t *testing.T) {
	p, err := LoadPresets("../../examples/replay-presets.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.Resolve(Choice{}); err != nil {
		t.Fatal(err)
	}
}
