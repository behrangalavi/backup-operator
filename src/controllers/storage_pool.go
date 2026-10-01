package controllers

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"sync"
	"time"

	"github.com/go-logr/logr"

	"backup-operator/internal/secrets"
	"backup-operator/storage"
	storageFactory "backup-operator/storage/factory"
)

// StoragePool keeps one storage.Storage instance per destination alive
// across refresh / scrub ticks. The previous behaviour rebuilt a fresh
// client on every call (storageFactory.NewStorage inside each goroutine),
// which for SFTP meant an SSH handshake — ~100–300 ms — per call. At
// 100 sources × 5 destinations × 30s ticks that is the dominant cost
// of the whole refresher.
//
// Cache key: destination Name plus a SHA-256 over the stable identity
// fields and the entire SecretData map. The data hash matters: without
// it, a Secret edit (rotated SSH key, swapped S3 endpoint) would
// silently keep authenticating with the previous credentials for the
// lifetime of the operator pod. With it, the next tick's Get() detects
// signature drift, rebuilds, and the old client is dropped to GC.
//
// The pool is safe for concurrent use by both the MetricsRefresher and
// the StorageScrubber on the leader pod — sharing one instance avoids
// two parallel client lifecycles for the same backend. See the §18 ADR.
type StoragePool struct {
	mu      sync.Mutex
	clients map[string]pooledClient
	log     logr.Logger
	// closeGrace is how long an evicted or superseded client stays open
	// before Close is called on it. Get hands out the Storage value without
	// reference counting, so a refresher goroutine that fetched the client
	// just before a credential rotation may still be mid-List when the pool
	// replaces it. One grace window comfortably outlives any single
	// List+Get (bounded by the callers' per-op timeouts); after it, the
	// client — and for GCS its gRPC connections — is released instead of
	// leaking until the GC happens to collect it.
	closeGrace time.Duration
}

// defaultCloseGrace is longer than the longest bounded storage call the
// refresher/scrubber/UI issue (the scrubber's full-dump re-stream is the
// outlier and runs under its own context deadline).
const defaultCloseGrace = 2 * time.Minute

type pooledClient struct {
	sig     string
	storage storage.Storage
}

// NewStoragePool returns an empty pool. log is used only for diagnostic
// messages; per-destination lookups never log on the hot path.
func NewStoragePool(log logr.Logger) *StoragePool {
	return &StoragePool{
		clients:    make(map[string]pooledClient),
		log:        log.WithName("storage-pool"),
		closeGrace: defaultCloseGrace,
	}
}

// retire schedules Close on a client that is no longer handed out. Called
// with p.mu held; the close itself runs later, off the lock.
func (p *StoragePool) retire(c pooledClient) {
	st := c.storage
	if st == nil {
		return
	}
	closeIt := func() {
		if err := st.Close(); err != nil {
			p.log.V(1).Info("closing retired storage client failed", "destination", st.Name(), "err", err.Error())
		}
	}
	if p.closeGrace <= 0 {
		closeIt()
		return
	}
	time.AfterFunc(p.closeGrace, closeIt)
}

// Close retires every cached client immediately. Intended for operator
// shutdown and tests; concurrent Get callers get a fresh client afterwards.
func (p *StoragePool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	grace := p.closeGrace
	p.closeGrace = 0
	for name, c := range p.clients {
		p.retire(c)
		delete(p.clients, name)
	}
	p.closeGrace = grace
}

// Get returns a pooled Storage for the destination, building one on
// demand. Signature drift transparently rebuilds the client; in-flight
// callers holding the previous reference are unaffected — they finish
// against the old client (closed after closeGrace), then the next tick
// gets the new one.
func (p *StoragePool) Get(d *secrets.Destination) (storage.Storage, error) {
	sig := destSignature(d)
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.clients[d.Name]; ok && c.sig == sig {
		return c.storage, nil
	}
	st, err := storageFactory.NewStorage(d.StorageType, d.Name, d.Data, p.log)
	if err != nil {
		return nil, err
	}
	if old, ok := p.clients[d.Name]; ok {
		p.retire(old)
	}
	p.clients[d.Name] = pooledClient{sig: sig, storage: st}
	return st, nil
}

// Retain drops cached entries whose destination is no longer present.
// Called once per tick from the controller that owns the freshest
// destination list; safe to call concurrently with Get().
func (p *StoragePool) Retain(current []*secrets.Destination) {
	keep := make(map[string]struct{}, len(current))
	for _, d := range current {
		keep[d.Name] = struct{}{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for name, c := range p.clients {
		if _, ok := keep[name]; !ok {
			p.retire(c)
			delete(p.clients, name)
		}
	}
}

// Size returns the number of cached clients. Used in tests; production
// code should not depend on the exact count.
func (p *StoragePool) Size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.clients)
}

// destSignature is a stable hash over the destination's identity AND
// the full SecretData map. SecretData is included so a credential or
// endpoint change invalidates the cached client at the next tick.
// Map iteration order is non-deterministic in Go, so the keys are
// sorted before hashing.
func destSignature(d *secrets.Destination) string {
	h := sha256.New()
	h.Write([]byte(d.ObjectName))
	h.Write([]byte{0})
	h.Write([]byte(d.Namespace))
	h.Write([]byte{0})
	h.Write([]byte(d.StorageType))
	h.Write([]byte{0})
	keys := make([]string, 0, len(d.Data))
	for k := range d.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write(d.Data[k])
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
