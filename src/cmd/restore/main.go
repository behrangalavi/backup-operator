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
	"compress/gzip"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"

	"backup-operator/crypto"
	"backup-operator/internal/secrets"
	"backup-operator/storage"
	storageFactory "backup-operator/storage/factory"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const dumpSuffix = ".sql.gz.age"

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
		decompress    = flag.Bool("decompress", false, "gunzip the decrypted stream before writing to output")
	)
	flag.Parse()

	if *storageSecret == "" || *target == "" {
		die("flags --storage-secret and --target are required")
	}
	if !*listOnly && *ageKeyFile == "" {
		die("flag --age-key is required (omit only with --list)")
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
	defer closer()

	srcReader := plain
	if *decompress {
		gz, err := gzip.NewReader(plain)
		if err != nil {
			die("gunzip: %v", err)
		}
		defer func() { _ = gz.Close() }()
		srcReader = gz
	}

	if _, err := io.Copy(out, srcReader); err != nil {
		die("copy: %v", err)
	}
}

// pickDumps filters listed objects down to dump artifacts and parses their
// timestamps from the filename. Meta JSON files and any unexpected entries
// are silently skipped — they're not what the user asked for.
func pickDumps(objs []storage.Object) []dumpEntry {
	out := make([]dumpEntry, 0, len(objs))
	for _, o := range objs {
		base := path.Base(o.Path)
		if !strings.HasPrefix(base, "dump-") || !strings.HasSuffix(base, dumpSuffix) {
			continue
		}
		ts := strings.TrimSuffix(strings.TrimPrefix(base, "dump-"), dumpSuffix)
		if ts == "" {
			continue
		}
		out = append(out, dumpEntry{timestamp: ts, path: o.Path, size: o.Size})
	}
	return out
}

// openOutput returns a writer plus a closer; "-" maps to stdout with a no-op closer.
func openOutput(target string) (io.Writer, func(), error) {
	if target == "-" {
		return os.Stdout, func() {}, nil
	}
	f, err := os.Create(target)
	if err != nil {
		return nil, nil, err
	}
	return f, func() { _ = f.Close() }, nil
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
