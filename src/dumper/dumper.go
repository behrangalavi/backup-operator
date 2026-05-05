package dumper

import (
	"context"
	"io"
	"time"
)

// Config holds connection details parsed from a discovered DB Secret.
// Extra carries type-specific options (e.g. postgres "sslmode", mongo "authSource").
type Config struct {
	Name     string
	Host     string
	Port     int
	Database string
	Username string
	Password string
	Extra    map[string]string
}

// Stats is captured before/independent of the dump so the analyzer can compare
// the current database state to the previous run without re-parsing the dump.
//
// Charset / Collation are recorded on a best-effort basis (only meaningful
// for engines that track them at the database level — postgres, mysql,
// mariadb). Drift across runs flags servers being upgraded or migrated
// without reconciling the new defaults, which silently breaks Umlauts /
// emoji on restore.
type Stats struct {
	SchemaHash  string       `json:"schemaHash"`
	Charset     string       `json:"charset,omitempty"`
	Collation   string       `json:"collation,omitempty"`
	Tables      []TableStats `json:"tables"`
	GeneratedAt time.Time    `json:"generatedAt"`
}

type TableStats struct {
	Name      string `json:"name"`
	RowCount  int64  `json:"rowCount"`
	SizeBytes int64  `json:"sizeBytes"`
}

// Dumper streams a logical dump and exposes pre-dump database statistics.
// Implementations must be safe for concurrent CollectStats / Dump calls
// against independent Dumper instances (one Dumper per backup run is fine).
type Dumper interface {
	Type() string
	Dump(ctx context.Context, w io.Writer) error
	CollectStats(ctx context.Context) (*Stats, error)
}
