// Restore CLI — runs OUTSIDE the cluster on an operator's machine.
//
//	backup-restore --destination hetzner-sb --namespace backup \
//	               --target prod-users --age-key ~/age.key > dump.sql.gz
//
// The age private key is read from a local file the operator controls; the
// service running in the cluster has no access to it. The restore tool:
//  1. Loads one or more BackupDestinations (repeat --destination or pass a
//     comma-separated list) and their credentials Secrets from the cluster
//     (kubeconfig).
//  2. Lists the target's dumps across every given destination, merged by
//     timestamp, and tells you which destination it picks.
//  3. Streams: download → age decrypt → (optional decompress) → stdout/file,
//     verifying the ciphertext SHA256 against meta.json when a sidecar exists.
//
// --purge deletes from EVERY given destination — a right-to-erasure that
// only cleaned one copy would not be an erasure.
//
// `backup-restore migrate` converts a namespace from the legacy labelled-
// Secret discovery model to BackupSource / BackupDestination /
// BackupRecipient objects — see migrate.go.
//
// The output stream matches what was dumped: a gzip-compressed dump (Postgres/
// MySQL SQL or Mongo --archive). Pipe to gunzip + the matching restore tool.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"backup-operator/api/v1alpha1"
	"backup-operator/crypto"
	"backup-operator/internal/labels"
	"backup-operator/internal/meta"
	"backup-operator/internal/secrets"
	"backup-operator/storage"
	storageFactory "backup-operator/storage/factory"

	"filippo.io/age"
	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// dumpEntry is one backup run as seen across every destination that holds
// it. Object paths are identical on every destination (the storage layer
// strips each destination's pathPrefix), so one path serves all of them.
type dumpEntry struct {
	timestamp string
	path      string
	size      int64
	// dests lists the destinations (in --destination order) that hold this
	// run. Download tries them in order; --list shows them.
	dests []string
}

// destStorage pairs a destination's logical name with its storage client.
type destStorage struct {
	name string
	st   storage.Storage
}

// destList collects repeated / comma-separated --destination values.
type destList []string

func (d *destList) String() string { return strings.Join(*d, ",") }
func (d *destList) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			*d = append(*d, part)
		}
	}
	return nil
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		os.Exit(runMigrate(os.Args[2:]))
	}
	var (
		namespace    = flag.String("namespace", "default", "namespace of the BackupDestination(s)")
		destinations destList
		target       = flag.String("target", "", "logical name of the backup target")
		ageKeyFile   = flag.String("age-key", "", "path to the age private key file (required for download)")
		timestamp    = flag.String("timestamp", "", "specific dump timestamp (e.g. 20260429T020000Z); empty = latest")
		listOnly     = flag.Bool("list", false, "list available dumps for the target instead of downloading")
		output       = flag.String("o", "-", "output file; '-' = stdout")
		decompress   = flag.Bool("decompress", false, "decompress the decrypted stream before writing to output")
		compression  = flag.String("compression", "", "compression algorithm (gzip or zstd); overrides auto-detection from the dump's file suffix / meta.json")
		purge        = flag.Bool("purge", false, "delete all dumps for the target from EVERY given destination (use --before to limit)")
		before       = flag.String("before", "", "only purge dumps older than this date (YYYY-MM-DD or 20060102T150405Z)")
		dryRun       = flag.Bool("dry-run", false, "show what --purge would delete without actually deleting")
	)
	flag.Var(&destinations, "destination", "name of a BackupDestination to read from; repeat or comma-separate to merge several (default: latest run across all of them)")
	// -n mirrors kubectl; both names write the same variable.
	flag.StringVar(namespace, "n", "default", "alias for --namespace")
	flag.Parse()

	if len(destinations) == 0 || *target == "" {
		die("flags --destination and --target are required")
	}
	if !*listOnly && !*purge && *ageKeyFile == "" {
		die("flag --age-key is required (omit only with --list or --purge)")
	}
	var cutoff time.Time
	if *before != "" {
		var err error
		if cutoff, err = parseCutoff(*before); err != nil {
			die("invalid --before value %q: %v (expected YYYY-MM-DD or 20060102T150405Z)", *before, err)
		}
	}

	log := newStderrLogger()
	ctx := context.Background()

	cl, err := loadClient()
	if err != nil {
		die("kubernetes client: %v", err)
	}

	stores := openStores(ctx, cl, *namespace, destinations, log)

	// Purge operates on ALL objects, not just dumps — a right-to-erasure must
	// still remove the unencrypted meta.json sidecars (schema fingerprints,
	// table names, row counts) even after every dump has already been pruned.
	// It runs against every destination given: a copy left on a second
	// destination would make the erasure a lie.
	if *purge {
		if failed := purgeAll(ctx, stores, *target, cutoff, *dryRun, log); failed > 0 {
			os.Exit(1)
		}
		return
	}

	dumps := listAcrossDestinations(ctx, stores, *target, log)

	if *listOnly {
		_, _ = fmt.Fprintf(os.Stdout, "%-20s\t%-10s\t%-24s\t%s\n", "TIMESTAMP", "SIZE", "DESTINATIONS", "PATH")
		for _, d := range dumps {
			_, _ = fmt.Fprintf(os.Stdout, "%-20s\t%-10d\t%-24s\t%s\n", d.timestamp, d.size, strings.Join(d.dests, ","), d.path)
		}
		return
	}

	pick := selectRun(dumps, *timestamp, len(stores) > 1)
	restoreRun(ctx, stores, pick, restoreOptions{
		ageKeyFile: *ageKeyFile, output: *output, decompress: *decompress, compression: *compression,
	}, log)
}

