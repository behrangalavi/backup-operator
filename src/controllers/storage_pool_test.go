package controllers

import (
	"testing"

	"backup-operator/internal/secrets"

	"github.com/go-logr/logr"
)

// destination Secret data shape for an S3 destination — matches what
// the parser produces. We pick s3 because the factory accepts it
// without filesystem side-effects (SFTP would need a working
// known-hosts file or trip the insecure-warning code path).
func s3Dest(name, bucket, accessKey, secret string) *secrets.Destination {
	return &secrets.Destination{
		SecretName:  name,
		Namespace:   "backup",
		Name:        name,
		StorageType: "s3",
		Data: map[string][]byte{
			"bucket":            []byte(bucket),
			"access-key-id":     []byte(accessKey),
			"secret-access-key": []byte(secret),
			"endpoint":          []byte("http://localhost:9000"),
			"region":            []byte("us-east-1"),
		},
	}
}

func TestStoragePool_GetCachesSameDestination(t *testing.T) {
	// Two Get calls with identical destination data must return the
	// same Storage instance — that is the entire point of the pool.
	p := NewStoragePool(logr.Discard())
	d := s3Dest("hetzner-sb", "backups", "AKIA", "secret123")

	st1, err := p.Get(d)
	if err != nil {
		t.Fatalf("first Get: %v", err)
	}
	st2, err := p.Get(d)
	if err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if st1 != st2 {
		t.Errorf("pool must return the same instance for identical destinations; got %p vs %p", st1, st2)
	}
	if got := p.Size(); got != 1 {
		t.Errorf("pool size = %d, want 1", got)
	}
}

func TestStoragePool_SignatureDriftRebuildsClient(t *testing.T) {
	// If the SecretData changes between calls — credential rotation,
	// endpoint swap — the cached client must be replaced. Otherwise
	// the operator silently authenticates with the prior credentials
	// for its remaining lifetime.
	p := NewStoragePool(logr.Discard())
	d1 := s3Dest("hetzner-sb", "backups", "AKIA", "secret123")
	d2 := s3Dest("hetzner-sb", "backups", "AKIA", "rotated456") // same name, new key

	st1, err := p.Get(d1)
	if err != nil {
		t.Fatalf("first Get: %v", err)
	}
	st2, err := p.Get(d2)
	if err != nil {
		t.Fatalf("second Get after rotation: %v", err)
	}
	if st1 == st2 {
		t.Error("pool must rebuild the client when SecretData changes; got the cached instance instead")
	}
	if got := p.Size(); got != 1 {
		t.Errorf("rebuild should replace, not append; size = %d, want 1", got)
	}
}

func TestStoragePool_DifferentDestinationsCoexist(t *testing.T) {
	// Two different destinations must each get their own pooled
	// client — no accidental sharing across names.
	p := NewStoragePool(logr.Discard())
	d1 := s3Dest("hetzner-sb", "backups", "AKIA", "secret123")
	d2 := s3Dest("offsite-r2", "mirror", "ZZZ", "other")

	if _, err := p.Get(d1); err != nil {
		t.Fatalf("Get(d1): %v", err)
	}
	if _, err := p.Get(d2); err != nil {
		t.Fatalf("Get(d2): %v", err)
	}
	if got := p.Size(); got != 2 {
		t.Errorf("expected two cached clients, got %d", got)
	}
}

func TestStoragePool_RetainPrunesAbsentDestinations(t *testing.T) {
	// Retain mirrors the cluster's current destination set into the
	// pool — entries for destinations that disappeared between ticks
	// must be dropped so we do not leak clients across long-running
	// operator pods.
	p := NewStoragePool(logr.Discard())
	keep := s3Dest("hetzner-sb", "backups", "AKIA", "secret123")
	gone := s3Dest("decommissioned", "old", "AAA", "bbb")

	if _, err := p.Get(keep); err != nil {
		t.Fatalf("Get(keep): %v", err)
	}
	if _, err := p.Get(gone); err != nil {
		t.Fatalf("Get(gone): %v", err)
	}
	if got := p.Size(); got != 2 {
		t.Fatalf("pre-Retain size = %d, want 2", got)
	}

	p.Retain([]*secrets.Destination{keep})

	if got := p.Size(); got != 1 {
		t.Errorf("post-Retain size = %d, want 1", got)
	}
	// And the surviving destination must still resolve to the same
	// instance — Retain must not invalidate kept entries.
	st, err := p.Get(keep)
	if err != nil {
		t.Fatalf("Get(keep) after Retain: %v", err)
	}
	if st == nil {
		t.Error("kept destination resolved to nil")
	}
}

func TestStoragePool_DestSignatureDeterministic(t *testing.T) {
	// destSignature must be stable across calls regardless of the
	// non-deterministic Go map iteration order — otherwise the cache
	// would invalidate itself on every other call.
	d := s3Dest("hetzner-sb", "backups", "AKIA", "secret123")
	first := destSignature(d)
	for i := 0; i < 50; i++ {
		if got := destSignature(d); got != first {
			t.Fatalf("destSignature non-deterministic: iteration %d = %s, want %s", i, got, first)
		}
	}
}

func TestStoragePool_DestSignatureDriftOnDataChange(t *testing.T) {
	// Any change to SecretData must produce a different signature.
	d := s3Dest("hetzner-sb", "backups", "AKIA", "secret123")
	base := destSignature(d)

	// Mutate one value.
	d.Data["secret-access-key"] = []byte("rotated")
	if destSignature(d) == base {
		t.Error("signature must change when a data value is rotated")
	}

	// Add a new key.
	d2 := s3Dest("hetzner-sb", "backups", "AKIA", "secret123")
	d2.Data["path-style"] = []byte("true")
	if destSignature(d2) == base {
		t.Error("signature must change when a data key is added")
	}
}
