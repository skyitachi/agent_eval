// Package machine names the computer a recording was made on, so a replay can
// tell whether it is running on the same one.
//
// A collected task is measured against what the recorded session did, and what
// it did depended on the machine around it: the directories it read, the tools
// on its PATH, the caches it hit. A replay on a different machine is a
// different measurement, and nothing in the numbers would say so. The identity
// here is what lets the harness refuse that up front instead.
package machine

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
)

// Identity is what a baseline records about the machine it was collected on.
//
// ID is the only field compared. The others are there for the person reading
// a refusal: an opaque hash says "different", a host name says which.
type Identity struct {
	// ID is derived from the platform's own machine identifier. It is hashed
	// rather than stored as read, because the raw identifier is meant to stay
	// on the machine (machine-id(5) says as much) and a dataset is a file
	// people pass around.
	ID       string `json:"id"`
	Hostname string `json:"hostname,omitempty"`
	OS       string `json:"os,omitempty"`
	Arch     string `json:"arch,omitempty"`
}

// Same reports whether two identities name one machine. An identity with no
// ID names nothing, so it is never the same as anything.
func (i Identity) Same(o Identity) bool {
	return i.ID != "" && i.ID == o.ID
}

// String renders the identity for a sentence a person reads.
func (i Identity) String() string {
	host := i.Hostname
	if host == "" {
		host = "unnamed host"
	}
	id := i.ID
	if len(id) > 12 {
		id = id[:12]
	}
	if i.OS == "" && i.Arch == "" {
		return fmt.Sprintf("%s (id %s)", host, id)
	}
	return fmt.Sprintf("%s (%s/%s, id %s)", host, i.OS, i.Arch, id)
}

// probe reads the platform's machine identifier. It is a variable so tests can
// stand in for a machine they are not running on.
var probe = platformID

var (
	once    sync.Once
	current Identity
	errCur  error
)

// Current is the identity of the machine this process runs on.
//
// An error is returned rather than a weaker identity: the host name is the
// obvious fallback and it is also the one thing two machines on a network are
// likely to share by default, so a check built on it would pass exactly when it
// should not.
func Current() (Identity, error) {
	once.Do(func() {
		raw, err := probe()
		if err != nil {
			errCur = fmt.Errorf("cannot read this machine's identity: %w", err)
			return
		}
		raw = strings.TrimSpace(raw)
		if raw == "" {
			errCur = errors.New("cannot read this machine's identity: the platform reported an empty one")
			return
		}
		host, _ := os.Hostname()
		current = Identity{ID: hash(raw), Hostname: host, OS: runtime.GOOS, Arch: runtime.GOARCH}
	})
	return current, errCur
}

// hash turns a raw platform identifier into the one a dataset carries. The
// prefix keeps it from matching what any other program derives from the same
// identifier.
func hash(raw string) string {
	sum := sha256.Sum256([]byte("agenteval machine\x00" + raw))
	return hex.EncodeToString(sum[:16])
}

// platformID reads the identifier the operating system keeps for this machine.
// Each of these survives a reboot and a change of host name, which is the
// point: they name the installation rather than how it is currently addressed.
func platformID() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("ioreg", "-rd1", "-c", "IOPlatformExpertDevice").Output()
		if err != nil {
			return "", fmt.Errorf("ioreg: %w", err)
		}
		return field(string(out), "IOPlatformUUID")
	case "linux":
		return firstFile("/etc/machine-id", "/var/lib/dbus/machine-id")
	case "freebsd":
		return firstFile("/etc/hostid")
	case "windows":
		out, err := exec.Command("reg", "query",
			`HKLM\SOFTWARE\Microsoft\Cryptography`, "/v", "MachineGuid").Output()
		if err != nil {
			return "", fmt.Errorf("reg query MachineGuid: %w", err)
		}
		for _, line := range strings.Split(string(out), "\n") {
			if f := strings.Fields(line); len(f) == 3 && f[0] == "MachineGuid" {
				return f[2], nil
			}
		}
		return "", errors.New("reg query printed no MachineGuid")
	default:
		return "", fmt.Errorf("no machine identifier is known for %s", runtime.GOOS)
	}
}

// field pulls `"key" = "value"` out of ioreg's output.
func field(out, key string) (string, error) {
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, `"`+key+`"`) {
			continue
		}
		if _, v, ok := strings.Cut(line, "="); ok {
			return strings.Trim(strings.TrimSpace(v), `"`), nil
		}
	}
	return "", fmt.Errorf("ioreg printed no %s", key)
}

func firstFile(paths ...string) (string, error) {
	var errs []error
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err == nil && strings.TrimSpace(string(b)) != "" {
			return string(b), nil
		}
		if err == nil {
			err = fmt.Errorf("%s is empty", p)
		}
		errs = append(errs, err)
	}
	return "", errors.Join(errs...)
}
