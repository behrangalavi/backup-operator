package ephemeral

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestSpawn_RejectsMissingImage(t *testing.T) {
	cs := fake.NewSimpleClientset()
	s := NewK8sSpawner(cs, "backup")
	_, err := s.Spawn(context.Background(), Spec{Port: 5432}, testr.New(t))
	if err == nil {
		t.Error("expected error for missing image")
	}
}

func TestSpawn_RejectsMissingPort(t *testing.T) {
	cs := fake.NewSimpleClientset()
	s := NewK8sSpawner(cs, "backup")
	_, err := s.Spawn(context.Background(), Spec{Image: "postgres:16"}, testr.New(t))
	if err == nil {
		t.Error("expected error for missing port")
	}
}

// Spawn creates a Pod with the expected security context, owner ref,
// and emptyDir size. We assert on the resulting object via the fake
// client's tracker rather than mocking Create.
func TestSpawn_CreatesHardenedPod(t *testing.T) {
	cs := fake.NewSimpleClientset()
	s := NewK8sSpawner(cs, "backup")

	owner := metav1.OwnerReference{
		APIVersion: "batch/v1",
		Kind:       "Job",
		Name:       "worker-job",
		UID:        "uid-123",
	}

	_, err := s.Spawn(context.Background(), Spec{
		NamePrefix:      "verify-prod-users",
		Image:           "postgres:16-alpine",
		Port:            5432,
		VolumeSizeBytes: 5 * 1024 * 1024 * 1024,
		VolumeMountPath: "/var/lib/postgresql/data",
		OwnerRef:        &owner,
		EnvVars: []corev1.EnvVar{
			{Name: "POSTGRES_PASSWORD", Value: "x"},
		},
	}, testr.New(t))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	pods, err := cs.CoreV1().Pods("backup").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) != 1 {
		t.Fatalf("want 1 pod, got %d", len(pods.Items))
	}
	pod := pods.Items[0]

	if !strings.HasPrefix(pod.Name, "verify-prod-users-") {
		t.Errorf("name = %q, want verify-prod-users-* prefix", pod.Name)
	}

	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("want 1 container, got %d", len(pod.Spec.Containers))
	}
	c := pod.Spec.Containers[0]
	if c.Image != "postgres:16-alpine" {
		t.Errorf("image = %q", c.Image)
	}
	if len(c.Ports) != 1 || c.Ports[0].ContainerPort != 5432 {
		t.Errorf("port = %v", c.Ports)
	}
	if c.SecurityContext == nil || c.SecurityContext.AllowPrivilegeEscalation == nil ||
		*c.SecurityContext.AllowPrivilegeEscalation {
		t.Error("AllowPrivilegeEscalation must be false")
	}
	if c.SecurityContext.ReadOnlyRootFilesystem == nil || !*c.SecurityContext.ReadOnlyRootFilesystem {
		t.Error("ReadOnlyRootFilesystem must be true")
	}
	if c.SecurityContext.Capabilities == nil ||
		len(c.SecurityContext.Capabilities.Drop) != 1 ||
		c.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Error("must drop ALL capabilities")
	}

	if pod.Spec.SecurityContext == nil || pod.Spec.SecurityContext.RunAsNonRoot == nil ||
		!*pod.Spec.SecurityContext.RunAsNonRoot {
		t.Error("RunAsNonRoot must be true")
	}
	if pod.Spec.SecurityContext.SeccompProfile == nil ||
		pod.Spec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Error("SeccompProfile must be RuntimeDefault")
	}

	if len(pod.Spec.Volumes) != 1 || pod.Spec.Volumes[0].EmptyDir == nil {
		t.Fatalf("volumes = %v", pod.Spec.Volumes)
	}
	gotSize := pod.Spec.Volumes[0].EmptyDir.SizeLimit
	if gotSize == nil || gotSize.Value() != 5*1024*1024*1024 {
		t.Errorf("emptyDir sizeLimit = %v", gotSize)
	}

	if len(pod.OwnerReferences) != 1 || pod.OwnerReferences[0].UID != owner.UID {
		t.Errorf("OwnerReferences = %v", pod.OwnerReferences)
	}

	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("RestartPolicy = %v, want Never", pod.Spec.RestartPolicy)
	}
}

