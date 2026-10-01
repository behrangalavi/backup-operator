// Operator — reconciles BackupSource objects to managed K8s CronJobs.
// It does not run backups itself; CronJob-spawned Job pods do, executing
// the worker binary (cmd/worker).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	nethttppprof "net/http/pprof"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"backup-operator/api/v1alpha1"
	"backup-operator/assert"
	"backup-operator/config"
	"backup-operator/controllers"
	"backup-operator/docs"
	"backup-operator/internal/alerts"
	"backup-operator/internal/safe"
	"backup-operator/internal/scheduler"
	"backup-operator/metrics"
	"backup-operator/ui"
)

var Version = "dev" // overridden via -ldflags at build time

func adaptKubeConfig(c *rest.Config) {
	c.QPS = 50
	c.Burst = 100
}

func main() {
	err := config.InitializeConfigModule([]config.ConfigItemDescription{
		{Key: "WATCH_NAMESPACE", Optional: true},
		{Key: "POD_NAMESPACE", Optional: true},
		{Key: "LEADER_ELECTION_ID", Optional: true},
		// A bad default schedule used to surface only when the first CronJob
		// create failed; validate it here so a Helm typo fails the pod start.
		{Key: "DEFAULT_SCHEDULE", Optional: true, Default: config.DefaultSchedule, Validate: scheduler.ValidateSchedule},
		// Flows straight into each Job's activeDeadlineSeconds; a value <= 0
		// makes every backup Job fail immediately.
		{Key: "RUN_TIMEOUT_SECONDS", Optional: true, Default: config.DefaultRunTimeoutSeconds, Validate: config.ValidatePositiveInt},
		{Key: "TEMP_DIR", Optional: true, Default: config.DefaultTempDir},
		{Key: "TEMP_DIR_SIZE", Optional: true, Default: "10Gi"},
		{Key: "DEFAULT_RETENTION_DAYS", Optional: true, Default: config.DefaultRetentionDays, Validate: config.ValidateNonNegInt},
		{Key: "DEFAULT_MIN_KEEP", Optional: true, Default: config.DefaultMinKeep, Validate: config.ValidateNonNegInt},
		{Key: "METRICS_REFRESH_INTERVAL_SECONDS", Optional: true, Default: "30", Validate: config.ValidateIntRange(5, 86400)},

		// Storage scrubber — re-hashes the most recent dump per
		// (source, destination) on a schedule and compares against the
		// SHA256 recorded in meta.json. Detects silent storage corruption.
		// Off by default because each scrub re-streams a full encrypted
		// dump from storage; enable once you've sized your egress.
		{Key: "STORAGE_SCRUB_ENABLED", Optional: true, Default: "false"},
		{Key: "STORAGE_SCRUB_INTERVAL_HOURS", Optional: true, Default: "24", Validate: config.ValidatePositiveInt},
		// Cap on (target, destination) pairs re-hashed per scrub tick; the
		// rest continue round-robin on the following ticks.
		{Key: "STORAGE_SCRUB_MAX_PAIRS_PER_TICK", Optional: true, Default: "500", Validate: config.ValidatePositiveInt},

		// Worker pod template — these flow into every CronJob the reconciler
		// produces. Set by Helm; required so CronJobs are runnable.
		{Key: "WORKER_IMAGE", Optional: false},
		{Key: "WORKER_IMAGE_PULL_POLICY", Optional: true, Default: "IfNotPresent"},
		// Comma-separated pull-secret names, mirrored from the chart's
		// imagePullSecrets so worker pods can pull the (fat) image from a
		// private registry — without this a private-registry install fails
		// with node-dependent ImagePullBackOff on worker pods only.
		{Key: "WORKER_IMAGE_PULL_SECRETS", Optional: true},
		{Key: "WORKER_SERVICE_ACCOUNT", Optional: false},
		{Key: "AGE_SECRET_NAME", Optional: false},
		// Comma- or newline-separated age recipients the chart was installed
		// with. Seeded into BackupRecipient objects at startup (idempotent) —
		// the chart cannot ship them as CRs because Helm applies templated
		// CRDs and CRs in one batch, and a CR whose CRD is not yet established
		// fails the very first install.
		{Key: "BOOTSTRAP_AGE_RECIPIENTS", Optional: true},

		// Worker resource limits — empty means no limits.
		{Key: "WORKER_CPU_LIMIT", Optional: true},
		{Key: "WORKER_MEMORY_LIMIT", Optional: true},
		{Key: "WORKER_CPU_REQUEST", Optional: true},
		{Key: "WORKER_MEMORY_REQUEST", Optional: true},

		// K8s-native retry on Job failure. Default 2 = initial attempt + 2
		// retries with exponential backoff (10s, 20s, 40s, ... capped at
		// 6m). Bounds the cost of permanent failures while catching
		// transient ones (DB blips, brief network partitions). 0 disables.
		{Key: "WORKER_BACKOFF_LIMIT", Optional: true, Default: "2", Validate: config.ValidateIntRange(0, 10)},

		// Worker pod scheduling / shape. JSON-encoded so the chart can pass
		// arbitrary Kubernetes structures through a flat env; unknown fields
		// are rejected so a typo in values.yaml fails the operator start
		// instead of silently scheduling workers anywhere.
		{Key: "WORKER_NODE_SELECTOR", Optional: true, Validate: validateJSON(func() any { return &map[string]string{} })},
		{Key: "WORKER_TOLERATIONS", Optional: true, Validate: validateJSON(func() any { return &[]corev1.Toleration{} })},
		{Key: "WORKER_AFFINITY", Optional: true, Validate: validateJSON(func() any { return &corev1.Affinity{} })},
		{Key: "WORKER_PRIORITY_CLASS_NAME", Optional: true},
		{Key: "WORKER_POD_ANNOTATIONS", Optional: true, Validate: validateJSON(func() any { return &map[string]string{} })},
		{Key: "WORKER_EXTRA_ENV", Optional: true, Validate: validateJSON(func() any { return &[]corev1.EnvVar{} })},
		{Key: "WORKER_EXTRA_VOLUMES", Optional: true, Validate: validateJSON(func() any { return &[]corev1.Volume{} })},
		{Key: "WORKER_EXTRA_VOLUME_MOUNTS", Optional: true, Validate: validateJSON(func() any { return &[]corev1.VolumeMount{} })},

		// UI dashboard — read-only timeline of run history. Disabled by
		// default to keep the operator's surface minimal; flip UI_ENABLED
		// to expose it on UI_ADDR.
		{Key: "UI_ENABLED", Optional: true, Default: "false"},
		{Key: "UI_ADDR", Optional: true, Default: ":8081"},
		// UI mutation gates — defense in depth on top of the (optional)
		// auth proxy. UI_READ_ONLY=true disables every mutating endpoint.
		// UI_ALLOW_KEY_MUTATION specifically gates the age-key add/remove
		// endpoints — that's the most security-critical mutation in the
		// UI (a hostile add silently widens future-backup decryption to
		// the attacker's private key), so it stays opt-in even when the
		// rest of the UI is read-write.
		{Key: "UI_READ_ONLY", Optional: true, Default: "false"},
		{Key: "UI_ALLOW_KEY_MUTATION", Optional: true, Default: "false"},
		// Hardening knobs against unauthenticated misuse. Defaults are
		// applied inside ui.New if 0/empty; we still surface them as env
		// values so operators can tune without rebuilding.
		{Key: "UI_MAX_BODY_BYTES", Optional: true, Default: "1048576", Validate: config.ValidatePositiveInt}, // 1 MiB
		{Key: "UI_MAX_SSE_CLIENTS", Optional: true, Default: "256", Validate: config.ValidatePositiveInt},
		// Alert integration. PROMETHEUS_URL points at the in-cluster
		// Prometheus that scrapes our ServiceMonitor and evaluates the
		// chart's PrometheusRule — when set, /api/alerts mirrors what
		// Alertmanager will route. When unset the UI falls back to a
		// local heuristic over our own metric registry, which is enough
		// to be useful but does not honor the rule's "for:" duration.
		{Key: "PROMETHEUS_URL", Optional: true},
		{Key: "ALERTMANAGER_URL", Optional: true},
		// Docs server. Independent listener so it can be exposed/scoped
		// separately from the management UI (read-only, public-facing
		// reference vs. mutating admin surface). DOCS_DIR points at the
		// directory that holds CLAUDE.md, README.md, and go.mod; the
		// Dockerfile populates /app/docs, locally a developer points it
		// at the repo root.
		{Key: "DOCS_ENABLED", Optional: true, Default: "false"},
		{Key: "DOCS_ADDR", Optional: true, Default: ":8083"},
		{Key: "DOCS_DIR", Optional: true, Default: "/app/docs"},
		// Debug-only Go pprof endpoint. Empty (default) = disabled. When set
		// (e.g. ":6060") the operator serves net/http/pprof on a dedicated
		// listener for heap/goroutine/CPU profiling. Off by default and never
		// enabled by the chart — pprof can dump memory contents and a CPU
		// profile is a cheap DoS, so it must only be turned on deliberately
		// for diagnosis and scoped away from any public ingress.
		{Key: "PPROF_ADDR", Optional: true},
	})
	assert.NoError(err, "failed to initialize config module")

	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	cfg := ctrl.GetConfigOrDie()
	adaptKubeConfig(cfg)

	metrics.Register(ctrlmetrics.Registry)

	watchNs := config.GetValue("WATCH_NAMESPACE")
	leaderElectionID := config.GetValue("LEADER_ELECTION_ID")
	scheme := runtime.NewScheme()
	assert.NoError(clientgoscheme.AddToScheme(scheme), "failed to register client-go scheme")
	assert.NoError(v1alpha1.AddToScheme(scheme), "failed to register backup.mogenius.io scheme")
	mgrOpts := ctrl.Options{
		Scheme:                        scheme,
		LeaderElection:                leaderElectionID != "",
		LeaderElectionID:              leaderElectionID,
		LeaderElectionNamespace:       config.GetValue("POD_NAMESPACE"),
		LeaderElectionReleaseOnCancel: true,
		HealthProbeBindAddress:        ":8082",
		// Secrets are read straight from the API server, never cached. The
		// operator only ever needs a credentials Secret by name (destination
		// probes, UI edits), and a cached client would otherwise start an
		// informer that holds EVERY Secret in the namespace — DB passwords,
		// TLS keys, unrelated application secrets — in operator memory.
		Client: client.Options{Cache: &client.CacheOptions{DisableFor: []client.Object{&corev1.Secret{}}}},
	}
	if watchNs != "" {
		mgrOpts.Cache = cache.Options{DefaultNamespaces: map[string]cache.Config{watchNs: {}}}
	}

	mgr, err := ctrl.NewManager(cfg, mgrOpts)
	assert.NoError(err, "failed to create controller manager")

	assert.NoError(mgr.AddHealthzCheck("healthz", healthz.Ping), "failed to add healthz check")
	assert.NoError(mgr.AddReadyzCheck("readyz", healthz.Ping), "failed to add readyz check")

	worker := controllers.WorkerSpec{
		Image:              config.GetValue("WORKER_IMAGE"),
		ImagePullPolicy:    corev1.PullPolicy(config.GetValue("WORKER_IMAGE_PULL_POLICY")),
		ImagePullSecrets:   parsePullSecrets(config.GetValue("WORKER_IMAGE_PULL_SECRETS")),
		ServiceAccountName: config.GetValue("WORKER_SERVICE_ACCOUNT"),
		AgeSecretName:      config.GetValue("AGE_SECRET_NAME"),
		TempDir:            config.GetValue("TEMP_DIR"),
		TempDirSize:        config.GetValue("TEMP_DIR_SIZE"),
		RunTimeoutSeconds:  int64(config.GetInt("RUN_TIMEOUT_SECONDS")),
		BackoffLimit:       int32(config.GetInt("WORKER_BACKOFF_LIMIT")),
		RetentionDaysDef:   config.GetValue("DEFAULT_RETENTION_DAYS"),
		MinKeepDef:         config.GetValue("DEFAULT_MIN_KEEP"),
		DefaultSchedule:    config.GetValue("DEFAULT_SCHEDULE"),
		Resources:          buildWorkerResources(),
		PriorityClassName:  config.GetValue("WORKER_PRIORITY_CLASS_NAME"),
	}
	// Validated at config init; a decode failure here would be a bug.
	worker.NodeSelector = jsonEnv[map[string]string]("WORKER_NODE_SELECTOR")
	worker.Tolerations = jsonEnv[[]corev1.Toleration]("WORKER_TOLERATIONS")
	worker.Affinity = jsonEnvPtr[corev1.Affinity]("WORKER_AFFINITY")
	worker.PodAnnotations = jsonEnv[map[string]string]("WORKER_POD_ANNOTATIONS")
	worker.ExtraEnv = jsonEnv[[]corev1.EnvVar]("WORKER_EXTRA_ENV")
	worker.ExtraVolumes = jsonEnv[[]corev1.Volume]("WORKER_EXTRA_VOLUMES")
	worker.ExtraVolumeMounts = jsonEnv[[]corev1.VolumeMount]("WORKER_EXTRA_VOLUME_MOUNTS")

	reconciler := &controllers.CronJobReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Logger: ctrl.Log.WithName("cronjob-reconciler"),
		Worker: worker,
	}
	err = reconciler.SetupWithManager(mgr)
	assert.NoError(err, "failed to setup cronjob reconciler")

	// One pool, shared by the refresher and the scrubber. Long-lived
	// storage clients per destination, so SFTP handshakes and S3 client
	// builds happen once per backend instead of once per call. See ADR
	// in CLAUDE.md §18.
	storagePool := controllers.NewStoragePool(ctrl.Log.WithName("storage-pool"))

	// broadcastFn is the SSE-publish bridge from controllers to the UI's
	// event broker. It's a function variable (not a direct method) so we
	// can wire it AFTER the controllers are constructed — the UI server
	// gets built further down inside the `if UI_ENABLED` block, but the
	// controllers' Reconcile loops only fire after mgr.Start(ctx) is
	// called, by which point broadcastFn has either been set (UI on) or
	// stayed nil (UI off, broadcasts become no-ops).
	var broadcastFn func(eventType, data string)
	broadcast := func(t, d string) {
		if broadcastFn != nil {
			broadcastFn(t, d)
		}
	}

	refresher := &controllers.MetricsRefresher{
		Client:          mgr.GetClient(),
		Logger:          ctrl.Log.WithName("metrics-refresher"),
		Namespace:       watchNs,
		Interval:        config.GetDurationSeconds("METRICS_REFRESH_INTERVAL_SECONDS"),
		DefaultSchedule: config.GetValue("DEFAULT_SCHEDULE"),
		Pool:            storagePool,
		Broadcast:       broadcast,
	}
	assert.NoError(mgr.Add(refresher), "failed to register metrics refresher")

	// Job watcher: emits SSE on every backup-Job state transition so the
	// live UI reflects job start/completion within ~1 s, instead of
	// waiting for the periodic 10 s SSE refresh tick.
	jobWatcher := &controllers.JobWatcher{
		Client:    mgr.GetClient(),
		Logger:    ctrl.Log.WithName("job-watcher"),
		Namespace: watchNs,
		Broadcast: broadcast,
	}
	assert.NoError(jobWatcher.SetupWithManager(mgr), "failed to setup job watcher")

	// Recipient reconciler: watches BackupRecipient objects and materialises
	// them into the operator-managed merged Secret named by AGE_SECRET_NAME —
	// the same name worker pods reference via `secretKeyRef`.
	recipientReconciler := &controllers.RecipientReconciler{
		Client:           mgr.GetClient(),
		Logger:           ctrl.Log.WithName("recipient-reconciler"),
		Namespace:        watchNs,
		MergedSecretName: config.GetValue("AGE_SECRET_NAME"),
	}

	// Bootstrap before the manager starts so the merged Secret exists with
	// the right content before any CronJob tick can race against an empty
	// or missing target. Uses a non-cached client because the manager's
	// cache only starts during mgr.Start(). Bootstrap also seeds the
	// chart-provided recipients (BOOTSTRAP_AGE_RECIPIENTS) as
	// BackupRecipient objects.
	bootstrapClient, err := client.New(cfg, client.Options{Scheme: mgr.GetScheme()})
	assert.NoError(err, "failed to create bootstrap client")
	recipientReconciler.Bootstrap(context.Background(), bootstrapClient, []string{config.GetValue("BOOTSTRAP_AGE_RECIPIENTS")})

	assert.NoError(recipientReconciler.SetupWithManager(mgr), "failed to setup recipient reconciler")

	if config.GetBool("STORAGE_SCRUB_ENABLED") {
		scrubber := &controllers.StorageScrubber{
			Client:          mgr.GetClient(),
			Logger:          ctrl.Log.WithName("storage-scrubber"),
			Namespace:       watchNs,
			Interval:        time.Duration(config.GetInt("STORAGE_SCRUB_INTERVAL_HOURS")) * time.Hour,
			MaxPairsPerTick: config.GetInt("STORAGE_SCRUB_MAX_PAIRS_PER_TICK"),
			Pool:            storagePool,
		}
		assert.NoError(mgr.Add(scrubber), "failed to register storage scrubber")
	}

	if config.GetBool("UI_ENABLED") {
		maxBody := config.GetInt64("UI_MAX_BODY_BYTES")
		maxSSE := config.GetInt("UI_MAX_SSE_CLIENTS")

		// Pick an alerts provider. Order: explicit Prometheus > local
		// fallback over our own metric registry. The chained provider
		// degrades gracefully when Prometheus is reachable at boot but
		// briefly unavailable later.
		//
		// Trim whitespace defensively — a trailing space in Helm values
		// (easy to introduce via --set or YAML quoting) would otherwise
		// land in the URL, fail url.Parse, and surface as a confusing
		// "unreachable" status with no obvious cause in the logs.
		promURL := strings.TrimSpace(config.GetValue("PROMETHEUS_URL"))
		alertmanagerURL := strings.TrimSpace(config.GetValue("ALERTMANAGER_URL"))
		var alertsProvider alerts.Provider = alerts.NewLocalProvider(metrics.Gatherer())
		if promURL != "" {
			alertsProvider = chainedProvider{
				primary:  alerts.NewPrometheusProvider(promURL),
				fallback: alertsProvider,
				log:      ctrl.Log.WithName("alerts"),
			}
		}

		uiServer, err := ui.New(ui.Config{
			Addr:            config.GetValue("UI_ADDR"),
			Namespace:       namespaceForUI(watchNs),
			Client:          mgr.GetClient(),
			APIReader:       mgr.GetAPIReader(),
			Logger:          ctrl.Log.WithName("ui"),
			AgeSecretName:   config.GetValue("AGE_SECRET_NAME"),
			DefaultSchedule: config.GetValue("DEFAULT_SCHEDULE"),
			Defaults: ui.OperatorDefaults{
				DefaultSchedule:      config.GetValue("DEFAULT_SCHEDULE"),
				RunTimeoutSeconds:    config.GetValue("RUN_TIMEOUT_SECONDS"),
				DefaultRetentionDays: config.GetValue("DEFAULT_RETENTION_DAYS"),
				DefaultMinKeep:       config.GetValue("DEFAULT_MIN_KEEP"),
				TempDir:              config.GetValue("TEMP_DIR"),
				TempDirSize:          config.GetValue("TEMP_DIR_SIZE"),
				WorkerCPULimit:       config.GetValue("WORKER_CPU_LIMIT"),
				WorkerMemoryLimit:    config.GetValue("WORKER_MEMORY_LIMIT"),
				WorkerCPURequest:     config.GetValue("WORKER_CPU_REQUEST"),
				WorkerMemoryRequest:  config.GetValue("WORKER_MEMORY_REQUEST"),
			},
			ReadOnly:             config.GetBool("UI_READ_ONLY"),
			AllowKeyMutation:     config.GetBool("UI_ALLOW_KEY_MUTATION"),
			MaxBodyBytes:         maxBody,
			MaxSSEClients:        maxSSE,
			AlertsProvider:       alertsProvider,
			PrometheusURL:        promURL,
			AlertmanagerURL:      alertmanagerURL,
			WorkerServiceAccount: config.GetValue("WORKER_SERVICE_ACCOUNT"),
			Pool:                 storagePool,
		})
		assert.NoError(err, "failed to construct UI server")
		// Wire the controllers' SSE bridge (declared earlier) to the
		// freshly-built broker. Set before mgr.Start so reconciles that
		// start firing can publish events. When UI is disabled,
		// broadcastFn stays nil and controller broadcasts no-op.
		broadcastFn = uiServer.Broadcast
		// Register before manager start so the cache and HTTP listener share
		// the manager's context (and shut down with it).
		assert.NoError(mgr.Add(uiServer), "failed to register UI server")
	}

	if config.GetBool("DOCS_ENABLED") {
		docsServer, err := docs.New(docs.Config{
			Addr:    config.GetValue("DOCS_ADDR"),
			DocsDir: config.GetValue("DOCS_DIR"),
			Logger:  ctrl.Log.WithName("docs"),
			Version: Version,
		})
		assert.NoError(err, "failed to construct docs server")
		assert.NoError(mgr.Add(docsServer), "failed to register docs server")
	}

	if addr := strings.TrimSpace(config.GetValue("PPROF_ADDR")); addr != "" {
		assert.NoError(mgr.Add(&pprofRunnable{addr: addr, log: ctrl.Log.WithName("pprof")}),
			"failed to register pprof server")
	}

	ctx := ctrl.SetupSignalHandler()
	if err := mgr.Start(ctx); err != nil {
		assert.NoError(err, "manager exited with error")
	}
}

