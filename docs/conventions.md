# Coding Conventions

## 8. Coding Conventions

### 8.1 Everything is Generic — Program to Interfaces

Any component that touches a database, storage backend, encryption, or external system is an interface. Add new types by implementing the interface and registering it in the factory; **never branch on type strings outside the factory**.

Existing extension points:

| Interface | When to extend |
|---|---|
| `dumper.Dumper` | Adding a new database engine |
| `storage.Storage` | Adding a new upload backend. Contract: `Upload`/`List`/`Get`/`Delete`/`Close`; `Get`/`Delete` on a missing object wrap `storage.ErrNotFound` and `Delete` of a missing object succeeds (idempotent). Optional: `storage.BatchStorage` (one session for many calls — SFTP/FTPS) and `storage.DirectoryRemover` (retention prunes empty date dirs — SFTP/FTPS). `storage/storagetest.Run` is the conformance suite every backend's semantics are measured against. |
| `crypto.Encryptor` / `crypto.Decryptor` | If we ever support a non-`age` scheme |
| `analyzer.Analyzer` | Stricter or more flexible diff rules |
| `backup.DestinationProvider` | Currently the worker uses a static list; the interface exists to keep the pipeline testable |

### 8.2 Factory Pattern is Mandatory

`dumper/factory/factory.go` and `storage/factory/factory.go` are the **only** places that branch on type strings:

```go
switch dbType {
case "postgres": return postgres.New(cfg, log), nil
case "mysql":    return mysql.New(cfg, log), nil
case "mongo":    return mongo.New(cfg, log), nil
default:         return nil, fmt.Errorf("unsupported db-type %q", dbType)
}
```

If you find yourself writing `if storage.Type() == "s3"` or similar in calling code, that's a smell — it belongs in the factory or as a method on the interface.

### 8.3 Configuration Access

- Declare new config values in the relevant binary's `InitializeConfigModule` call (`Optional`/`Default`/`Validate`).
- Access via `config.GetValue(KEY)` — never `os.Getenv` directly outside `cmd/`.
- The schema in `main.go` is the single source of truth for what the binary accepts.

### 8.4 Error Handling

- `fmt.Errorf("context: %w", err)` for normal wrapping.
- `assert.NoError()` / `assert.Assert()` only for unrecoverable startup failures (config init, manager creation).
- **Per-destination upload errors do NOT abort the whole run** — they are surfaced via `backup_operator_destination_failed`. A single bad destination cannot prevent all backups.
- **Retention errors do NOT fail the run** — old dumps are best-effort, fresh dumps are mandatory.
- **Stats collection errors do NOT fail the run** — the analyzer just skips the comparison.

### 8.5 Concurrency

- The pipeline writes the encrypted dump to a single temp file once, then fans out to N destinations in parallel (`sync.WaitGroup`).
- The worker is one-shot; no inter-run concurrency to worry about within a pod.
- Operator replicas race on `CreateOrUpdate` → harmless, last write wins. Leader election is still set so unnecessary work is minimised.
- K8s CronJob `concurrencyPolicy: Forbid` prevents *overlap* of runs against the same source — a 6-hour dump under an hourly schedule simply skips ticks until it finishes.

### 8.6 Logging

Use the `logr.Logger` interface throughout (injected, never global).

- `logger.Info(...)` for normal operational events
- `logger.V(1).Info(...)` for verbose/debug output
- `logger.Error(err, ...)` for errors with context
- Never `fmt.Println` or `log.Print` in production code paths

### 8.7 Tests

- The analyzer (`analyzer/`), the flatteners (`internal/secrets/`), the retention selector (`internal/backup/retention.go`), the SFTP host-key callback (`storage/sftp/`), the migration (`internal/migrate/`), the controllers (fake client with status subresource) and the storage backends (`storage/storagetest` conformance suite + in-memory fake) all have unit tests. Pure-function logic is the right place for tests; integration with real DBs and real storage is left to the cluster. Fuzz targets exist for `RowCounter` and `ApplyJitter`.
- `go test -race ./...` from `src/` is the gate. CI runs `just check` (generate-check + lint + Go tests + `node --test` for the SPA + helm-unittest/kubeconform for the chart).
- golangci-lint runs `errcheck`, `gosec`, `govet`, `ineffassign`, `staticcheck`, `unused`, `bodyclose`, `errorlint` (errorf=false so the deliberate chain-breaking `%v` on sanitized driver errors stays legal), `gocognit` (≤40 — chosen over funlen: a long flat table function is fine, deep nesting is what hides bugs), `misspell` (no locale, the codebase is British), `noctx` (tests use `httptest.NewRequestWithContext` / the `httpGetCtx` helpers), `prealloc`, `unconvert`. No `nolint` directives are in the tree; keep it that way.

### 8.8 Comments

Comments explain **why**, not what. The expected reader knows Go and Kubernetes.

Good targets for a comment:

- A non-obvious workaround (e.g. "knownhosts.New only takes file paths, so we materialise...")
- A subtle invariant (e.g. "Object.Path is already prefix-stripped — passing it back to Get round-trips correctly")
- An intentional trade-off (e.g. "estimates from pg_stat_user_tables; exact COUNT(*) would be cost-prohibitive on large tables")

Avoid restating the code. Avoid comments that reference the current change ("added for issue #123") — those belong in the commit message.

---
