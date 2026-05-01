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
	uri := d.buildURI()

	args := []string{"--uri=" + uri, "--archive"}
	if d.cfg.Database != "" {
		args = append(args, "--db="+d.cfg.Database)
	}

	cmd := exec.CommandContext(ctx, "mongodump", args...)
	cmd.Stdout = w

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	d.logger.V(1).Info("running mongodump", "host", d.cfg.Host, "db", d.cfg.Database)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mongodump failed: %w: %s", err, stderr.String())
	}
	return nil
}

func (d *mongoDumper) buildURI() string {
	u := url.URL{
		Scheme: "mongodb",
		Host:   d.cfg.Host + ":" + strconv.Itoa(d.cfg.Port),
		User:   url.UserPassword(d.cfg.Username, d.cfg.Password),
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
	clientOpts := options.Client().ApplyURI(d.buildURI())
	client, err := mongo.Connect(clientOpts)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = client.Disconnect(ctx) }()

	if err := client.Ping(ctx, nil); err != nil {
		return nil, fmt.Errorf("ping: %w", err)
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
	defer cur.Close(ctx)

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