// pprofRunnable serves net/http/pprof on a dedicated listener for live
// heap/goroutine/CPU profiling. Registered as a manager.Runnable so it shares
// the manager's context and shuts down with it. NeedLeaderElection=false so it
// runs on every replica (you want to profile the specific pod that's misbehaving,
// not only the leader). Gated entirely by PPROF_ADDR — see the config comment.
type pprofRunnable struct {
	addr string
	log  logr.Logger
}

func (*pprofRunnable) NeedLeaderElection() bool { return false }

func (p *pprofRunnable) Start(ctx context.Context) error {
	// Dedicated mux (not DefaultServeMux) so importing net/http/pprof can't
	// leak the profiling handlers onto any other server in this process.
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", nethttppprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", nethttppprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", nethttppprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", nethttppprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", nethttppprof.Trace)
	srv := &http.Server{Addr: p.addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	p.log.Info("starting pprof debug server", "addr", p.addr)
	errc := make(chan error, 1)
	go func() {
		defer safe.Goroutine(p.log, "pprof-listen", p.addr)
		errc <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// namespaceForUI mirrors the manager's watch scope — the dashboard only
// shows what the operator is responsible for. An empty WATCH_NAMESPACE
// (cluster-scoped operator) falls back to POD_NAMESPACE for display.
func namespaceForUI(watchNs string) string {
	if watchNs != "" {
		return watchNs
	}
	if podNs := config.GetValue("POD_NAMESPACE"); podNs != "" {
		return podNs
	}
	return "default"
}

// validateJSON returns a config validator that strictly decodes the value
// into the type newT produces (unknown fields rejected). Empty is allowed —
// every JSON knob is optional.
func validateJSON(newT func() any) func(string) error {
	return func(v string) error {
		if strings.TrimSpace(v) == "" {
			return nil
		}
		dec := json.NewDecoder(strings.NewReader(v))
		dec.DisallowUnknownFields()
		if err := dec.Decode(newT()); err != nil {
			return fmt.Errorf("must be JSON: %w", err)
		}
		return nil
	}
}

// jsonEnv decodes an already-validated JSON config value; empty → zero value.
func jsonEnv[T any](key string) T {
	var out T
	v := strings.TrimSpace(config.GetValue(key))
	if v == "" {
		return out
	}
	assert.NoError(json.Unmarshal([]byte(v), &out), "decode "+key)
	return out
}

// jsonEnvPtr is jsonEnv for pointer-typed fields (nil when unset).
func jsonEnvPtr[T any](key string) *T {
	v := strings.TrimSpace(config.GetValue(key))
	if v == "" {
		return nil
	}
	out := new(T)
	assert.NoError(json.Unmarshal([]byte(v), out), "decode "+key)
	return out
}

// parsePullSecrets turns a comma-separated list of Secret names into the
// LocalObjectReference slice the worker pod spec needs. Blank entries are
// skipped; empty input yields nil (no pull secrets).
func parsePullSecrets(csv string) []corev1.LocalObjectReference {
	var out []corev1.LocalObjectReference
	for _, name := range strings.Split(csv, ",") {
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, corev1.LocalObjectReference{Name: name})
		}
	}
	return out
}

func buildWorkerResources() corev1.ResourceRequirements {
	reqs := corev1.ResourceRequirements{}
	if v := config.GetValue("WORKER_CPU_LIMIT"); v != "" {
		if q, err := resource.ParseQuantity(v); err == nil {
			if reqs.Limits == nil {
				reqs.Limits = corev1.ResourceList{}
			}
			reqs.Limits[corev1.ResourceCPU] = q
		}
	}
	if v := config.GetValue("WORKER_MEMORY_LIMIT"); v != "" {
		if q, err := resource.ParseQuantity(v); err == nil {
			if reqs.Limits == nil {
				reqs.Limits = corev1.ResourceList{}
			}
			reqs.Limits[corev1.ResourceMemory] = q
		}
	}
	if v := config.GetValue("WORKER_CPU_REQUEST"); v != "" {
		if q, err := resource.ParseQuantity(v); err == nil {
			if reqs.Requests == nil {
				reqs.Requests = corev1.ResourceList{}
			}
			reqs.Requests[corev1.ResourceCPU] = q
		}
	}
	if v := config.GetValue("WORKER_MEMORY_REQUEST"); v != "" {
		if q, err := resource.ParseQuantity(v); err == nil {
			if reqs.Requests == nil {
				reqs.Requests = corev1.ResourceList{}
			}
			reqs.Requests[corev1.ResourceMemory] = q
		}
	}
	return reqs
}

// chainedProvider tries primary first; on error it logs (without leaking
// PROMETHEUS_URL credentials thanks to the provider's own redaction) and
// returns the fallback's result. This keeps the UI up if Prometheus blips.
type chainedProvider struct {
	primary  alerts.Provider
	fallback alerts.Provider
	log      logr.Logger
}

func (c chainedProvider) List(ctx context.Context) ([]alerts.Alert, error) {
	if out, err := c.primary.List(ctx); err == nil {
		return out, nil
	} else {
		c.log.V(1).Info("primary alerts source failed, falling back to local", "err", err.Error())
	}
	return c.fallback.List(ctx)
}
