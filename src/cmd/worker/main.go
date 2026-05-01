// Worker — one-shot backup runner. Each invocation backs up exactly ONE
// source database and exits. Launched by a Kubernetes Job created from the
// CronJob the operator manages.
//
//	backup-worker --source-secret prod-users-db --namespace backup
//
// The worker:
//   1. Reads the source Secret it was pointed at.
//   2. Lists destination Secrets in the same namespace via label selector,
//      then applies the source's optional destination allow-list.
//   3. Runs the existing Pipeline.Run end-to-end.
//   4. Exits 0 on success, 1 on failure — Kubernetes records job status.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"backup-operator/analyzer"
	"backup-operator/assert"
	"backup-operator/config"
	"backup-operator/crypto"
	"backup-operator/internal/backup"
	"backup-operator/internal/labels"
	"backup-operator/internal/secrets"
)

// staticDestProvider implements backup.DestinationProvider with a fixed list —
// the worker resolves destinations exactly once at startup, so caching adds
// no value.
type staticDestProvider struct {
	dests []*secrets.Destination
}

func (s *staticDestProvider) Destinations() []*secrets.Destination { return s.dests }

// secretEventEmitter records Kubernetes Events against the source Secret for
// audit-trail compliance (DSGVO Art. 30, SOC2). Events are visible via
// `kubectl describe secret <source>` and in cluster audit logs.
type secretEventEmitter struct {
	recorder record.EventRecorder
	ref      *corev1.ObjectReference
}

func (e *secretEventEmitter) Emit(eventType, reason, message string) {
	e.recorder.Event(e.ref, eventType, reason, message)
}

func main() { os.Exit(run()) }

func run() int {
	sourceSecret := flag.String("source-secret", "", "name of the source Secret to back up")
	namespace := flag.String("namespace", "", "namespace of the source Secret (defaults to POD_NAMESPACE)")
	flag.Parse()

	if *sourceSecret == "" {
		die("flag --source-secret is required")
	}

	err := config.InitializeConfigModule([]config.ConfigItemDescription{
		{Key: "AGE_PUBLIC_KEYS", Optional: false},
		{Key: "RUN_TIMEOUT_SECONDS", Optional: true, Default: "3600", Validate: validateInt},
		{Key: "TEMP_DIR", Optional: true, Default: "/tmp/backup-operator"},
		{Key: "DEFAULT_RETENTION_DAYS", Optional: true, Default: "30", Validate: validateNonNegInt},
		{Key: "DEFAULT_MIN_KEEP", Optional: true, Default: "3", Validate: validateNonNegInt},
		{Key: "DEFAULT_SCHEDULE", Optional: true, Default: "0 2 * * *"}, // unused here, but parser needs it
		{Key: "POD_NAMESPACE", Optional: true},
	})
	assert.NoError(err, "failed to initialize config module")

	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("worker")

	ns := *namespace
	if ns == "" {
		ns = config.GetValue("POD_NAMESPACE")
	}
	if ns == "" {
		die("namespace not provided and POD_NAMESPACE env unset")
	}

	cfg, err := loadKubeConfig()
	assert.NoError(err, "failed to load kubeconfig")
	cs, err := kubernetes.NewForConfig(cfg)
	assert.NoError(err, "failed to build kubernetes client")

	sigCtx, sigStop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer sigStop()
	runTimeoutSec, _ := strconv.Atoi(config.GetValue("RUN_TIMEOUT_SECONDS"))
	ctx, cancel := context.WithTimeout(sigCtx, time.Duration(runTimeoutSec)*time.Second)
	defer cancel()

	srcSecret, err := cs.CoreV1().Secrets(ns).Get(ctx, *sourceSecret, metav1.GetOptions{})
	if err != nil {
		log.Error(err, "get source secret", "secret", *sourceSecret, "namespace", ns)
		return 1
	}

	src, err := secrets.ParseSource(srcSecret, config.GetValue("DEFAULT_SCHEDULE"))
	if err != nil {
		log.Error(err, "parse source secret")
		return 1
	}

	dests, err := loadDestinations(ctx, cs, ns, src)
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

	defaultRet, _ := strconv.Atoi(config.GetValue("DEFAULT_RETENTION_DAYS"))
	defaultMin, _ := strconv.Atoi(config.GetValue("DEFAULT_MIN_KEEP"))
	policy := backup.RetentionPolicy{Days: defaultRet, MinKeep: defaultMin}

	events, flushEvents := buildEventEmitter(cs, srcSecret)
	defer flushEvents()

	pipeline := backup.NewPipelineWithEvents(
		enc,
		analyzer.NewAnalyzer(),
		config.GetValue("TEMP_DIR"),
		&staticDestProvider{dests: dests},
		policy,
		log.WithName("pipeline"),
		events,
	)

	if err := pipeline.Run(ctx, src); err != nil {
		log.Error(err, "backup run failed", "target", src.TargetName)
		return 1
	}
	log.Info("backup run completed", "target", src.TargetName)
	return 0
}

// loadDestinations lists Secrets in the namespace carrying the destination
// role label, parses each, then filters via the source's allow-list. A
// malformed destination Secret is logged and skipped — one bad Secret must
// not block all uploads.
func loadDestinations(ctx context.Context, cs kubernetes.Interface, ns string, src *secrets.Source) ([]*secrets.Destination, error) {
	list, err := cs.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{
		LabelSelector: labels.LabelRole + "=" + labels.RoleDestination,
	})
	if err != nil {
		return nil, err
	}
	out := make([]*secrets.Destination, 0, len(list.Items))
	for i := range list.Items {
		s := &list.Items[i]
		dest, err := secrets.ParseDestination(s)
		if err != nil {
			ctrl.Log.WithName("worker").Error(err, "skipping invalid destination", "secret", s.Name)
			continue
		}
		if !src.AllowsDestination(dest.Name) {
			continue
		}
		out = append(out, dest)
	}
	return out, nil
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

func validateInt(v string) error {
	if _, err := strconv.Atoi(v); err != nil {
		return fmt.Errorf("must be integer: %w", err)
	}
	return nil
}

func validateNonNegInt(v string) error {
	n, err := strconv.Atoi(v)
	if err != nil {
		return fmt.Errorf("must be integer: %w", err)
	}
	if n < 0 {
		return fmt.Errorf("must be >= 0")
	}
	return nil
}

// buildEventEmitter creates a Kubernetes EventRecorder that emits events
// against the source Secret. Events form a permanent audit trail visible
// via `kubectl describe secret <source>` and cluster audit logs.
// The returned cleanup function shuts down the broadcaster, flushing
// any buffered events before the process exits.
func buildEventEmitter(cs kubernetes.Interface, sec *corev1.Secret) (backup.EventEmitter, func()) {
	s := runtime.NewScheme()
	_ = scheme.AddToScheme(s)
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{
		Interface: cs.CoreV1().Events(sec.Namespace),
	})
	recorder := eventBroadcaster.NewRecorder(s, corev1.EventSource{Component: "backup-worker"})
	ref := &corev1.ObjectReference{
		Kind:       "Secret",
		Namespace:  sec.Namespace,
		Name:       sec.Name,
		UID:        sec.UID,
		APIVersion: "v1",
	}
	return &secretEventEmitter{recorder: recorder, ref: ref}, eventBroadcaster.Shutdown
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}
