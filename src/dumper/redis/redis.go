package redis

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"backup-operator/dumper"

	"github.com/go-logr/logr"
)

type redisDumper struct {
	cfg    dumper.Config
	logger logr.Logger
}

func New(cfg dumper.Config, logger logr.Logger) dumper.Dumper {
	return &redisDumper{cfg: cfg, logger: logger}
}

func (d *redisDumper) Type() string { return "redis" }

// Dump streams an RDB snapshot from the live instance via `redis-cli --rdb -`.
// RDB is per-instance, so the entire keyspace (all DB indexes) is captured —
// cfg.Database scopes stats only.
func (d *redisDumper) Dump(ctx context.Context, w io.Writer) error {
	args := d.baseArgs()
	args = append(args, "--rdb", "-")

	cmd := exec.CommandContext(ctx, "redis-cli", args...)
	cmd.Env = d.envWithAuth()
	cmd.Stdout = w

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	d.logger.V(1).Info("running redis-cli --rdb", "host", d.cfg.Host)
	if err := cmd.Run(); err != nil {
		return dumper.WrapExecError("redis-cli --rdb", err, stderr.String(), d.cfg.Password)
	}
	return nil
}

// CollectStats reads `INFO keyspace` to build per-DB key counts. Redis is
// schemaless at the value level, so the schema fingerprint is just the sorted
// list of populated DB indexes — a new DB index appearing or disappearing
// shows up as a schema change, which is the closest analogue Redis offers.
func (d *redisDumper) CollectStats(ctx context.Context) (*dumper.Stats, error) {
	out, err := d.runInfo(ctx, "keyspace")
	if err != nil {
		return nil, fmt.Errorf("INFO keyspace: %w", err)
	}
	dbs := parseKeyspace(out)

	if d.cfg.Database != "" {
		// User narrowed the source to one DB — keep only that one in stats.
		want := strings.TrimSpace(d.cfg.Database)
		if !strings.HasPrefix(want, "db") {
			want = "db" + want
		}
		filtered := make([]dbStat, 0, 1)
		for _, s := range dbs {
			if s.name == want {
				filtered = append(filtered, s)
			}
		}
		dbs = filtered
	}

	tables := make([]dumper.TableStats, 0, len(dbs))
	hashSeed := make([]string, 0, len(dbs))
	for _, s := range dbs {
		tables = append(tables, dumper.TableStats{
			Name:      s.name,
			RowCount:  s.keys,
			SizeBytes: 0, // Redis does not expose per-DB byte size.
		})
		hashSeed = append(hashSeed, s.name)
	}

	return &dumper.Stats{
		SchemaHash:  hashSchema(hashSeed),
		Tables:      tables,
		GeneratedAt: time.Now().UTC(),
	}, nil
}

func (d *redisDumper) baseArgs() []string {
	args := []string{
		"-h", d.cfg.Host,
		"-p", strconv.Itoa(d.cfg.Port),
	}
	if d.cfg.Username != "" && d.cfg.Username != "default" {
		// ACL user (Redis 6+). Old-style auth uses no username.
		args = append(args, "--user", d.cfg.Username)
	}
	if tls := d.cfg.Extra["tls"]; tls == "true" {
		args = append(args, "--tls")
	}
	return args
}

// envWithAuth passes the password via REDISCLI_AUTH so it never lands on the
// command line — `ps` and process listings would otherwise leak it. We
// inherit os.Environ so PATH/HOME stay intact.
func (d *redisDumper) envWithAuth() []string {
	if d.cfg.Password == "" {
		return nil
	}
	return append(os.Environ(), "REDISCLI_AUTH="+d.cfg.Password)
}

func (d *redisDumper) runInfo(ctx context.Context, section string) (string, error) {
	args := append(d.baseArgs(), "INFO", section)
	cmd := exec.CommandContext(ctx, "redis-cli", args...)
	cmd.Env = d.envWithAuth()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", dumper.WrapExecError("redis-cli INFO", err, stderr.String(), d.cfg.Password)
	}
	return stdout.String(), nil
}

type dbStat struct {
	name string
	keys int64
}

// parseKeyspace turns `INFO keyspace` output into per-DB stats. Lines look
// like: `db0:keys=12345,expires=0,avg_ttl=0`.
func parseKeyspace(s string) []dbStat {
	var out []dbStat
	scanner := bufio.NewScanner(strings.NewReader(s))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "db") {
			continue
		}
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		name := line[:colon]
		fields := strings.Split(line[colon+1:], ",")
		var keys int64
		for _, f := range fields {
			eq := strings.IndexByte(f, '=')
			if eq < 0 {
				continue
			}
			if f[:eq] == "keys" {
				if n, err := strconv.ParseInt(f[eq+1:], 10, 64); err == nil {
					keys = n
				}
				break
			}
		}
		out = append(out, dbStat{name: name, keys: keys})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
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