// restoreOptions carries the flags the download/decrypt/write phase reads.
type restoreOptions struct {
	ageKeyFile  string
	output      string
	decompress  bool
	compression string
}

// openStores resolves every --destination into a ready storage client.
// Dying on the first failure is deliberate: a restore (or a purge) that
// silently skips a destination it was asked to use is worse than no restore.
func openStores(ctx context.Context, cl client.Client, namespace string, names []string, log logr.Logger) []destStorage {
	stores := make([]destStorage, 0, len(names))
	for _, name := range names {
		dest, err := loadDestination(ctx, cl, namespace, name)
		if err != nil {
			die("%v", err)
		}
		st, err := storageFactory.NewStorage(dest.StorageType, dest.Name, dest.Data, log)
		if err != nil {
			die("init storage %s: %v", name, err)
		}
		stores = append(stores, destStorage{name: dest.Name, st: st})
	}

	return stores
}

// listAcrossDestinations lists the target prefix on every destination,
// skipping (with a log line) the ones that cannot be listed, and merges the
// runs by timestamp. Dies when no destination could be listed or none holds a
// dump.
func listAcrossDestinations(ctx context.Context, stores []destStorage, target string, log logr.Logger) []dumpEntry {
	listings := make(map[string][]storage.Object, len(stores))
	listed := 0
	for _, ds := range stores {
		objs, err := ds.st.List(ctx, target+"/")
		if err != nil {
			log.Error(err, "list failed; destination skipped", "destination", ds.name)
			continue
		}
		listings[ds.name] = objs
		listed++
	}
	if listed == 0 {
		die("could not list %s/ on any destination", target)
	}
	dumps := mergeDumps(destNames(stores), listings)
	if len(dumps) == 0 {
		die("no dumps found for target %q on %s", target, strings.Join(destNames(stores), ", "))
	}

	return dumps
}

// selectRun picks the requested timestamp or the newest run. With several
// destinations the chosen run and each destination's newest run are printed:
// destinations can legitimately diverge (one offline during a run) and the
// operator must see which run they are about to restore.
func selectRun(dumps []dumpEntry, timestamp string, multi bool) dumpEntry {
	pick := dumps[0]
	if timestamp != "" {
		pick = dumpEntry{}
		for _, d := range dumps {
			if d.timestamp == timestamp {
				pick = d
				break
			}
		}
		if pick.path == "" {
			die("no dump with timestamp %q (use --list to see what's available)", timestamp)
		}
	}
	if timestamp == "" && multi {
		// Destinations can legitimately diverge (one offline during a run); the
		// operator must see which run they are about to restore.
		fmt.Fprintf(os.Stderr, "latest run %s found on %s (newest run per destination: %s)\n",
			pick.timestamp, strings.Join(pick.dests, ","), newestPerDest(dumps, destNamesOf(dumps)))
	}
	return pick
}

