// Worker — one-shot backup runner. Each invocation backs up exactly ONE
// BackupSource and exits. Launched by a Kubernetes Job created from the
// CronJob the operator manages.
//
//	backup-worker --source prod-users-db --namespace backup
//
// The worker:
//  1. Reads the BackupSource it was pointed at plus its credentials Secret.
//  2. Lists BackupDestinations in the same namespace, resolves each one's
//     credentials Secret, then applies the source's optional allow-list.
//  3. Runs the existing Pipeline.Run end-to-end.
//  4. Exits 0 on success, 1 on failure — Kubernetes records job status.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"

	"github.com/go-logr/logr"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"backup-operator/analyzer"
	"backup-operator/api/v1alpha1"
	"backup-operator/assert"
	"backup-operator/config"
	"backup-operator/crypto"
	"backup-operator/internal/backup"
	"backup-operator/internal/secrets"
	"backup-operator/verifier/ephemeral"
	verifierFactory "backup-operator/verifier/factory"

	apitypes "k8s.io/apimachinery/pkg/types"
)

// staticDestProvider implements backup.DestinationProvider with a fixed list —
// the worker resolves destinations exactly once at startup, so caching adds
// no value.
type staticDestProvider struct {
	dests []*secrets.Destination
}

func (s *staticDestProvider) Destinations() []*secrets.Destination { return s.dests }

// sourceEventEmitter records Kubernetes Events against the BackupSource for
// audit-trail compliance (DSGVO Art. 30, SOC2). Events are visible via
// `kubectl describe backupsource <name>` and in cluster audit logs.
type sourceEventEmitter struct {
	recorder record.EventRecorder
	ref      *corev1.ObjectReference
}

func (e *sourceEventEmitter) Emit(eventType, reason, message string) {
	e.recorder.Event(e.ref, eventType, reason, message)
}

// Version is overridden via -ldflags "-X main.Version=..." at build time so
// worker logs can be correlated with the image that produced them.
var Version = "dev"

func main() { os.Exit(run()) }

