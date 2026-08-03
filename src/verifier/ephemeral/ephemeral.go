// Package ephemeral spawns short-lived database pods inside the cluster
// so the restore-verifier can perform an actual restore against a real
// engine and run smoke queries. Each spawned pod uses an emptyDir volume
// (no PVC) sized for the verifier mode and dies as soon as the worker pod
// (its OwnerReference target) is garbage-collected.
//
// The Spawner interface keeps callers free of client-go imports — the
// verifier package, which talks to Spawner, can be unit-tested with a
// fake Spawner that returns a mocked Endpoint instead of a real Pod.
package ephemeral

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Spec describes what the verifier wants spawned. Image / Port / EnvVars
// are engine-specific (filled in by the restore engine). VolumeSizeBytes
// translates to emptyDir.sizeLimit. RunAsUID is required because the
// project's PodSecurityAdmission level is "restricted" — root pods get
// rejected at admission.
type Spec struct {
	NamePrefix     string
	Image          string
	Port           int32
	EnvVars        []corev1.EnvVar
	Args           []string
	Command        []string
	VolumeSizeBytes int64
	VolumeMountPath string
	ReadyTimeout   time.Duration

	// RunAsUID is the UID the container runs as. It MUST match the non-root
	// DB user baked into Image — postgres:*-alpine is UID 70, the Debian
	// postgres/mysql/mariadb/mongo images and redis:*-alpine are 999. A
	// mismatch makes the engine's entrypoint fail ("could not look up
	// effective user ID") under runAsNonRoot, so the pod never becomes ready.
	// 0 falls back to 999 (the common case).
	RunAsUID int64

	// ActiveDeadline bounds the pod's total lifetime as a safety net. The
	// OwnerReference cascades only when the worker POD OBJECT is deleted — a
	// SIGKILL/OOM'd worker lingers as a Failed object, so its owned verifier
	// DB pod (holding a full data copy) would otherwise run until something
	// else reaps the worker. 0 falls back to a sane default in Spawn.
	ActiveDeadline time.Duration

	// OwnerRef ties the spawned pod's lifetime to the worker pod. When
	// the worker exits and is GC'd, the spawned pod cascades. nil means
	// "no owner" — only acceptable in tests.
	OwnerRef *metav1.OwnerReference

	// Probe is the engine-specific readiness check. It runs after the
	// Pod reports Ready=true (basic kubelet probe) and confirms the DB
	// is actually accepting connections. Connection-level readiness is
	// fundamentally engine-specific (psql -c "SELECT 1" / mysqladmin
	// ping / mongosh --eval "db.runCommand({ping:1})"); we delegate.
	Probe func(ctx context.Context, endpoint string) error
}

// DB is the handle returned by Spawner.Spawn. Endpoint() is host:port
// for the spawned pod (Pod IP + port), available only after Wait()
// returns. Stop() removes the pod immediately rather than waiting for
// OwnerRef cascade — best-effort, won't fail if the pod is already gone.
type DB interface {
	Wait(ctx context.Context) error
	Endpoint() string
	Stop(ctx context.Context) error
}

// Spawner is the abstraction the verifier consumes.
type Spawner interface {
	Spawn(ctx context.Context, spec Spec, log logr.Logger) (DB, error)
}

// NewK8sSpawner returns a Spawner backed by the given kubernetes clientset
// in the given namespace. Worker SA needs pods:create/get/list/watch/delete
// in this namespace.
func NewK8sSpawner(cs kubernetes.Interface, namespace string) Spawner {
	return &k8sSpawner{cs: cs, namespace: namespace}
}

type k8sSpawner struct {
	cs        kubernetes.Interface
	namespace string
}

func (s *k8sSpawner) Spawn(ctx context.Context, spec Spec, log logr.Logger) (DB, error) {
	if spec.Image == "" {
		return nil, fmt.Errorf("ephemeral: image required")
	}
	if spec.Port <= 0 {
		return nil, fmt.Errorf("ephemeral: port required")
	}
	if spec.VolumeMountPath == "" {
		spec.VolumeMountPath = "/data"
	}
	if spec.ReadyTimeout <= 0 {
		spec.ReadyTimeout = 5 * time.Minute
	}
	if spec.ActiveDeadline <= 0 {
		// Matches the operator's default RUN_TIMEOUT_SECONDS; a verifier pod
		// should never outlive the worker run that drives it.
		spec.ActiveDeadline = time.Hour
	}

	name := buildPodName(spec.NamePrefix)
	pod := s.buildPodSpec(name, spec)

	log.Info("spawning ephemeral verifier DB pod",
		"name", name, "image", spec.Image, "namespace", s.namespace, "volumeSizeBytes", spec.VolumeSizeBytes)

	created, err := s.cs.CoreV1().Pods(s.namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("create ephemeral pod %s: %w", name, err)
	}
	return &k8sDB{
		cs:        s.cs,
		namespace: s.namespace,
		name:      created.Name,
		port:      spec.Port,
		probe:     spec.Probe,
		readyTO:   spec.ReadyTimeout,
		log:       log.WithName("ephemeral-db"),
	}, nil
}