// restoreRun downloads, decrypts, optionally decompresses and writes the
// chosen run, verifying the ciphertext against meta.json's sha256.
func restoreRun(ctx context.Context, stores []destStorage, pick dumpEntry, opts restoreOptions, log logr.Logger) {
	src, err := openDump(ctx, stores, pick)
	if err != nil {
		die("%v", err)
	}
	fmt.Fprintf(os.Stderr, ">>> restoring timestamp %s from destination %s (%s, %d bytes)\n", pick.timestamp, src.name, pick.path, pick.size)
	defer func() { _ = src.rc.Close() }()

	keyBytes, err := os.ReadFile(opts.ageKeyFile)
	if err != nil {
		die("read age key %s: %v", opts.ageKeyFile, err)
	}
	dec, err := crypto.NewDecryptorFromKeys(string(keyBytes))
	if err != nil {
		die("init decryptor: %v\n  hint: --age-key must point at the file age-keygen wrote (AGE-SECRET-KEY-1... line); the age1... public key cannot decrypt", err)
	}

	// Hash the ciphertext as it streams past so the download can be checked
	// against meta.json's sha256 without a second pass over the object.
	sidecar := loadMeta(ctx, src.st, pick.path, log)
	hasher := sha256.New()
	cipherReader := io.TeeReader(src.rc, hasher)

	plain, err := dec.Wrap(cipherReader)
	if err != nil {
		die("age decrypt: %v%s", err, decryptHint(err))
	}

	out, closer, err := openOutput(opts.output)
	if err != nil {
		die("open output: %v", err)
	}

	srcReader := plain
	if opts.decompress {
		algo := opts.compression
		if algo == "" {
			algo = detectCompression(pick.path, sidecar, log)
		}
		dc, err := meta.NewDecompressor(plain, algo)
		if err != nil {
			_ = closer()
			die("decompress (%s): %v", algo, err)
		}
		defer func() { _ = dc.Close() }()
		srcReader = dc
	}

	if _, err := io.Copy(out, srcReader); err != nil {
		_ = closer()
		removePartial(opts.output)
		die("copy: %v%s", err, decryptHint(err))
	}
	// Check the output Close error explicitly rather than deferring-and-ignoring:
	// a flush/close failure (NFS, quota) on the restored file must not exit 0
	// leaving a truncated dump — this is a recovery tool.
	if err := closer(); err != nil {
		removePartial(opts.output)
		die("close output %s: %v", opts.output, err)
	}
	// The whole ciphertext has streamed through the hasher by now (age reads
	// to EOF for its final MAC). A mismatch means the stored object is not
	// the one the worker wrote — bit rot, a tampered bucket, or a truncated
	// upload — and what we just produced must not pass for a restore.
	if sidecar != nil && sidecar.SHA256 != "" {
		if err := verifySHA256(sidecar.SHA256, hasher); err != nil {
			removePartial(opts.output)
			die("%v (output removed; try --destination <other> or a different --timestamp)", err)
		}
		log.Info("ciphertext sha256 verified against meta.json", "sha256", sidecar.SHA256)
	} else {
		log.Info("no sha256 in meta.json for this run; download integrity not verified")
	}
}