func run() int {
	sourceName := flag.String("source", "", "name of the BackupSource to back up")
	namespace := flag.String("namespace", "", "namespace of the BackupSource (defaults to POD_NAMESPACE)")
	// Bind zap's flags BEFORE the single Parse so --zap-log-level is known.
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	if *sourceName == "" {
		die("flag --source is required")
	}

	err := config.InitializeConfigModule([]config.ConfigItemDescription{
		{Key: "AGE_PUBLIC_KEYS", Optional: false},
		{Key: "RUN_TIMEOUT_SECONDS", Optional: true, Default: config.DefaultRunTimeoutSeconds, Validate: config.ValidatePositiveInt},
		{Key: "TEMP_DIR", Optional: true, Default: config.DefaultTempDir},
		{Key: "DEFAULT_RETENTION_DAYS", Optional: true, Default: config.DefaultRetentionDays, Validate: config.ValidateNonNegInt},
		{Key: "DEFAULT_MIN_KEEP", Optional: true, Default: config.DefaultMinKeep, Validate: config.ValidateNonNegInt},
		{Key: "DEFAULT_SCHEDULE", Optional: true, Default: config.DefaultSchedule}, // unused here, but the flattener needs it
		{Key: "POD_NAMESPACE", Optional: true},
		// POD_NAME / POD_UID come from the Downward API in the CronJob
		// pod-spec; the restore-verifier uses them as OwnerReference
		// targets for spawned ephemeral DB pods. Optional because
		// stream-validate / off don't need them.
		{Key: "POD_NAME", Optional: true},
		{Key: "POD_UID", Optional: true},
	})
	assert.NoError(err, "failed to initialize config module")

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("worker")

	ns := *namespace
	if ns == "" {
		ns = config.GetValue("POD_NAMESPACE")
	}
	if ns == "" {
		die("namespace not provided and POD_NAMESPACE env unset")
	}
	log.Info("backup worker starting", "version", Version, "source", *sourceName, "namespace", ns)

	cfg, err := loadKubeConfig()
	assert.NoError(err, "failed to load kubeconfig")
	cs, err := kubernetes.NewForConfig(cfg)
	assert.NoError(err, "failed to build kubernetes client")
	cl, err := client.New(cfg, client.Options{Scheme: workerScheme()})
	assert.NoError(err, "failed to build controller-runtime client")

	sigCtx, sigStop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer sigStop()
	ctx, cancel := context.WithTimeout(sigCtx, config.GetDurationSeconds("RUN_TIMEOUT_SECONDS"))
	defer cancel()

	var bs v1alpha1.BackupSource
	if err := cl.Get(ctx, client.ObjectKey{Namespace: ns, Name: *sourceName}, &bs); err != nil {
		log.Error(err, "get backupsource", "source", *sourceName, "namespace", ns)
		return 1
	}
	var creds corev1.Secret
	credsKey := client.ObjectKey{Namespace: ns, Name: bs.Spec.Database.CredentialsSecretRef.Name}
	if err := cl.Get(ctx, credsKey, &creds); err != nil {
		log.Error(err, "get credentials secret", "secret", credsKey.Name, "source", bs.Name)
		return 1
	}

	src, err := secrets.FromBackupSource(&bs, &creds, config.GetValue("DEFAULT_SCHEDULE"))
	if err != nil {
		log.Error(err, "flatten backupsource")
		return 1
	}

	tempDir := config.GetValue("TEMP_DIR")
	if src.TLSCA != nil {
		cleanup, err := materialiseCA(ctx, cl, ns, src, tempDir)
		if err != nil {
			log.Error(err, "materialise TLS CA", "secret", src.TLSCA.Name, "key", src.TLSCA.Key)
			return 1
		}
		defer cleanup()
	}

	dests, err := loadDestinations(ctx, cl, ns, src, log)
	if err != nil {
		log.Error(err, "load destinations")
		return 1
	}
	if len(dests) == 0 {
		log.Error(nil, "no destinations matched the source's allow-list", "target", src.TargetName)
		return 1
	}

	enc, err := crypto.NewFromPublicKeys(config.GetValue("AGE_PUBLIC_KEYS"))
	assert.NoError(err, "failed to initialize age encryptor")

	policy := backup.RetentionPolicy{
		Days:    config.GetInt("DEFAULT_RETENTION_DAYS"),
		MinKeep: config.GetInt("DEFAULT_MIN_KEEP"),
	}

	events, flushEvents := buildEventEmitter(cs, &bs)
	defer flushEvents()

	pipeline := backup.NewPipeline(
		enc,
		analyzer.NewAnalyzer(),
		tempDir,
		&staticDestProvider{dests: dests},
		policy,
		log.WithName("pipeline"),
		backup.WithEvents(events),
		backup.WithVerifierFactory(verifierFactory.New),
		backup.WithRestoreSpawner(
			ephemeral.NewK8sSpawner(cs, ns),
			ns,
			workerOwnerRef(),
		),
	)

	if err := pipeline.Run(ctx, src); err != nil {
		log.Error(err, "backup run failed", "target", src.TargetName)
		return 1
	}
	log.Info("backup run completed", "target", src.TargetName)
	return 0
}

// workerScheme registers exactly the kinds the worker reads: core Secrets
// and the backup.mogenius.io API group.
func workerScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	return s
}

// loadDestinations lists BackupDestinations in the namespace, resolves each
// one's credentials Secret, flattens it, then filters via the source's
// allow-list. A destination that fails to resolve is logged and skipped —
// one broken destination must not block all uploads. Names are unique per
// namespace by construction, so no logical-name de-duplication is needed.
func loadDestinations(ctx context.Context, cl client.Client, ns string, src *secrets.Source, log logr.Logger) ([]*secrets.Destination, error) {
	var list v1alpha1.BackupDestinationList
	if err := cl.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return nil, err
	}
	out := make([]*secrets.Destination, 0, len(list.Items))
	for i := range list.Items {
		bd := &list.Items[i]
		if !src.AllowsDestination(bd.Name) {
			continue
		}
		var creds corev1.Secret
		key := client.ObjectKey{Namespace: ns, Name: bd.Spec.CredentialsSecretRef.Name}
		if err := cl.Get(ctx, key, &creds); err != nil {
			if apierrors.IsNotFound(err) {
				log.Error(nil, "skipping destination: credentials secret not found", "destination", bd.Name, "secret", key.Name)
				continue
			}
			return nil, fmt.Errorf("get credentials secret %q for destination %q: %w", key.Name, bd.Name, err)
		}
		dest, err := secrets.FromBackupDestination(bd, &creds)
		if err != nil {
			log.Error(err, "skipping invalid destination", "destination", bd.Name)
			continue
		}
		out = append(out, dest)
	}
	return out, nil
}

