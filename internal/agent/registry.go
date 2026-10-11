package agent

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Registry of agent implementations.
//
// The engine resolves Spec.Kind through New, so adding an agent does not require
// touching the engine, graders or storage. Registration is process-global and
// safe for concurrent use.
var (
	regMu    sync.RWMutex
	registry = map[string]func() Runner{}
)

// Register binds a kind name to a Runner factory. It panics on a duplicate or
// empty name, since both are programming errors that would silently change which
// agent a config runs.
func Register(kind string, factory func() Runner) {
	key := normalizeKind(kind)
	if key == "" {
		panic("agent: Register called with an empty kind")
	}
	if factory == nil {
		panic("agent: Register called with a nil factory for " + kind)
	}
	regMu.Lock()
	defer regMu.Unlock()
	if _, dup := registry[key]; dup {
		panic("agent: duplicate registration for kind " + kind)
	}
	registry[key] = factory
}

// RegisterAlias points an additional name at an already-registered runner.
func RegisterAlias(alias, target string) {
	regMu.Lock()
	defer regMu.Unlock()
	f, ok := registry[normalizeKind(target)]
	if !ok {
		panic("agent: RegisterAlias target not registered: " + target)
	}
	registry[normalizeKind(alias)] = f
}

// New returns a Runner for the requested kind.
func New(kind string) (Runner, error) {
	key := normalizeKind(kind)
	if key == "" {
		key = KindClaudeCode
	}
	regMu.RLock()
	f, ok := registry[key]
	regMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown agent kind %q (available: %s)", kind, strings.Join(Registered(), ", "))
	}
	return f(), nil
}

// Registered lists the known kinds in sorted order.
func Registered() []string {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func normalizeKind(k string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(k)), "-", "_")
}

func init() {
	Register(KindClaudeCode, func() Runner { return NewClaudeCode() })
	RegisterAlias("claude", KindClaudeCode)
	RegisterAlias("claudecode", KindClaudeCode)
	Register(KindMock, func() Runner { return &Mock{} })
	RegisterAlias("fake", KindMock)
}
