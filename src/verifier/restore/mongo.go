package restore

import (
	"bytes"
	"context"
	"fmt"
	"io"
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

type mongoEngine struct{}

func (mongoEngine) DBType() string { return "mongo" }

func (mongoEngine) PodSpec(volumeBytes int64, imageOverride string) ephemeral.Spec {
	image := imageOverride
	if image == "" {
		image = DefaultImage("mongo")
	}
	return ephemeral.Spec{
		Image: image,
		Port:  27017,
		EnvVars: []corev1.EnvVar{
			{Name: "MONGO_INITDB_ROOT_USERNAME", Value: mongoVerifierUser},
			{Name: "MONGO_INITDB_ROOT_PASSWORD", Value: verifierPassword},
		},
		VolumeMountPath: "/data/db",
		VolumeSizeBytes: volumeBytes,
		ReadyTimeout:    5 * time.Minute,
		Probe:           probeMongo,
	}
}

func probeMongo(ctx context.Context, endpoint string) error {
	uri := fmt.Sprintf("mongodb://%s:%s@%s/?authSource=admin&serverSelectionTimeoutMS=3000",
		mongoVerifierUser, verifierPassword, endpoint)
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		return err
	}
	defer func() { _ = client.Disconnect(ctx) }()
	return client.Ping(ctx, nil)
}

func (mongoEngine) Restore(ctx context.Context, endpoint string, plaintext io.Reader, mode Mode, log logr.Logger) error {
	host, port, err := splitEndpoint(endpoint)
	if err != nil {
		return err
	}

	// mongorestore reads its archive from stdin via --archive (no path).
	// --gzip is intentionally OMITTED here because the verifier-driver
	// has already gunzipped the stream upstream — feeding compressed
	// bytes into mongorestore that expects raw BSON would error out.
	args := []string{
		"--host", host,
		"--port", port,
		"--username", mongoVerifierUser,
		"--password", verifierPassword,
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
		return fmt.Errorf("mongorestore: %w; stderr: %s", err, sanitiseMongoStderr(stderr.String()))
	}
	return nil
}

func (mongoEngine) SmokeQueries(ctx context.Context, endpoint string, preTables map[string]int64, mode Mode, log logr.Logger) (*SmokeResult, error) {
	uri := fmt.Sprintf("mongodb://%s:%s@%s/?authSource=admin&serverSelectionTimeoutMS=10000",
		mongoVerifierUser, verifierPassword, endpoint)
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

func sanitiseMongoStderr(s string) string {
	return strings.ReplaceAll(s, verifierPassword, "***")
}
