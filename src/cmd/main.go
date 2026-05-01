// Operator — reconciles labeled source Secrets to managed K8s CronJobs.
// It does not run backups itself; CronJob-spawned Job pods do, executing
// the worker binary (cmd/worker).
package main

import (
	"flag"
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"backup-operator/assert"
	"backup-operator/config"
	"backup-operator/controllers"
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
		{Key: "DEFAULT_SCHEDULE", Optional: true, Default: "0 2 * * *"},
		{
			Key:      "RUN_TIMEOUT_SECONDS",
			Optional: true,
			Default:  "3600",
			Validate: func(v string) error {
				if _, err := strconv.Atoi(v); err != nil {
					return fmt.Errorf("'RUN_TIMEOUT_SECONDS' must be integer: %w", err)
				}
				return nil
			},
		},
		{Key: "TEMP_DIR", Optional: true, Default: "/tmp/backup-operator"},
		{Key: "TEMP_DIR_SIZE", Optional: true, Default: "10Gi"},
		{Key: "DEFAULT_RETENTION_DAYS", Optional: true, Default: "30"},
		{Key: "DEFAULT_MIN_KEEP", Optional: true, Default: "3"},
		{Key: "METRICS_REFRESH_INTERVAL_SECONDS", Optional: true, Default: "30", Validate: func(v string) error {
			n, err := strconv.Atoi(v)
			if err != nil {
				return fmt.Errorf("'METRICS_REFRESH_INTERVAL_SECONDS' must be integer: %w", err)
			}
			if n < 5 {
				return fmt.Errorf("'METRICS_REFRESH_INTERVAL_SECONDS' must be >= 5")
			}
			return nil
		}},

		// Worker pod template — these flow into every CronJob the reconciler
		// produces. Set by Helm; required so CronJobs are runnable.
		{Key: "WORKER_IMAGE", Optional: false},
		{Key: "WORKER_IMAGE_PULL_POLICY", Optional: true, Default: "IfNotPresent"},
		{Key: "WORKER_SERVICE_ACCOUNT", Optional: false},
		{Key: "AGE_SECRET_NAME", Optional: false},

		// UI dashboard — read-only timeline of run history. Disabled by
		// default to keep the operator's surface minimal; flip UI_ENABLED
		// to expose it on UI_ADDR.
		{Key: "UI_ENABLED", Optional: true, Default: "false"},
		{Key: "UI_ADDR", Optional: true, Default: ":8081"},
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
	mgrOpts := ctrl.Options{
		LeaderElection:                leaderElectionID != "",
		LeaderElectionID:              leaderElectionID,
		LeaderElectionNamespace:       config.GetValue("POD_NAMESPACE"),
		LeaderElectionReleaseOnCancel: true,
	}
	if watchNs != "" {
		mgrOpts.Cache = cache.Options{DefaultNamespaces: map[string]cache.Config{watchNs: {}}}
	}

	mgr, err := ctrl.NewManager(cfg, mgrOpts)
	assert.NoError(err, "failed to create controller manager")

	runTimeoutSec, _ := strconv.Atoi(config.GetValue("RUN_TIMEOUT_SECONDS"))

	worker := controllers.WorkerSpec{
		Image:              config.GetValue("WORKER_IMAGE"),
		ImagePullPolicy:    corev1.PullPolicy(config.GetValue("WORKER_IMAGE_PULL_POLICY")),
		ServiceAccountName: config.GetValue("WORKER_SERVICE_ACCOUNT"),
		AgeSecretName:      config.GetValue("AGE_SECRET_NAME"),
		TempDir:            config.GetValue("TEMP_DIR"),
		TempDirSize:        config.GetValue("TEMP_DIR_SIZE"),
		RunTimeoutSeconds:  int64(runTimeoutSec),
		RetentionDaysDef:   config.GetValue("DEFAULT_RETENTION_DAYS"),
		MinKeepDef:         config.GetValue("DEFAULT_MIN_KEEP"),
		DefaultSchedule:    config.GetValue("DEFAULT_SCHEDULE"),
	}

	reconciler := &controllers.CronJobReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Logger: ctrl.Log.WithName("cronjob-reconciler"),
		Worker: worker,
	}
	err = reconciler.SetupWithManager(mgr)
	assert.NoError(err, "failed to setup cronjob reconciler")

	refreshSec, _ := strconv.Atoi(config.GetValue("METRICS_REFRESH_INTERVAL_SECONDS"))
	refresher := &controllers.MetricsRefresher{
		Client:    mgr.GetClient(),
		Logger:    ctrl.Log.WithName("metrics-refresher"),
		Namespace: watchNs,
		Interval:  time.Duration(refreshSec) * time.Second,
	}
	assert.NoError(mgr.Add(refresher), "failed to register metrics refresher")

	if config.GetValue("UI_ENABLED") == "true" {
		uiServer, err := ui.New(ui.Config{
			Addr:      config.GetValue("UI_ADDR"),
			Namespace: namespaceForUI(watchNs),
			Client:    mgr.GetClient(),
			Logger:    ctrl.Log.WithName("ui"),
		})
		assert.NoError(err, "failed to construct UI server")
		// Register before manager start so the cache and HTTP listener share
		// the manager's context (and shut down with it).
		assert.NoError(mgr.Add(uiServer), "failed to register UI server")
	}

	ctx := ctrl.SetupSignalHandler()
	if err := mgr.Start(ctx); err != nil {
		assert.NoError(err, "manager exited with error")
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