// destNamesOf returns every destination name that appears in the merged run
// list (the ones that were listable), in first-seen order.
func destNamesOf(dumps []dumpEntry) []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range dumps {
		for _, n := range d.dests {
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	return out
}

// openedDump is a dump stream together with the destination it came from.
type openedDump struct {
	name string
	st   storage.Storage
	rc   io.ReadCloser
}

// openDump fetches the dump from the first destination holding it, falling
// through to the next on a Get error so one unreachable copy does not block
// a restore that another destination can serve.
func openDump(ctx context.Context, stores []destStorage, pick dumpEntry) (*openedDump, error) {
	byName := make(map[string]storage.Storage, len(stores))
	for _, ds := range stores {
		byName[ds.name] = ds.st
	}
	var lastErr error
	for _, name := range pick.dests {
		st := byName[name]
		rc, err := st.Get(ctx, pick.path)
		if err != nil {
			lastErr = fmt.Errorf("get %s from %s: %w", pick.path, name, err)
			fmt.Fprintf(os.Stderr, "warning: %v\n", lastErr)
			continue
		}
		return &openedDump{name: name, st: st, rc: rc}, nil
	}
	return nil, fmt.Errorf("dump %s unavailable on every destination holding it: %w", pick.path, lastErr)
}

// mergeDumps indexes the dumps of every destination by timestamp, newest
// first. The same run on several destinations collapses into one entry that
// remembers where it lives (destination order follows `order`).
func mergeDumps(order []string, listings map[string][]storage.Object) []dumpEntry {
	byTS := make(map[string]*dumpEntry)
	for _, name := range order {
		for _, d := range pickDumps(listings[name]) {
			e, ok := byTS[d.timestamp]
			if !ok {
				d := d
				e = &d
				byTS[d.timestamp] = e
			}
			e.dests = append(e.dests, name)
		}
	}
	out := make([]dumpEntry, 0, len(byTS))
	for _, e := range byTS {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].timestamp > out[j].timestamp })
	return out
}

// newestPerDest renders "dest=timestamp" pairs so a diverged destination is
// obvious when the latest run is not present everywhere.
func newestPerDest(dumps []dumpEntry, order []string) string {
	newest := make(map[string]string, len(order))
	for _, d := range dumps { // newest first
		for _, n := range d.dests {
			if _, ok := newest[n]; !ok {
				newest[n] = d.timestamp
			}
		}
	}
	parts := make([]string, 0, len(order))
	for _, n := range order {
		ts := newest[n]
		if ts == "" {
			ts = "none"
		}
		parts = append(parts, n+"="+ts)
	}
	return strings.Join(parts, " ")
}

func destNames(stores []destStorage) []string {
	out := make([]string, len(stores))
	for i, ds := range stores {
		out[i] = ds.name
	}
	return out
}

// verifySHA256 compares the hex digest recorded in meta.json with the hash
// of the bytes that were actually downloaded.
func verifySHA256(expected string, h hash.Hash) error {
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(strings.TrimSpace(expected), got) {
		return fmt.Errorf("sha256 mismatch: meta.json records %s, downloaded object hashes to %s — stored dump is corrupt, tampered or truncated", expected, got)
	}
	return nil
}

// decryptHint turns age's terse errors into something an operator at 3 a.m.
// can act on. The common case by far is the wrong private key.
func decryptHint(err error) string {
	var noMatch *age.NoIdentityMatchError
	if errors.As(err, &noMatch) {
		return "\n  hint: none of the identities in --age-key can open this dump — wrong private key? The cluster encrypts to the recipients listed by `kubectl get backuprecipients`; use the matching key file."
	}
	msg := err.Error()
	if strings.Contains(msg, "parse header") || strings.Contains(msg, "failed to read header") {
		return "\n  hint: the object does not start with an age header — is this really an encrypted dump (or did the download return an error page)?"
	}
	if strings.Contains(msg, "bad header MAC") || strings.Contains(msg, "incorrect") {
		return "\n  hint: the ciphertext is damaged or was modified after upload."
	}
	return ""
}

// removePartial deletes a half-written output file so a failed restore
// cannot leave behind something that looks like a valid dump. Stdout is
// never touched.
func removePartial(target string) {
	if target == "-" || target == "" {
		return
	}
	if err := os.Remove(target); err == nil {
		fmt.Fprintf(os.Stderr, "removed partial output %s\n", target)
	}
}

