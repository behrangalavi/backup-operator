# Adding a New Backend

## 9. Adding a New Backend

### 9.1 New database type

1. Create `src/dumper/<name>/<name>.go` implementing `dumper.Dumper`:
   - `Type() string` → return the type string used in `spec.database.type`
   - `Dump(ctx, w io.Writer) error` → exec the dump tool, stream to `w`
   - `CollectStats(ctx) (*Stats, error)` → query the live DB for table-level rows + size and a schema fingerprint
2. Register the type-constant in `src/dumper/factory/factory.go`. If your new
   type can reuse an existing dumper (e.g. MariaDB shares MySQL's wire
   protocol and `mysqldump`), share the case rather than duplicating code:
   ```go
   case TypeMySQL, TypeMariaDB:
       return mysql.New(cfg, logger.WithName(dbType)), nil
   ```
3. Update the `Dockerfile` to install the matching client tool (`apk add ...`).
4. Document the type in section 6.1 of this file.

The DB driver is consumed only by `CollectStats`; if you can live without semantic alerts on the new type, returning `nil, fmt.Errorf("not implemented")` from `CollectStats` is acceptable as a stage-one ship — the dump still works, the analyzer just stays quiet.

### 9.2 New storage backend

1. Create `src/storage/<name>/<name>.go` implementing `storage.Storage`:
   - `Name() string`
   - `Upload(ctx, path string, r io.Reader) error`
   - `List(ctx, prefix string) ([]Object, error)` — **must return prefix-stripped logical paths**
   - `Get(ctx, path string) (io.ReadCloser, error)`
   - `Delete(ctx, path string) error`
2. Map the provider's not-found error to `storage.ErrNotFound` in `Get`/`Delete`, make `Delete` of a missing object return nil, implement `Close` (no-op if there is no persistent client), and add the compile-time assertions (`var _ storage.Storage = (*myStorage)(nil)`; plus `BatchStorage`/`DirectoryRemover` if implemented).
3. Run the conformance suite against it where it can run without network (`storagetest.Run`), and add the prefix-bleed regression test for string-prefix stores.
4. Register the type-constant in `src/storage/factory/factory.go`, extend `secrets.CredentialKeys` with the backend's credential keys, add the spec block to `BackupDestination` (`just generate`).
5. Document the data-key schema in sections 6.4/6.5.

If your backend has its own native encryption (e.g. server-side S3 encryption), still go through `age` first. The whole point of public-key encryption is that the cluster can't read its own backups; relying on the storage provider's keys breaks that.

---
