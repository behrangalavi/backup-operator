package docs

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strings"
)

// techStackPage is the JSON shape consumed by the SPA's /tech-stack page.
// Direct dependencies get hand-curated descriptions so the page is
// informative; indirect transitive deps are summarised as a single count.
type techStackPage struct {
	Module           string         `json:"module"`
	GoVersion        string         `json:"goVersion"`
	DirectDeps       []techDep      `json:"directDeps"`
	IndirectCount    int            `json:"indirectCount"`
	Frontend         []techDep      `json:"frontend"`
	OperationalDeps  []techDep      `json:"operationalDeps"`
	BuildTooling     []techDep      `json:"buildTooling"`
}

type techDep struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	License string `json:"license,omitempty"`
	Purpose string `json:"purpose"`
}

// loadTechStack parses go.mod for the module path, Go version, and direct
// dependencies, then enriches each direct dep with a hand-curated purpose
// string. Unknown deps still appear, just with an empty Purpose — better
// than hiding something that ships with the binary.
func loadTechStack(goModPath string) (*techStackPage, error) {
	f, err := os.Open(goModPath)
	if err != nil {
		return nil, fmt.Errorf("open go.mod: %w", err)
	}
	defer func() { _ = f.Close() }()

	out := &techStackPage{}
	out.Frontend = staticFrontendStack()
	out.OperationalDeps = staticOperationalDeps()
	out.BuildTooling = staticBuildTooling()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)

	inRequire := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		if strings.HasPrefix(line, "module ") {
			out.Module = strings.TrimSpace(strings.TrimPrefix(line, "module "))
			continue
		}
		if strings.HasPrefix(line, "go ") {
			out.GoVersion = strings.TrimSpace(strings.TrimPrefix(line, "go "))
			continue
		}
		if line == "require (" {
			inRequire = true
			continue
		}
		if inRequire && line == ")" {
			inRequire = false
			continue
		}

		// Single-line require also exists: `require foo/bar v1.2.3`.
		if !inRequire && strings.HasPrefix(line, "require ") {
			parseRequireLine(strings.TrimPrefix(line, "require "), out)
			continue
		}
		if inRequire {
			parseRequireLine(line, out)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan go.mod: %w", err)
	}

	sort.Slice(out.DirectDeps, func(i, j int) bool {
		return out.DirectDeps[i].Name < out.DirectDeps[j].Name
	})
	return out, nil
}

func parseRequireLine(line string, out *techStackPage) {
	indirect := strings.Contains(line, "// indirect")
	if i := strings.Index(line, "//"); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	parts := strings.Fields(line)
	if len(parts) < 2 {
		return
	}
	if indirect {
		out.IndirectCount++
		return
	}
	name, version := parts[0], parts[1]
	out.DirectDeps = append(out.DirectDeps, techDep{
		Name:    name,
		Version: version,
		License: depLicense(name),
		Purpose: depPurpose(name),
	})
}

// depPurpose maps a Go module path to a human-readable role in the project.
// Unmapped modules return "" — they still appear in the page list, so a
// reviewer can spot a new dep that hasn't been documented yet.
func depPurpose(name string) string {
	switch name {
	case "filippo.io/age":
		return "Age public-key encryption (X25519). Wraps every dump before upload; private key never enters the cluster."
	case "github.com/aws/aws-sdk-go-v2",
		"github.com/aws/aws-sdk-go-v2/config",
		"github.com/aws/aws-sdk-go-v2/credentials",
		"github.com/aws/aws-sdk-go-v2/feature/s3/manager",
		"github.com/aws/aws-sdk-go-v2/service/s3":
		return "S3-compatible storage backend (AWS, MinIO, Hetzner Object Storage, R2, B2, Wasabi)."
	case "github.com/go-logr/logr":
		return "Structured logging interface used throughout the operator. Compatible with controller-runtime."
	case "github.com/go-sql-driver/mysql":
		return "MySQL/MariaDB driver. Used by the dumper for stats collection (mysqldump handles the actual dump)."
	case "github.com/jackc/pgx/v5":
		return "PostgreSQL driver. Used for stats collection and schema fingerprinting (pg_dump handles the actual dump)."
	case "github.com/pkg/sftp":
		return "SFTP client for the SFTP and Hetzner-Storage-Box destinations."
	case "github.com/prometheus/client_golang":
		return "Prometheus metrics registry and HTTP exposition for /metrics."
	case "go.mongodb.org/mongo-driver/v2":
		return "MongoDB driver. Used for stats collection and schema fingerprinting (mongodump handles the actual dump)."
	case "golang.org/x/crypto":
		return "SSH transport, key parsing, and known-hosts validation for SFTP destinations."
	case "k8s.io/api", "k8s.io/apimachinery", "k8s.io/client-go":
		return "Kubernetes API types and client used by the operator and the UI."
	case "sigs.k8s.io/controller-runtime":
		return "Operator framework: Manager, reconcilers, leader election, health probes, cache."
	case "github.com/yuin/goldmark":
		return "Markdown → HTML renderer for this docs server."
	}
	return ""
}