// Wait succeeds when the fake client reports the pod with a PodIP and Running phase.
func TestWait_PodGetsIP(t *testing.T) {
	cs := fake.NewSimpleClientset()
	s := NewK8sSpawner(cs, "backup")
	db, err := s.Spawn(context.Background(), Spec{
		NamePrefix:   "x",
		Image:        "redis:7-alpine",
		Port:         6379,
		ReadyTimeout: 5 * time.Second,
	}, testr.New(t))
	if err != nil {
		t.Fatal(err)
	}

	// Patch the pod to be Running with an IP — simulate kubelet scheduling.
	pods, _ := cs.CoreV1().Pods("backup").List(context.Background(), metav1.ListOptions{})
	pod := pods.Items[0].DeepCopy()
	pod.Status.PodIP = "10.0.0.42"
	pod.Status.Phase = corev1.PodRunning
	if _, err := cs.CoreV1().Pods("backup").UpdateStatus(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	if err := db.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got := db.Endpoint(); got != "10.0.0.42:6379" {
		t.Errorf("Endpoint = %q", got)
	}
}

func TestWait_PodFailsImmediately(t *testing.T) {
	cs := fake.NewSimpleClientset()
	s := NewK8sSpawner(cs, "backup")
	db, err := s.Spawn(context.Background(), Spec{
		NamePrefix:   "x",
		Image:        "redis:7-alpine",
		Port:         6379,
		ReadyTimeout: 5 * time.Second,
	}, testr.New(t))
	if err != nil {
		t.Fatal(err)
	}

	pods, _ := cs.CoreV1().Pods("backup").List(context.Background(), metav1.ListOptions{})
	pod := pods.Items[0].DeepCopy()
	pod.Status.Phase = corev1.PodFailed
	if _, err := cs.CoreV1().Pods("backup").UpdateStatus(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	if err := db.Wait(context.Background()); err == nil {
		t.Fatal("expected error for Failed pod")
	}
}

func TestWait_ProbeRunsAfterIP(t *testing.T) {
	cs := fake.NewSimpleClientset()
	s := NewK8sSpawner(cs, "backup")
	probeCalls := 0
	db, err := s.Spawn(context.Background(), Spec{
		NamePrefix:   "x",
		Image:        "redis:7-alpine",
		Port:         6379,
		ReadyTimeout: 5 * time.Second,
		Probe: func(ctx context.Context, endpoint string) error {
			probeCalls++
			if endpoint != "10.0.0.42:6379" {
				return errors.New("wrong endpoint passed to probe")
			}
			if probeCalls < 2 {
				return errors.New("not yet ready")
			}
			return nil
		},
	}, testr.New(t))
	if err != nil {
		t.Fatal(err)
	}

	pods, _ := cs.CoreV1().Pods("backup").List(context.Background(), metav1.ListOptions{})
	pod := pods.Items[0].DeepCopy()
	pod.Status.PodIP = "10.0.0.42"
	pod.Status.Phase = corev1.PodRunning
	if _, err := cs.CoreV1().Pods("backup").UpdateStatus(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	if err := db.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if probeCalls < 2 {
		t.Errorf("probe calls = %d, want at least 2 (retry path)", probeCalls)
	}
}

func TestStop_DeletesPod(t *testing.T) {
	cs := fake.NewSimpleClientset()
	s := NewK8sSpawner(cs, "backup")
	db, err := s.Spawn(context.Background(), Spec{
		NamePrefix:   "x",
		Image:        "redis:7-alpine",
		Port:         6379,
		ReadyTimeout: 5 * time.Second,
	}, testr.New(t))
	if err != nil {
		t.Fatal(err)
	}

	if err := db.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	pods, _ := cs.CoreV1().Pods("backup").List(context.Background(), metav1.ListOptions{})
	if len(pods.Items) != 0 {
		t.Errorf("expected pod deleted, got %d", len(pods.Items))
	}

	// Idempotent: second Stop on already-deleted pod should not error.
	if err := db.Stop(context.Background()); err != nil {
		t.Errorf("second Stop returned error: %v", err)
	}
}

func TestSanitiseDNS1123(t *testing.T) {
	cases := map[string]string{
		"prod-users":      "prod-users",
		"PROD_USERS":      "prod-users",
		"prod.users.db":   "prod-users-db",
		"--leading--":     "leading",
		"":                "x",
		"123abc":          "123abc",
	}
	for in, want := range cases {
		if got := sanitiseDNS1123(in); got != want {
			t.Errorf("sanitiseDNS1123(%q) = %q, want %q", in, got, want)
		}
	}
}
