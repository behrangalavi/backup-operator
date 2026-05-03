package mongo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"backup-operator/dumper"

	"github.com/go-logr/logr"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// systemDatabases are excluded from stats unless the user explicitly targets one.
var systemDatabases = map[string]bool{
	"admin":  true,
	"local":  true,
	"config": true,
}

type mongoDumper struct {
	cfg    dumper.Config
	logger logr.Logger
}

func New(cfg dumper.Config, logger logr.Logger) dumper.Dumper {
	return &mongoDumper{cfg: cfg, logger: logger}
}

func (d *mongoDumper) Type() string { return "mongo" }

func (d *mongoDumper) Dump(ctx context.Context, w io.Writer) error {
	// Build the URI WITHOUT credentials so the connection string never lands
	// on the command line where `ps` could read it. mongodump accepts the
	// password via a 0600 YAML config file (`--config`); username goes via
	// the (non-sensitive) `--username` flag. Auth source / replica set still
	// belong in the URI query because mongodump applies them from there.
	uri := d.buildURI(false)
	args := []string{"--uri=" + uri, "--archive"}
	if d.cfg.Username != "" {
		args = append(args, "--username="+d.cfg.Username)
	}

	var configPath string
	if d.cfg.Password != "" {
		var err error
		configPath, err = writeMongoPasswordConfig(d.cfg.Password)
		if err != nil {
			return fmt.Errorf("write mongo config: %w", err)
		}
		defer func() { _ = os.Remove(configPath) }()
		args = append(args, "--config="+configPath)
	}

	if d.cfg.Database != "" {
		args = append(args, "--db="+d.cfg.Database)
	}

	cmd := exec.CommandContext(ctx, "mongodump", args...)
	cmd.Stdout = w

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	d.logger.V(1).Info("running mongodump", "host", d.cfg.Host, "db", d.cfg.Database)
	if err := cmd.Run(); err != nil {
		return dumper.WrapExecError("mongodump", err, stderr.String(), d.cfg.Password)
	}
	return nil
}

