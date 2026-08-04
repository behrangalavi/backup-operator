// Restore CLI — runs OUTSIDE the cluster on an operator's machine.
//
//	backup-restore --storage-secret hetzner-sb --namespace backup \
//	               --target prod-users --age-key ~/age.key > dump.sql.gz
//
// The age private key is read from a local file the operator controls; the
// service running in the cluster has no access to it. The restore tool:
//   1. Loads the storage destination Secret from the cluster (kubeconfig).
//   2. Lists or downloads the requested artifact for the target.
//   3. Streams: download → age decrypt → (optional gunzip) → stdout/file.
//
// The output stream matches what was dumped: a gzip-compressed dump (Postgres/
// MySQL SQL or Mongo --archive). Pipe to gunzip + the matching restore tool.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"backup-operator/crypto"
	"backup-operator/internal/labels"
	"backup-operator/internal/meta"
	"backup-operator/internal/secrets"
	"backup-operator/storage"
	storageFactory "backup-operator/storage/factory"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// knownDumpSuffixes lists every encrypted-dump file extension the restore
// CLI recognises. Order matters: newer suffix first so pickDumps matches
// zstd-compressed dumps before falling back to gzip.
var knownDumpSuffixes = []string{
	"." + labels.DumpSuffix(labels.CompressionZstd),
	"." + labels.DumpSuffix(labels.CompressionGzip),
}

type dumpEntry struct {
	timestamp string
	path      string
	size      int64
}

