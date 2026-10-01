package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"backup-operator/internal/migrate"
)

// runMigrate implements `backup-restore migrate`: it reads every legacy
// labelled Secret in a namespace, builds BackupSource / BackupDestination /
// BackupRecipient objects plus credentials-only Secrets, and either prints
// the manifests (default — for review or a GitOps commit) or applies them.
//
// Legacy Secrets are kept unless --delete-legacy is given: they hold the
// only copy of the passwords until the new Secrets exist, and deleting
// user data should be an explicit decision.
func runMigrate(args []string) int {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	var (
		namespace    = fs.String("namespace", "default", "namespace holding the legacy labelled Secrets")
		apply        = fs.Bool("apply", false, "create the objects in the cluster instead of printing them")
		deleteLegacy = fs.Bool("delete-legacy", false, "with --apply: delete the legacy labelled Secrets after the new objects exist")
		mergedName   = fs.String("age-secret", "", "name of the operator-managed merged age Secret (AGE_SECRET_NAME), split into BackupRecipients when no per-recipient Secrets exist")
	)
	fs.StringVar(namespace, "n", "default", "alias for --namespace")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: backup-restore migrate --namespace <ns> [--age-secret <name>] [--apply [--delete-legacy]]\n\n")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	if *deleteLegacy && !*apply {
		die("--delete-legacy requires --apply")
	}

	ctx := context.Background()
	cl, err := loadClient()
	if err != nil {
		die("kubernetes client: %v", err)
	}

	var list corev1.SecretList
	if err := cl.List(ctx, &list, client.InNamespace(*namespace)); err != nil {
		die("list secrets in %s: %v", *namespace, err)
	}
	plan := migrate.BuildPlan(list.Items, migrate.Options{MergedSecretName: *mergedName})

	for _, w := range plan.Warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	fmt.Fprintf(os.Stderr, "plan: %d source(s), %d destination(s), %d recipient(s), %d credentials secret(s)\n",
		len(plan.Sources), len(plan.Destinations), len(plan.Recipients), len(plan.Secrets))
	if len(plan.Sources)+len(plan.Destinations)+len(plan.Recipients) == 0 {
		fmt.Fprintln(os.Stderr, "nothing to migrate: no Secrets labelled backup.mogenius.io/role found")
		return 0
	}

	if !*apply {
		if err := migrate.WriteYAML(os.Stdout, plan); err != nil {
			die("write manifests: %v", err)
		}
		fmt.Fprintln(os.Stderr, "\nreview the manifests above (they contain plaintext credentials), then re-run with --apply or commit them to your GitOps repo")
		return 0
	}

	notes, err := migrate.Apply(ctx, cl, plan, *deleteLegacy)
	for _, n := range notes {
		fmt.Fprintln(os.Stderr, n)
	}
	if err != nil {
		die("apply: %v", err)
	}
	fmt.Fprintln(os.Stderr, "migration applied")
	if !*deleteLegacy {
		fmt.Fprintln(os.Stderr, "legacy Secrets were kept; re-run with --apply --delete-legacy (or kubectl delete) once the operator shows the new objects as Ready:")
		for _, n := range plan.Legacy {
			fmt.Fprintf(os.Stderr, "  kubectl -n %s delete secret %s\n", *namespace, n)
		}
	}
	return 0
}
