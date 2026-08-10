package ephemeral

import (
	"context"
	cryptorand "crypto/rand"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/go-logr/logr"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// cryptoRand is split out so it can be swapped in tests if needed; the
// signature mirrors crypto/rand.Read.
var cryptoRand = cryptorand.Read

// k8sDB tracks one spawned Pod from creation through teardown.
type k8sDB struct {
	cs        kubernetes.Interface
	namespace string
	name      string
	port      int32
	probe     func(ctx context.Context, endpoint string) error
	readyTO   time.Duration
	endpoint  string
	log       logr.Logger
}

// Wait polls the Pod until kubelet reports Ready, then runs the
// engine-specific Probe() to confirm the DB is actually accepting
// connections (kubelet's "Ready" is only truthful when the container
// has a real readinessProbe — many official DB images don't ship one).
//
// On context cancellation or readyTimeout expiry, the pod is best-effort
// torn down before the error returns; otherwise an aborted Wait() leaves
// orphaned pods until OwnerReference GC catches up.
func (d *k8sDB) Wait(ctx context.Context) error {
	deadline := time.Now().Add(d.readyTO)
	waitCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	d.log.V(1).Info("waiting for ephemeral pod readiness", "name", d.name, "timeout", d.readyTO)

	if err := d.waitForPodIP(waitCtx); err != nil {
		_ = d.Stop(context.Background())
		return fmt.Errorf("wait pod IP: %w", err)
	}

	if d.probe == nil {
		// No engine probe — kubelet Ready is the only signal.
		return nil
	}

	if err := d.waitForProbe(waitCtx); err != nil {
		_ = d.Stop(context.Background())
		return fmt.Errorf("engine probe: %w", err)
	}
	return nil
}

func (d *k8sDB) waitForPodIP(ctx context.Context) error {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		pod, err := d.cs.CoreV1().Pods(d.namespace).Get(ctx, d.name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return fmt.Errorf("pod %s vanished before ready", d.name)
			}
			// An RBAC denial will never clear by retrying — fail immediately
			// with the actual cause instead of spinning until readyTimeout
			// (default 5 min) and then returning an opaque deadline error.
			// The worker SA needs pods/get when restore-verification spawns
			// ephemeral pods (restoreVerification.enableEphemeralPodSpawn).
			if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
				return fmt.Errorf("not permitted to read pod %s (worker ServiceAccount missing pods/get?): %w", d.name, err)
			}
			d.log.V(1).Info("pod get failed; retrying", "err", err.Error())
		} else if pod.Status.Phase == corev1.PodFailed {
			return fmt.Errorf("pod %s entered Failed phase", d.name)
		} else if pod.Status.Phase == corev1.PodSucceeded {
			// The DB container exited 0 immediately (bad image entrypoint, a
			// one-shot command, RestartPolicyNever). It will never serve — fail
			// fast instead of spinning until readyTimeout and returning an
			// opaque deadline error.
			return fmt.Errorf("pod %s exited (Succeeded) before serving; check the verification image entrypoint", d.name)
		} else if pod.Status.PodIP != "" && pod.Status.Phase == corev1.PodRunning {
			// net.JoinHostPort brackets IPv6 PodIPs ("[fd00::1]:5432"); a bare
			// fmt "%s:%d" produced "fd00::1:5432", which every DSN/URI form
			// (postgres/mysql/mongo use the endpoint verbatim) then parsed wrong.
			d.endpoint = net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(int(d.port)))
			d.log.V(1).Info("ephemeral pod has IP", "endpoint", d.endpoint)
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

func (d *k8sDB) waitForProbe(ctx context.Context) error {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	var lastErr error
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := d.probe(probeCtx, d.endpoint)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("probe never succeeded; last error: %w", lastErr)
			}
			return ctx.Err()
		case <-tick.C:
		}
	}
}

func (d *k8sDB) Endpoint() string { return d.endpoint }

// Stop deletes the pod immediately. Best-effort: NotFound is swallowed,
// other errors are logged but never returned as a hard failure since
// OwnerReference GC will catch up regardless.
func (d *k8sDB) Stop(ctx context.Context) error {
	d.log.V(1).Info("stopping ephemeral pod", "name", d.name)
	gracePeriod := int64(0) // skip graceful shutdown — we just want it gone
	err := d.cs.CoreV1().Pods(d.namespace).Delete(ctx, d.name, metav1.DeleteOptions{
		GracePeriodSeconds: &gracePeriod,
	})
	if err != nil && !apierrors.IsNotFound(err) {
		d.log.V(1).Info("ephemeral pod delete failed (will rely on OwnerReference GC)", "err", err.Error())
		return err
	}
	return nil
}
