package controllers

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"backup-operator/api/v1alpha1"
	"backup-operator/internal/secrets"
	"backup-operator/metrics"
	"backup-operator/storage"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// memStorage is an in-memory storage.Storage whose List/Get can be made to
// fail, so the refresher's per-destination decisions can be driven without
// a real backend.
type memStorage struct {
	name    string
	objects map[string][]byte
	listErr error
	getErr  error
}

func (m *memStorage) Name() string                                    { return m.name }
func (m *memStorage) Close() error                                    { return nil }
func (m *memStorage) Upload(context.Context, string, io.Reader) error { return errors.New("unused") }
func (m *memStorage) Delete(context.Context, string) error            { return errors.New("unused") }
func (m *memStorage) List(_ context.Context, prefix string) ([]storage.Object, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	var out []storage.Object
	for p, b := range m.objects {
		if len(prefix) == 0 || len(p) >= len(prefix) && p[:len(prefix)] == prefix {
			out = append(out, storage.Object{Path: p, Size: int64(len(b))})
		}
	}
	return out, nil
}
func (m *memStorage) Get(_ context.Context, p string) (io.ReadCloser, error) {
	if m.getErr != nil {
		return nil, m.getErr
	}
	b, ok := m.objects[p]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

var registerOnce sync.Once

// testRegistry registers the metrics package into a dedicated registry once
// per test binary (collectors can only be registered once per registry) and
// returns it for Gather-based assertions.
func testRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	registerOnce.Do(func() { metrics.Register(reg) })
	// Later calls still need the registry the collectors live in; Register
	// stored it as the package gatherer.
	if g, ok := metrics.Gatherer().(*prometheus.Registry); ok {
		return g
	}
	return reg
}

func gaugeValue(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) (float64, bool) {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
	next:
		for _, m := range mf.Metric {
			for k, v := range labels {
				found := false
				for _, lp := range m.Label {
					if lp.GetName() == k && lp.GetValue() == v {
						found = true
						break
					}
				}
				if !found {
					continue next
				}
			}
			return m.GetGauge().GetValue(), true
		}
	}
	return 0, false
}

func s3DestObj(name, secret string) *v1alpha1.BackupDestination {
	return &v1alpha1.BackupDestination{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "backup"},
		Spec: v1alpha1.BackupDestinationSpec{
			Type:                 v1alpha1.StorageS3,
			CredentialsSecretRef: v1alpha1.LocalSecretRef{Name: secret},
			S3:                   &v1alpha1.S3Spec{Bucket: "b"},
		},
	}
}

// TestRefresh_DestinationFailedDecision pins the one rule BackupDestinationFailing
// depends on: a destination whose listing errors is failed (1); one that is
// reachable but has nothing uploaded yet is healthy (0); one that serves a
// readable meta is healthy (0) and feeds last_success. It also confirms the
// tick needs exactly one root listing per destination, not one per source.
func TestRefresh_DestinationFailedDecision(t *testing.T) {
	reg := testRegistry()
	creds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "backup"},
		Data:       map[string][]byte{"access-key-id": []byte("k"), "secret-access-key": []byte("s")},
	}
	src1, src2 := testSource("t1", "backup"), testSource("t2", "backup")
	c := newFakeClient(t, src1, src2, creds,
		s3DestObj("broken", "creds"), s3DestObj("empty", "creds"), s3DestObj("ok", "creds"))

	meta := []byte(`{"target":"t1","timestamp":"20260101T020000Z","status":"success","encryptedSizeBytes":10}`)
	listCalls := map[string]int{}
	var mu sync.Mutex
	stores := map[string]*memStorage{
		"broken": {name: "broken", listErr: errors.New("ssh: handshake failed")},
		"empty":  {name: "empty", objects: map[string][]byte{}},
		"ok":     {name: "ok", objects: map[string][]byte{"t1/2026/01/01/dump-20260101T020000Z.meta.json": meta}},
	}
	r := &MetricsRefresher{
		Client: c, Namespace: "backup", Logger: logr.Discard(), DefaultSchedule: "0 2 * * *",
		StorageFor: func(d *secrets.Destination) (storage.Storage, error) {
			mu.Lock()
			listCalls[d.Name]++
			mu.Unlock()
			return stores[d.Name], nil
		},
	}
	r.refresh(context.Background())

	for dest, want := range map[string]float64{"broken": 1, "empty": 0, "ok": 0} {
		got, ok := gaugeValue(t, reg, metrics.NameDestinationFailed, map[string]string{"target": "t1", "destination": dest})
		if !ok {
			t.Errorf("destination_failed{t1,%s} absent", dest)
			continue
		}
		if got != want {
			t.Errorf("destination_failed{t1,%s} = %v, want %v", dest, got, want)
		}
	}
	if _, ok := gaugeValue(t, reg, metrics.NameLastSuccessTimestampSeconds, map[string]string{"target": "t1", "destination": "ok"}); !ok {
		t.Error("last_success must be set from the readable meta on the ok destination")
	}
	// t2 has no run on "ok" → empty, not failed.
	if got, _ := gaugeValue(t, reg, metrics.NameDestinationFailed, map[string]string{"target": "t2", "destination": "ok"}); got != 0 {
		t.Errorf("destination_failed{t2,ok} = %v, want 0 (reachable, nothing uploaded)", got)
	}
	for dest, n := range listCalls {
		if n != 1 {
			t.Errorf("destination %s resolved %d times in one tick; one root listing per destination is the contract", dest, n)
		}
	}
	got := getSource(t, c, "t1", "backup")
	if got.Status.LastRunStatus != v1alpha1.RunStatusSuccess {
		t.Errorf("status.lastRunStatus = %q, want success", got.Status.LastRunStatus)
	}
}
