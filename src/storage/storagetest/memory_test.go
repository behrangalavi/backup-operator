package storagetest

import (
	"context"
	"strings"
	"testing"

	"backup-operator/storage"
)

// The fake is the reference implementation: it must pass its own suite,
// with and without a path prefix.
func TestMemory_Conformance_NoPrefix(t *testing.T) {
	Run(t, func(t *testing.T) storage.Storage { return NewMemory("mem") })
}

func TestMemory_Conformance_WithPrefix(t *testing.T) {
	Run(t, func(t *testing.T) storage.Storage { return NewMemoryWithPrefix("mem", "/cluster-prod/") })
}

func TestMemory_KeysCarryPrefix(t *testing.T) {
	m := NewMemoryWithPrefix("mem", "cluster-prod")
	if err := m.Upload(context.Background(), "db/x.age", strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	if keys := m.Keys(); len(keys) != 1 || keys[0] != "cluster-prod/db/x.age" {
		t.Fatalf("raw keys = %v, want the prefixed key", keys)
	}
	if !m.Closed() {
		_ = m.Close()
	}
	if !m.Closed() {
		t.Fatal("Close must be recorded")
	}
}

func TestMemory_RemoveDirectory(t *testing.T) {
	m := NewMemory("mem").Seed("db/2026/01/01/dump.age", []byte("x"))
	if err := m.RemoveDirectory(context.Background(), "db/2026/01/01"); err == nil {
		t.Fatal("removing a non-empty directory must fail")
	}
	if err := m.RemoveDirectory(context.Background(), "db/2025"); err != nil {
		t.Fatalf("removing an empty directory must succeed, got %v", err)
	}
}