// materialiseCA writes the CA bundle referenced by spec.database.tls.caSecretRef
// to the worker's temp volume (the only writable path under
// readOnlyRootFilesystem) and points the dumper at it. The file is 0600 and
// removed when the run ends; the pod's emptyDir goes with the pod anyway.
func materialiseCA(ctx context.Context, cl client.Client, ns string, src *secrets.Source, tempDir string) (func(), error) {
	var sec corev1.Secret
	if err := cl.Get(ctx, client.ObjectKey{Namespace: ns, Name: src.TLSCA.Name}, &sec); err != nil {
		return nil, fmt.Errorf("get CA secret: %w", err)
	}
	pem, ok := sec.Data[src.TLSCA.Key]
	if !ok || len(pem) == 0 {
		return nil, fmt.Errorf("CA secret %q has no data key %q", src.TLSCA.Name, src.TLSCA.Key)
	}
	if err := os.MkdirAll(tempDir, 0o755); err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	path := filepath.Join(tempDir, "tls-ca-"+src.ObjectName+".pem")
	if err := os.WriteFile(path, pem, 0o600); err != nil {
		return nil, fmt.Errorf("write CA file: %w", err)
	}
	secrets.ApplyTLSCAPath(src, path)
	return func() { _ = os.Remove(path) }, nil
}

// loadKubeConfig prefers in-cluster config (the normal case for CronJob pods),
// then falls back to KUBECONFIG/~/.kube/config so the binary can also be run
// locally for one-off testing without rebuilding.
func loadKubeConfig() (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{},
	).ClientConfig()
}

// workerOwnerRef builds the OwnerReference the restore-verifier uses
// when spawning ephemeral DB pods. Tying spawned pods to the worker
// pod's UID lets K8s GC cascade-delete them as soon as the worker
// terminates — even if the verifier never reaches Stop() (panic, OOM,
// node drain). Returns nil if Downward API didn't populate the env;
// the spawner then logs a warning and relies on best-effort Stop()
// only.
func workerOwnerRef() *metav1.OwnerReference {
	name := config.GetValue("POD_NAME")
	uid := config.GetValue("POD_UID")
	if name == "" || uid == "" {
		return nil
	}
	t := true
	return &metav1.OwnerReference{
		APIVersion:         "v1",
		Kind:               "Pod",
		Name:               name,
		UID:                apitypes.UID(uid),
		BlockOwnerDeletion: &t,
		Controller:         nil,
	}
}

// buildEventEmitter creates a Kubernetes EventRecorder that emits events
// against the BackupSource. Events form a permanent audit trail visible via
// `kubectl describe backupsource <name>` and cluster audit logs. The
// returned cleanup function shuts down the broadcaster, flushing any
// buffered events before the process exits.
func buildEventEmitter(cs kubernetes.Interface, bs *v1alpha1.BackupSource) (backup.EventEmitter, func()) {
	s := runtime.NewScheme()
	_ = scheme.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{
		Interface: cs.CoreV1().Events(bs.Namespace),
	})
	recorder := eventBroadcaster.NewRecorder(s, corev1.EventSource{Component: "backup-worker"})
	ref := &corev1.ObjectReference{
		Kind:       v1alpha1.KindBackupSource,
		Namespace:  bs.Namespace,
		Name:       bs.Name,
		UID:        bs.UID,
		APIVersion: v1alpha1.GroupVersion.String(),
	}
	return &sourceEventEmitter{recorder: recorder, ref: ref}, eventBroadcaster.Shutdown
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}
