package state

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/metalmatze/kelpie/internal/core"
)

// State is everything kelpie remembers between ticks. Persistence comes later
// as a second implementation; callers must not rely on it being in memory.
type State interface {
	// Snapshots are per source: two sources may report the same subject with
	// different fields.
	Snapshot(source string, s core.Subject) (map[string]any, bool)
	SetSnapshot(source string, s core.Subject, fields map[string]any)

	// Bindings are per subject and workspace: several workspaces may watch
	// one subject, and each gets its own delivery.
	Binding(s core.Subject, workspace string) (core.Binding, bool)
	SetBinding(b core.Binding)
	DeleteBinding(s core.Subject, workspace string)
	// Bindings lists the bindings of one subject, by workspace.
	Bindings(s core.Subject) []core.Binding
	// AllBindings lists every binding, by subject and then workspace.
	AllBindings() []core.Binding
}

type snapshotKey struct {
	source  string
	subject core.Subject
}

type bindingKey struct {
	subject   core.Subject
	workspace string
}

func keyOf(b core.Binding) bindingKey { return bindingKey{b.Subject, b.Workspace} }

func compareBindings(a, b core.Binding) int {
	return cmp.Or(cmp.Compare(a.Subject, b.Subject), cmp.Compare(a.Workspace, b.Workspace))
}

type Memory struct {
	mu        sync.Mutex
	snapshots map[snapshotKey]map[string]any
	bindings  map[bindingKey]core.Binding

	// watchesPath, when set, keeps explicit bindings across restarts. Only
	// those: someone asked for them, while the others rebuild themselves.
	watchesPath string
}

// PersistWatches loads explicit bindings from path and saves them there on
// every change from now on.
func (m *Memory) PersistWatches(path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.watchesPath = path
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var list []core.Binding
	if err := json.Unmarshal(data, &list); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	for _, b := range list {
		m.bindings[keyOf(b)] = b
	}
	return nil
}

// saveWatches must be called with m.mu held.
func (m *Memory) saveWatches() {
	if m.watchesPath == "" {
		return
	}
	list := []core.Binding{}
	for _, b := range m.bindings {
		if b.Via == "explicit" {
			list = append(list, b)
		}
	}
	slices.SortFunc(list, compareBindings)
	data, err := json.MarshalIndent(list, "", "  ")
	if err == nil {
		err = os.MkdirAll(filepath.Dir(m.watchesPath), 0o755)
	}
	if err == nil {
		// Write and rename, so a crash mid-write cannot lose every watch.
		tmp := m.watchesPath + ".tmp"
		if err = os.WriteFile(tmp, data, 0o644); err == nil {
			err = os.Rename(tmp, m.watchesPath)
		}
	}
	if err != nil {
		slog.Error("saving watches failed", "path", m.watchesPath, "err", err)
	}
}

func NewMemory() *Memory {
	return &Memory{
		snapshots: map[snapshotKey]map[string]any{},
		bindings:  map[bindingKey]core.Binding{},
	}
}

func (m *Memory) Snapshot(source string, s core.Subject) (map[string]any, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.snapshots[snapshotKey{source, s}]
	return f, ok
}

func (m *Memory) SetSnapshot(source string, s core.Subject, fields map[string]any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snapshots[snapshotKey{source, s}] = fields
}

func (m *Memory) Binding(s core.Subject, workspace string) (core.Binding, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.bindings[bindingKey{s, workspace}]
	return b, ok
}

func (m *Memory) SetBinding(b core.Binding) {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, had := m.bindings[keyOf(b)]
	m.bindings[keyOf(b)] = b
	if b.Via == "explicit" || had && old.Via == "explicit" {
		m.saveWatches()
	}
}

func (m *Memory) DeleteBinding(s core.Subject, workspace string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := bindingKey{s, workspace}
	b, ok := m.bindings[k]
	delete(m.bindings, k)
	if ok && b.Via == "explicit" {
		m.saveWatches()
	}
}

func (m *Memory) Bindings(s core.Subject) []core.Binding {
	m.mu.Lock()
	defer m.mu.Unlock()
	var list []core.Binding
	for k, b := range m.bindings {
		if k.subject == s {
			list = append(list, b)
		}
	}
	slices.SortFunc(list, compareBindings)
	return list
}

func (m *Memory) AllBindings() []core.Binding {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.SortedFunc(maps.Values(m.bindings), compareBindings)
}