// depLicense returns the SPDX-style license tag for direct deps. Hand-curated
// because go.mod doesn't carry license info — a separate `go-licenses` tool
// would be the rigorous answer, but adding a build dep just for this page
// is overkill.
func depLicense(name string) string {
	switch {
	case name == "filippo.io/age":
		return "BSD-3-Clause"
	case strings.HasPrefix(name, "github.com/aws/aws-sdk-go-v2"):
		return "Apache-2.0"
	case name == "github.com/go-logr/logr",
		name == "github.com/jackc/pgx/v5",
		name == "github.com/pkg/sftp",
		name == "github.com/yuin/goldmark":
		return "MIT"
	case name == "github.com/go-sql-driver/mysql":
		return "MPL-2.0"
	case name == "github.com/prometheus/client_golang":
		return "Apache-2.0"
	case name == "go.mongodb.org/mongo-driver/v2":
		return "Apache-2.0"
	case strings.HasPrefix(name, "golang.org/x/"):
		return "BSD-3-Clause"
	case strings.HasPrefix(name, "k8s.io/"),
		strings.HasPrefix(name, "sigs.k8s.io/"):
		return "Apache-2.0"
	}
	return ""
}

func staticFrontendStack() []techDep {
	return []techDep{
		{Name: "Vanilla JavaScript (ES2022+)", Purpose: "SPA shell with hash-based routing. No framework, no build step — keeps the binary self-contained and instantly deployable."},
		{Name: "Pure SVG charts", Purpose: "Dump-size trend, run-status heatmap, sparklines. Hand-rolled in 200 lines instead of pulling a 50-100KB charting library."},
		{Name: "Server-Sent Events", Purpose: "Live updates for the management UI. Unidirectional and HTTP-native, no protocol upgrade needed."},
		{Name: "go:embed for assets", Purpose: "All static files (HTML, CSS, JS, favicon) shipped inside the operator binary; nothing to mount."},
	}
}

func staticOperationalDeps() []techDep {
	return []techDep{
		{Name: "pg_dump", Purpose: "PostgreSQL logical dump tool. Invoked by the worker; password passed via PGPASSWORD env var."},
		{Name: "mysqldump", Purpose: "MySQL/MariaDB logical dump tool. Password passed via MYSQL_PWD env var (not on argv)."},
		{Name: "mongodump", Purpose: "MongoDB binary dump tool. Password passed via a 0600 YAML --config file (not on argv)."},
		{Name: "redis-cli --rdb", Purpose: "Redis snapshot dump. Password passed via REDISCLI_AUTH env var."},
		{Name: "Kubernetes batch/v1 CronJob", Purpose: "Schedules every backup run. The operator does not run cron in-process."},
		{Name: "Prometheus + Alertmanager", Purpose: "Metric scrape and alert routing. The operator ships PrometheusRule defaults; routing belongs to the cluster."},
	}
}

func staticBuildTooling() []techDep {
	return []techDep{
		{Name: "Go (CGO_ENABLED=0)", Purpose: "Static linking, multi-arch via cross-compilation, no glibc/musl drift between dev and prod."},
		{Name: "Just", Purpose: "Project task runner (build, test, image, local stack)."},
		{Name: "Docker buildx", Purpose: "Multi-arch image builds (linux/amd64 + linux/arm64)."},
		{Name: "Helm", Purpose: "OCI-distributed chart for installation and upgrades."},
		{Name: "GitHub Actions", Purpose: "CI (vet, lint, tests, helm lint) and CD (semantic-release → Docker + chart)."},
		{Name: "semantic-release", Purpose: "Conventional-commits → tagged release + GHCR image + Helm chart push."},
	}
}
