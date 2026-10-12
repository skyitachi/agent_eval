package session

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(path, body string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(body), 0o644)
}

func strptr(s string) *string { return &s }

func TestParseFixtureIsReadable(t *testing.T) {
	if _, err := os.Stat(fixture); err != nil {
		t.Fatalf("fixture missing: %v", err)
	}
}
