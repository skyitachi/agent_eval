package machine

import (
	"errors"
	"strings"
	"sync"
	"testing"
)

// withProbe stands in for the platform and resets the cached identity.
func withProbe(t *testing.T, fn func() (string, error)) {
	t.Helper()
	old := probe
	probe = fn
	once, current, errCur = sync.Once{}, Identity{}, nil
	t.Cleanup(func() {
		probe = old
		once, current, errCur = sync.Once{}, Identity{}, nil
	})
}

func TestCurrentReadsThisMachine(t *testing.T) {
	id, err := Current()
	if err != nil {
		t.Skipf("this platform cannot be identified: %v", err)
	}
	if len(id.ID) != 32 {
		t.Errorf("id = %q, want 32 hex characters", id.ID)
	}
	again, _ := Current()
	if !id.Same(again) {
		t.Errorf("two reads of one machine disagree: %v vs %v", id, again)
	}
}

func TestTheRawIdentifierIsNotWhatIsStored(t *testing.T) {
	// The platform identifier is meant to stay on the machine, and a dataset is
	// a file people share.
	withProbe(t, func() (string, error) { return "4C4C4544-0042-5A10-8048-B7C04F4E3332\n", nil })
	id, err := Current()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(id.ID), "4c4c4544") {
		t.Errorf("the stored id %q carries the raw identifier", id.ID)
	}
	if id.ID != hash("4C4C4544-0042-5A10-8048-B7C04F4E3332") {
		t.Errorf("surrounding whitespace changed the id: %q", id.ID)
	}
}

func TestAnUnidentifiableMachineIsAnErrorNotAGuess(t *testing.T) {
	// Falling back to the host name would make two machines that share a
	// default name look like one.
	for name, fn := range map[string]func() (string, error){
		"probe fails": func() (string, error) { return "", errors.New("no ioreg") },
		"empty":       func() (string, error) { return "  \n", nil },
	} {
		t.Run(name, func(t *testing.T) {
			withProbe(t, fn)
			if id, err := Current(); err == nil {
				t.Fatalf("got %v, want an error", id)
			}
		})
	}
}

func TestSameComparesTheIDOnly(t *testing.T) {
	a := Identity{ID: "abc", Hostname: "box", OS: "linux"}
	if !a.Same(Identity{ID: "abc", Hostname: "renamed"}) {
		t.Error("a renamed host is still the same machine")
	}
	if a.Same(Identity{ID: "def", Hostname: "box", OS: "linux"}) {
		t.Error("two machines with one host name are not the same machine")
	}
	if (Identity{}).Same(Identity{}) {
		t.Error("two identities that name nothing must not match")
	}
}

func TestIoregField(t *testing.T) {
	out := `+-o J314sAP  <class IOPlatformExpertDevice>
    {
      "IOPlatformSerialNumber" = "XYZ"
      "IOPlatformUUID" = "11111111-2222-3333-4444-555555555555"
    }`
	got, err := field(out, "IOPlatformUUID")
	if err != nil || got != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("field = %q, %v", got, err)
	}
	if _, err := field(out, "Missing"); err == nil {
		t.Error("a missing key must be an error")
	}
}