// loadDestination fetches the BackupDestination and its credentials Secret
// and flattens them for the storage factory.
func loadDestination(ctx context.Context, cl client.Client, namespace, name string) (*secrets.Destination, error) {
	var bd v1alpha1.BackupDestination
	if err := cl.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &bd); err != nil {
		return nil, fmt.Errorf("get backupdestination %s/%s: %w", namespace, name, err)
	}
	var creds corev1.Secret
	credsKey := client.ObjectKey{Namespace: namespace, Name: bd.Spec.CredentialsSecretRef.Name}
	if err := cl.Get(ctx, credsKey, &creds); err != nil {
		return nil, fmt.Errorf("get credentials secret %s/%s: %w", namespace, credsKey.Name, err)
	}
	dest, err := secrets.FromBackupDestination(&bd, &creds)
	if err != nil {
		return nil, fmt.Errorf("flatten destination: %w", err)
	}
	return dest, nil
}

// pickDumps filters listed objects down to dump artifacts and parses their
// timestamps from the filename. Meta JSON files and any unexpected entries
// are silently skipped — they're not what the user asked for.
func pickDumps(objs []storage.Object) []dumpEntry {
	out := make([]dumpEntry, 0, len(objs))
	for _, o := range objs {
		base := path.Base(o.Path)
		if !strings.HasPrefix(base, "dump-") {
			continue
		}
		var ts string
		for _, sfx := range labels.DumpSuffixes() {
			if strings.HasSuffix(base, sfx) {
				ts = strings.TrimSuffix(strings.TrimPrefix(base, "dump-"), sfx)
				break
			}
		}
		if ts == "" {
			continue
		}
		out = append(out, dumpEntry{timestamp: ts, path: o.Path, size: o.Size})
	}
	return out
}

// openOutput returns a writer plus a closer that surfaces the Close error;
// "-" maps to stdout with a no-op closer (never close os.Stdout).
func openOutput(target string) (io.Writer, func() error, error) {
	if target == "-" {
		return os.Stdout, func() error { return nil }, nil
	}
	f, err := os.Create(target)
	if err != nil {
		return nil, nil, err
	}
	return f, f.Close, nil
}

// loadClient resolves a kubeconfig the way kubectl does — $KUBECONFIG first,
// then ~/.kube/config — so the restore tool works exactly like every other
// out-of-cluster CLI on the operator's machine. The scheme knows core
// Secrets and the backup.mogenius.io group.
func loadClient() (client.Client, error) {
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err != nil {
		return nil, err
	}
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	return client.New(cfg, client.Options{Scheme: s})
}

func newStderrLogger() logr.Logger {
	return funcr.New(func(prefix, args string) {
		fmt.Fprintln(os.Stderr, prefix, args)
	}, funcr.Options{})
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}

// purgeAll runs the erasure on every destination and sums the failures. A
// destination that cannot even be listed counts as a failure too: "I could
// not see what is there" must not read as "nothing left to delete".
func purgeAll(ctx context.Context, stores []destStorage, target string, cutoff time.Time, dryRun bool, log logr.Logger) int {
	failed := 0
	for _, ds := range stores {
		fmt.Fprintf(os.Stderr, "== destination %s\n", ds.name)
		objs, err := ds.st.List(ctx, target+"/")
		if err != nil {
			log.Error(err, "list failed; nothing purged on this destination", "destination", ds.name)
			failed++
			continue
		}
		failed += runPurge(ctx, ds.st, ds.name, objs, target, cutoff, dryRun, log)
	}
	if failed > 0 {
		fmt.Fprintf(os.Stderr, "\nERASURE INCOMPLETE: %d failure(s) across %d destination(s) — data remains in storage\n", failed, len(stores))
	}
	return failed
}