// writeMongoPasswordConfig writes a 0600 YAML config file consumable by
// `mongodump --config`. The file lives in os.TempDir; callers must remove it.
func writeMongoPasswordConfig(password string) (string, error) {
	f, err := os.CreateTemp("", "mongodump-*.yaml")
	if err != nil {
		return "", err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", err
	}
	// YAML-quote the password to survive every printable character. mongodump
	// parses this with a YAML 1.1 reader; double quotes + escaping the few
	// chars YAML cares about is the safest minimal form.
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

// buildURI assembles the connection string. With withCreds=false the URI is
// safe to pass on a command line — credentials are stripped and supplied
// out-of-band (`--username` flag + `--config` file). With withCreds=true the
// URI is suitable for the in-process Go driver, where it never leaks via ps.
func (d *mongoDumper) buildURI(withCreds bool) string {
	u := url.URL{
		Scheme: "mongodb",
		Host:   d.cfg.Host + ":" + strconv.Itoa(d.cfg.Port),
	}
	if withCreds {
		u.User = url.UserPassword(d.cfg.Username, d.cfg.Password)
	}
	q := u.Query()
	if authSource := d.cfg.Extra["authSource"]; authSource != "" {
		q.Set("authSource", authSource)
	}
	if rs := d.cfg.Extra["replicaSet"]; rs != "" {
		q.Set("replicaSet", rs)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// CollectStats per collection: EstimatedDocumentCount for the count (uses
// metadata, O(1)), and the collStats command for storage size. The schema
// hash fingerprints collection names + index specs — Mongo is schemaless
// at the document level, so hashing documents would just produce noise.
func (d *mongoDumper) CollectStats(ctx context.Context) (*dumper.Stats, error) {
	clientOpts := options.Client().ApplyURI(d.buildURI(true))
	client, err := mongo.Connect(clientOpts)
	if err != nil {
		return nil, dumper.SanitizeError("connect", err, d.cfg.Password)
	}
	defer func() { _ = client.Disconnect(ctx) }()

	if err := client.Ping(ctx, nil); err != nil {
		return nil, dumper.SanitizeError("ping", err, d.cfg.Password)
	}

	dbNames, err := d.listTargetDatabases(ctx, client)
	if err != nil {
		return nil, fmt.Errorf("list databases: %w", err)
	}

	var (
		tables   []dumper.TableStats
		hashSeed []string
	)
	for _, dbName := range dbNames {
		db := client.Database(dbName)
		collNames, err := db.ListCollectionNames(ctx, bson.D{})
		if err != nil {
			return nil, fmt.Errorf("list collections in %s: %w", dbName, err)
		}
		sort.Strings(collNames)

		for _, collName := range collNames {
			coll := db.Collection(collName)

			count, err := coll.EstimatedDocumentCount(ctx)
			if err != nil {
				return nil, fmt.Errorf("count %s.%s: %w", dbName, collName, err)
			}
			size, err := collectionSize(ctx, db, collName)
			if err != nil {
				return nil, fmt.Errorf("collStats %s.%s: %w", dbName, collName, err)
			}
			tables = append(tables, dumper.TableStats{
				Name:      dbName + "." + collName,
				RowCount:  count,
				SizeBytes: size,
			})

			indexSig, err := indexSignature(ctx, coll)
			if err != nil {
				return nil, fmt.Errorf("indexes %s.%s: %w", dbName, collName, err)
			}
			hashSeed = append(hashSeed, dbName+"."+collName+"#"+indexSig)
		}
	}

	return &dumper.Stats{
		SchemaHash:  hashSchema(hashSeed),
		Tables:      tables,
		GeneratedAt: time.Now().UTC(),
	}, nil
}

// listTargetDatabases respects cfg.Database when set; otherwise returns all
// non-system databases.
func (d *mongoDumper) listTargetDatabases(ctx context.Context, client *mongo.Client) ([]string, error) {
	if d.cfg.Database != "" {
		return []string{d.cfg.Database}, nil
	}
	all, err := client.ListDatabaseNames(ctx, bson.D{})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(all))
	for _, name := range all {
		if systemDatabases[name] {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

func collectionSize(ctx context.Context, db *mongo.Database, collName string) (int64, error) {
	cmd := bson.D{{Key: "collStats", Value: collName}}
	var raw bson.M
	if err := db.RunCommand(ctx, cmd).Decode(&raw); err != nil {
		return 0, err
	}
	switch v := raw["size"].(type) {
	case int32:
		return int64(v), nil
	case int64:
		return v, nil
	case float64:
		return int64(v), nil
	}
	return 0, nil
}

// indexSignature produces a stable string for one collection's index set —
// names + key specs. Sorted by index name so output is deterministic.
func indexSignature(ctx context.Context, coll *mongo.Collection) (string, error) {
	cur, err := coll.Indexes().List(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = cur.Close(ctx) }()

	type indexEntry struct {
		Name string
		Keys string
	}
	var entries []indexEntry
	for cur.Next(ctx) {
		var raw bson.M
		if err := cur.Decode(&raw); err != nil {
			return "", err
		}
		name, _ := raw["name"].(string)
		keyDoc, _ := raw["key"].(bson.M)
		keys, err := json.Marshal(sortedMap(keyDoc))
		if err != nil {
			return "", err
		}
		entries = append(entries, indexEntry{Name: name, Keys: string(keys)})
	}
	if err := cur.Err(); err != nil {
		return "", err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })

	parts := make([]string, 0, len(entries))
	for _, e := range entries {
		parts = append(parts, e.Name+":"+e.Keys)
	}
	sort.Strings(parts)
	return strings.Join(parts, ";"), nil
}

func sortedMap(m bson.M) []kv {
	out := make([]kv, 0, len(m))
	for k, v := range m {
		out = append(out, kv{K: k, V: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].K < out[j].K })
	return out
}

type kv struct {
	K string `json:"k"`
	V any    `json:"v"`
}

func hashSchema(seed []string) string {
	sort.Strings(seed)
	h := sha256.New()
	for _, s := range seed {
		h.Write([]byte(s))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}