// buildPodName: namePrefix-<8 random hex> — short enough to leave
// headroom for the 63-char DNS-1123 limit even with long target names.
func buildPodName(prefix string) string {
	if prefix == "" {
		prefix = "verifier"
	}
	prefix = sanitiseDNS1123(prefix)
	if len(prefix) > 40 {
		prefix = prefix[:40]
	}
	return fmt.Sprintf("%s-%s", prefix, randHex(8))
}

func (s *k8sSpawner) buildPodSpec(name string, spec Spec) *corev1.Pod {
	runAsNonRoot := true
	allowPrivEsc := false
	// Stock DB images write runtime state to root-fs paths outside the data
	// volume at startup — postgres needs /var/run/postgresql (socket+lock),
	// mysql /run/mysqld, mongo /tmp. With readOnlyRootFilesystem=true the
	// entrypoint can't create those and the container crashes into Failed.
	// This is a throwaway verifier pod (seconds-lived, OwnerRef-GC'd, only
	// holds a copy of data the cluster already has), so we trade root-fs
	// hardening for a working restore. Still PSA-restricted-compliant:
	// readOnlyRootFilesystem is not part of the restricted standard
	// (runAsNonRoot / seccomp / drop-ALL / no-privesc all stay on).
	readOnlyRootFs := false
	uid := spec.RunAsUID
	if uid <= 0 {
		uid = 999
	}
	// FSGroup owns the emptyDir mount so the (non-root) DB user can write its
	// data dir; keep it aligned with the run-as UID.
	fsGroup := uid

	cpuReq := resource.MustParse("100m")
	memReq := resource.MustParse("256Mi")
	cpuLim := resource.MustParse("2000m")
	memLim := resource.MustParse("2Gi")

	volSize := spec.VolumeSizeBytes
	if volSize <= 0 {
		volSize = 5 * 1024 * 1024 * 1024 // 5 GiB sane default; spec sets per-mode default
	}
	emptyDir := &corev1.EmptyDirVolumeSource{
		SizeLimit: ptrQuantity(volSize),
	}

	owners := []metav1.OwnerReference{}
	if spec.OwnerRef != nil {
		owners = append(owners, *spec.OwnerRef)
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: s.namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by":             "backup-operator",
				"backup.mogenius.io/role":                  "verifier-target",
				"backup.mogenius.io/verifier-target-prefix": dns1123Label(spec.NamePrefix),
			},
			OwnerReferences: owners,
		},
		Spec: corev1.PodSpec{
			RestartPolicy:         corev1.RestartPolicyNever,
			ActiveDeadlineSeconds: ptrInt64(int64(spec.ActiveDeadline.Seconds())),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: &runAsNonRoot,
				RunAsUser:    &uid,
				FSGroup:      &fsGroup,
				SeccompProfile: &corev1.SeccompProfile{
					Type: corev1.SeccompProfileTypeRuntimeDefault,
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "data",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: emptyDir,
					},
				},
			},
			Containers: []corev1.Container{
				{
					Name:    "db",
					Image:   spec.Image,
					Command: spec.Command,
					Args:    spec.Args,
					Env:     spec.EnvVars,
					Ports: []corev1.ContainerPort{
						{ContainerPort: spec.Port, Protocol: corev1.ProtocolTCP},
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "data", MountPath: spec.VolumeMountPath},
					},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    cpuReq,
							corev1.ResourceMemory: memReq,
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    cpuLim,
							corev1.ResourceMemory: memLim,
						},
					},
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: &allowPrivEsc,
						RunAsNonRoot:             &runAsNonRoot,
						RunAsUser:                &uid,
						ReadOnlyRootFilesystem:   &readOnlyRootFs,
						Capabilities: &corev1.Capabilities{
							Drop: []corev1.Capability{"ALL"},
						},
					},
				},
			},
		},
	}
	return pod
}

func ptrQuantity(bytes int64) *resource.Quantity {
	q := resource.NewQuantity(bytes, resource.BinarySI)
	return q
}

func ptrInt64(v int64) *int64 { return &v }

// dns1123Label sanitises to a valid label VALUE and truncates to the 63-char
// limit. NamePrefix is "verify-<targetName>" and target names may be up to 253
// chars, so an untruncated label value would make the API reject the pod.
func dns1123Label(in string) string {
	s := sanitiseDNS1123(in)
	if len(s) > 63 {
		s = strings.Trim(s[:63], "-")
	}
	return s
}

// sanitiseDNS1123 lowercases and replaces invalid chars with '-' so
// arbitrary target names can become Pod names.
func sanitiseDNS1123(in string) string {
	in = strings.ToLower(in)
	var b strings.Builder
	for _, r := range in {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "x"
	}
	return out
}

// randHex returns n hex chars from crypto/rand. Used only for pod-name
// uniqueness; collisions are merely an annoyance, not a security issue.
func randHex(n int) string {
	b := make([]byte, (n+1)/2)
	if _, err := cryptoRand(b); err != nil {
		// Fallback to a timestamp tail: still unique-enough for
		// pod-name purposes, never blocks the verifier.
		return fmt.Sprintf("%0*x", n, time.Now().UnixNano()&0xffffffff)
	}
	return fmt.Sprintf("%x", b)[:n]
}