// runPurge deletes all artifacts (dump + meta) for a target on ONE
// destination, optionally filtered by cutoff. Supports DSGVO Art. 17
// right-to-erasure workflows. Returns the number of deletions that failed so
// the caller can exit non-zero: a right-to-erasure that silently reports
// success while leaving data behind is a compliance failure, not a warning.
func runPurge(ctx context.Context, st storage.Storage, destName string, objs []storage.Object, target string, cutoff time.Time, dryRun bool, log logr.Logger) int {
	var toDelete []storage.Object
	for _, o := range objs {
		ts := extractTimestamp(o.Path)
		if ts.IsZero() {
			continue
		}
		if !cutoff.IsZero() && !ts.Before(cutoff) {
			continue
		}
		toDelete = append(toDelete, o)
	}

	if len(toDelete) == 0 {
		fmt.Fprintf(os.Stderr, "%s: no matching artifacts to purge\n", destName)
		return 0
	}

	verb := "deleting"
	if dryRun {
		verb = "would delete (dry-run)"
	}

	deleted, failed := 0, 0
	for _, o := range toDelete {
		fmt.Fprintf(os.Stderr, "%s: %s (%d bytes)\n", verb, o.Path, o.Size)
		if !dryRun {
			if err := st.Delete(ctx, o.Path); err != nil {
				log.Error(err, "delete failed", "destination", destName, "path", o.Path)
				failed++
				continue
			}
			deleted++
		}
	}
	if dryRun {
		fmt.Fprintf(os.Stderr, "%s: dry-run, %d artifact(s) would be deleted\n", destName, len(toDelete))
		return 0
	}
	if failed > 0 {
		// Loud, non-zero: the caller must not believe the erasure completed.
		fmt.Fprintf(os.Stderr, "%s: purged %d artifact(s) for target %q, %d FAILED — data remains in storage\n", destName, deleted, target, failed)
	} else {
		fmt.Fprintf(os.Stderr, "%s: purged %d artifact(s) for target %q\n", destName, deleted, target)
	}
	return failed
}

func parseCutoff(s string) (time.Time, error) {
	if t, err := time.Parse("20060102T150405Z", s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("unsupported format")
}

// loadMeta fetches and parses the meta.json sidecar next to a dump. nil when
// there is none (legacy run, sidecar lost) or it cannot be parsed — callers
// then fall back to what the dump itself tells them.
func loadMeta(ctx context.Context, st storage.Storage, dumpPath string, log logr.Logger) *meta.MetaFile {
	var metaPath string
	for _, sfx := range labels.DumpSuffixes() {
		if strings.HasSuffix(dumpPath, sfx) {
			metaPath = strings.TrimSuffix(dumpPath, sfx) + ".meta.json"
			break
		}
	}
	if metaPath == "" {
		return nil
	}
	rc, err := st.Get(ctx, metaPath)
	if err != nil {
		log.V(1).Info("meta.json not found", "path", metaPath, "err", err.Error())
		return nil
	}
	defer func() { _ = rc.Close() }()
	var m meta.MetaFile
	if err := json.NewDecoder(rc).Decode(&m); err != nil {
		log.V(1).Info("meta.json decode failed", "path", metaPath, "err", err.Error())
		return nil
	}
	return &m
}

// detectCompression picks the decompressor. Precedence: the dump's file
// suffix (authoritative — the pipeline named the file after the algorithm it
// used, and the suffix survives a lost sidecar), then meta.json's
// compression field (legacy layouts), then gzip. An explicit --compression
// flag bypasses this entirely.
func detectCompression(dumpPath string, sidecar *meta.MetaFile, log logr.Logger) string {
	if algo, ok := labels.CompressionForPath(dumpPath); ok {
		log.V(1).Info("compression from dump suffix", "compression", algo)
		return algo
	}
	if sidecar != nil && sidecar.Compression != "" {
		log.Info("compression from meta.json (dump suffix unknown)", "compression", sidecar.Compression)
		return sidecar.Compression
	}
	log.Info("compression unknown, defaulting to gzip")
	return labels.CompressionGzip
}

func extractTimestamp(p string) time.Time {
	base := path.Base(p)
	if !strings.HasPrefix(base, "dump-") {
		return time.Time{}
	}
	// dump-20260429T020000Z.sql.gz.age or dump-20260429T020000Z.meta.json
	ts := base[len("dump-"):]
	if idx := strings.IndexByte(ts, '.'); idx > 0 {
		ts = ts[:idx]
	}
	t, _ := time.Parse("20060102T150405Z", ts)
	return t
}
