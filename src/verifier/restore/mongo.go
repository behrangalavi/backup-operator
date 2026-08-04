package restore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"backup-operator/verifier/ephemeral"

	corev1 "k8s.io/api/core/v1"
)

const (
	mongoVerifierUser = "root"
	mongoVerifierDB   = "admin"
)

type mongoEngine struct {
	// password is the per-run root credential for the ephemeral pod.
	password string
}

func (*mongoEngine) DBType() string { return "mongo" }

func (e *mongoEngine) PodSpec(volumeBytes int64, imageOverride string) ephemeral.Spec {
	image := imageOverride
	if image == "" {
		image = DefaultImage("mongo")
	}
	pw := e.password
	return ephemeral.Spec{
		Image: image,
		Port:  27017,
		EnvVars: []corev1.EnvVar{
			{Name: "MONGO_INITDB_ROOT_USERNAME", Value: mongoVerifierUser},
			{Name: "MONGO_INITDB_ROOT_PASSWORD", Value: pw},
		},
		VolumeMountPath: "/data/db",
		VolumeSizeBytes: volumeBytes,
		ReadyTimeout:    5 * time.Minute,
		RunAsUID:        runAsUIDForImage("mongo", image),
		Probe: func(ctx context.Context, endpoint string) error {
			return probeMongo(ctx, endpoint, pw)
		},
	}
}

func probeMongo(ctx context.Context, endpoint, password string) error {
	uri := fmt.Sprintf("mongodb://%s:%s@%s/?authSource=admin&serverSelectionTimeoutMS=3000",
		mongoVerifierUser, password, endpoint)
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		return err
	}
	defer func() { _ = client.Disconnect(ctx) }()
	return client.Ping(ctx, nil)
}

func (e *mongoEngine) Restore(ctx context.Context, endpoint string, plaintext io.Reader, mode Mode, log logr.Logger) error {
	host, port, err := splitEndpoint(endpoint)
	if err != nil {
		return err
	}

	// Pass the password via a 0600 --config file, never on argv — otherwise it
	// is visible in `ps` / container telemetry / any sidecar with PID-namespace
	// access, breaking the project's "credentials never on command lines" rule
	// (the mongo dumper already does this). The per-run password shortens the
	// exposure but doesn't excuse leaking it.
	configPath, err := writeMongoPasswordConfig(e.password)
	if err != nil {
		return fmt.Errorf("write mongo config: %w", err)
	}
	defer func() { _ = os.Remove(configPath) }()

	// mongorestore reads its archive from stdin via --archive (no path).
	// --gzip is intentionally OMITTED here because the verifier-driver
	// has already gunzipped the stream upstream — feeding compressed
	// bytes into mongorestore that expects raw BSON would error out.
	args := []string{
		"--host", host,
		"--port", port,
		"--username", mongoVerifierUser,
		"--config", configPath,
		"--authenticationDatabase", "admin",
		"--archive",
		"--quiet",
	}
	if mode == ModeSchemaOnly {
		// mongorestore has no native schema-only flag. The closest
		// approximation: --noIndexRestore reduces the work, and we
		// skip the smoke row-counts. Document rows still land. For
		// schema-only mongo verification we accept this gap rather
		// than hand-parse the BSON archive.
		args = append(args, "--noIndexRestore")
	}

	cmd := exec.CommandContext(ctx, "mongorestore", args...)
	cmd.Env = append(cmd.Env, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	cmd.Stdin = plaintext
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	log.V(1).Info("running mongorestore", "endpoint", endpoint, "mode", mode)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mongorestore: %w; stderr: %s", err, sanitiseStderr(stderr.String(), e.password))
	}
	return nil
}

func (e *mongoEngine) SmokeQueries(ctx context.Context, endpoint string, preTables map[string]int64, mode Mode, log logr.Logger) (*SmokeResult, error) {
	uri := fmt.Sprintf("mongodb://%s:%s@%s/?authSource=admin&serverSelectionTimeoutMS=10000",
		mongoVerifierUser, e.password, endpoint)
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		return nil, fmt.Errorf("smoke connect: %w", err)
	}
	defer func() { _ = client.Disconnect(ctx) }()

	res := &SmokeResult{}
	for fullName, expected := range preTables {
		dbName, collName := splitMongoCollection(fullName)
		if dbName == "" || collName == "" {
			res.Notes = append(res.Notes, fmt.Sprintf("could not split mongo name %q (expected db.coll)", fullName))
			res.Tables = append(res.Tables, TableSmoke{Name: fullName, Expected: expected, Got: -1, Match: false})
			continue
		}
		coll := client.Database(dbName).Collection(collName)
		got, err := coll.EstimatedDocumentCount(ctx)
		if err != nil {
			res.Notes = append(res.Notes, fmt.Sprintf("query failed for %s: %v", fullName, err))
			res.Tables = append(res.Tables, TableSmoke{Name: fullName, Expected: expected, Got: -1, Match: false})
			continue
		}
		match := smokeMatch(mode, expected, got)
		res.Tables = append(res.Tables, TableSmoke{Name: fullName, Expected: expected, Got: got, Match: match})
	}
	return res, nil
}

// splitMongoCollection: CollectStats names mongo collections "db.coll".
// We split on the first dot only — db names may not contain dots, but
// collection names CAN, so the rest belongs to the collection.
func splitMongoCollection(name string) (string, string) {
	idx := strings.IndexByte(name, '.')
	if idx <= 0 || idx == len(name)-1 {
		return "", ""
	}
	return name[:idx], name[idx+1:]
}

// writeMongoPasswordConfig writes a 0600 YAML config consumable by
// `mongorestore --config`, so the password never appears on the command line.
// Mirrors the mongo dumper's helper. Caller must os.Remove the returned path.
func writeMongoPasswordConfig(password string) (string, error) {
	f, err := os.CreateTemp("", "mongorestore-*.yaml")
	if err != nil {
		return "", err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", err
	}
	escaped := strings.ReplaceAll(password, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	if _, err := fmt.Fprintf(f, "password: \"%s\"\n", escaped); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}