func main() {
	var (
		namespace     = flag.String("namespace", "default", "namespace of the storage Secret")
		storageSecret = flag.String("storage-secret", "", "name of the destination Secret to read from")
		target        = flag.String("target", "", "logical name of the backup target")
		ageKeyFile    = flag.String("age-key", "", "path to the age private key file (required for download)")
		timestamp     = flag.String("timestamp", "", "specific dump timestamp (e.g. 20260429T020000Z); empty = latest")
		listOnly      = flag.Bool("list", false, "list available dumps for the target instead of downloading")
		output        = flag.String("o", "-", "output file; '-' = stdout")
		decompress    = flag.Bool("decompress", false, "decompress the decrypted stream before writing to output")
		compression   = flag.String("compression", "", "compression algorithm (gzip or zstd); auto-detected from meta.json when omitted")
		purge         = flag.Bool("purge", false, "delete all dumps for the target (use --before to limit)")
		before        = flag.String("before", "", "only purge dumps older than this date (YYYY-MM-DD or 20060102T150405Z)")
		dryRun        = flag.Bool("dry-run", false, "show what --purge would delete without actually deleting")
	)
	flag.Parse()

	if *storageSecret == "" || *target == "" {
		die("flags --storage-secret and --target are required")
	}
	if !*listOnly && !*purge && *ageKeyFile == "" {
		die("flag --age-key is required (omit only with --list or --purge)")
	}

	log := newStderrLogger()
	ctx := context.Background()

	cs, err := loadClient()
	if err != nil {
		die("kubernetes client: %v", err)
	}

	sec, err := cs.CoreV1().Secrets(*namespace).Get(ctx, *storageSecret, metav1.GetOptions{})
	if err != nil {
		die("get secret %s/%s: %v", *namespace, *storageSecret, err)
	}
	dest, err := secrets.ParseDestination(sec)
	if err != nil {
		die("parse destination: %v", err)
	}

	st, err := storageFactory.NewStorage(dest.StorageType, dest.Name, dest.Data, log)
	if err != nil {
		die("init storage: %v", err)
	}

	objs, err := st.List(ctx, *target+"/")
	if err != nil {
		die("list %s/: %v", *target, err)
	}

	// Purge operates on ALL objects, not just dumps — a right-to-erasure must
	// still remove the unencrypted meta.json sidecars (schema fingerprints,
	// table names, row counts) even after every dump has already been pruned.
	// So handle it before the dumps==0 guard, which only makes sense for the
	// download/list paths.
	if *purge {
		if failed := runPurge(ctx, st, objs, *target, *before, *dryRun, log); failed > 0 {
			os.Exit(1)
		}
		return
	}

	dumps := pickDumps(objs)
	if len(dumps) == 0 {
		die("no dumps found for target %q", *target)
	}
	sort.Slice(dumps, func(i, j int) bool { return dumps[i].timestamp > dumps[j].timestamp })

	if *listOnly {
		_, _ = fmt.Fprintf(os.Stdout, "%-20s\t%-10s\t%s\n", "TIMESTAMP", "SIZE", "PATH")
		for _, d := range dumps {
			_, _ = fmt.Fprintf(os.Stdout, "%-20s\t%-10d\t%s\n", d.timestamp, d.size, d.path)
		}
		return
	}

	pick := dumps[0]
	if *timestamp != "" {
		pick = dumpEntry{}
		for _, d := range dumps {
			if d.timestamp == *timestamp {
				pick = d
				break
			}
		}
		if pick.path == "" {
			die("no dump with timestamp %q (use --list to see what's available)", *timestamp)
		}
	}
	log.Info("downloading", "path", pick.path, "timestamp", pick.timestamp, "size", pick.size)

	keyBytes, err := os.ReadFile(*ageKeyFile)
	if err != nil {
		die("read age key %s: %v", *ageKeyFile, err)
	}
	dec, err := crypto.NewDecryptorFromKeys(string(keyBytes))
	if err != nil {
		die("init decryptor: %v", err)
	}

	rc, err := st.Get(ctx, pick.path)
	if err != nil {
		die("get %s: %v", pick.path, err)
	}
	defer func() { _ = rc.Close() }()

	plain, err := dec.Wrap(rc)
	if err != nil {
		die("age decrypt: %v", err)
	}

	out, closer, err := openOutput(*output)
	if err != nil {
		die("open output: %v", err)
	}

	srcReader := plain
	if *decompress {
		algo := *compression
		if algo == "" {
			algo = detectCompression(ctx, st, pick.path, log)
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
		die("copy: %v", err)
	}
	// Check the output Close error explicitly rather than deferring-and-ignoring:
	// a flush/close failure (NFS, quota) on the restored file must not exit 0
	// leaving a truncated dump — this is a recovery tool.
	if err := closer(); err != nil {
		die("close output %s: %v", *output, err)
	}
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
		for _, sfx := range knownDumpSuffixes {
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
// out-of-cluster CLI on the operator's machine.
func loadClient() (*kubernetes.Clientset, error) {
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(cfg)
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

// runPurge deletes all artifacts (dump + meta) for a target, optionally
// filtered by --before. Supports DSGVO Art. 17 right-to-erasure workflows.
// Returns the number of deletions that failed so the caller can exit non-zero:
// a right-to-erasure that silently reports success while leaving data behind
// is a compliance failure, not a warning.
func runPurge(ctx context.Context, st storage.Storage, objs []storage.Object, target, before string, dryRun bool, log logr.Logger) int {
	var cutoff time.Time
	if before != "" {
		var err error
		cutoff, err = parseCutoff(before)
		if err != nil {
			die("invalid --before value %q: %v (expected YYYY-MM-DD or 20060102T150405Z)", before, err)
		}
	}

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
		fmt.Fprintln(os.Stderr, "no matching artifacts to purge")
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
				log.Error(err, "delete failed", "path", o.Path)
				failed++
				continue
			}
			deleted++
		}
	}
	if dryRun {
		fmt.Fprintf(os.Stderr, "\ndry-run: %d artifact(s) would be deleted\n", len(toDelete))
		return 0
	}
	if failed > 0 {
		// Loud, non-zero: the caller must not believe the erasure completed.
		fmt.Fprintf(os.Stderr, "\npurged %d artifact(s) for target %q, %d FAILED — data remains in storage\n", deleted, target, failed)
	} else {
		fmt.Fprintf(os.Stderr, "\npurged %d artifact(s) for target %q\n", deleted, target)
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

// detectCompression reads the meta.json sidecar next to the dump and returns
// the compression field. Falls back to "gzip" when meta is absent or unparseable.
func detectCompression(ctx context.Context, st storage.Storage, dumpPath string, log logr.Logger) string {
	var metaPath string
	for _, sfx := range knownDumpSuffixes {
		if strings.HasSuffix(dumpPath, sfx) {
			metaPath = strings.TrimSuffix(dumpPath, sfx) + ".meta.json"
			break
		}
	}
	if metaPath == "" {
		log.V(1).Info("cannot derive meta path from dump, defaulting to gzip")
		return "gzip"
	}
	rc, err := st.Get(ctx, metaPath)
	if err != nil {
		log.V(1).Info("meta.json not found, defaulting to gzip", "path", metaPath, "err", err)
		return "gzip"
	}
	defer func() { _ = rc.Close() }()
	var m meta.MetaFile
	if err := json.NewDecoder(rc).Decode(&m); err != nil {
		log.V(1).Info("meta.json decode failed, defaulting to gzip", "err", err)
		return "gzip"
	}
	if m.Compression == "" {
		return "gzip"
	}
	log.Info("detected compression from meta.json", "compression", m.Compression)
	return m.Compression
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
