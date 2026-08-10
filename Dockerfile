FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder
WORKDIR /workspace

ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
ENV CGO_ENABLED=0
ARG VERSION=dev

COPY src/go.mod src/go.sum ./
RUN go mod download
COPY src .

RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} GOARM=${TARGETVARIANT#v} \
    go build -tags timetzdata -trimpath -gcflags="all=-l" \
    -ldflags="-s -w -X main.Version=${VERSION}" \
    -o backup-operator ./cmd/main.go

RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} GOARM=${TARGETVARIANT#v} \
    go build -tags timetzdata -trimpath -gcflags="all=-l" \
    -ldflags="-s -w" \
    -o backup-worker ./cmd/worker

# Final image needs pg_dump, mysqldump, mariadb-dump, mongodump, redis-cli
# for the worker — these are the actual backup tools we exec. The operator
# does not need them but the image is shared, which is fine: simpler
# distribution, no duplicate registry.
#
# Runtime is debian-slim (not alpine) because Oracle's MySQL-8 client is
# only available glibc-linked through the official MySQL APT repo —
# alpine's `mysql-client` is a dummy alias for mariadb-client. Shipping
# both `mysql-community-client` (Oracle, for dbType=mysql sources) and
# `mariadb-client` (for dbType=mariadb sources) lets the dumper pick the
# canonical tool per source rather than papering over differences with
# wire-protocol compatibility tricks.
FROM debian:bookworm-slim AS runtime
ARG MONGO_TOOLS_VERSION=100.16.1
ARG TARGETARCH

# Install once: trust the four upstream repos (debian main, MySQL,
# Mongo) before the package install so the apt-update covers them all.
# `--no-install-recommends` trims the image, but the DB client tooling still
# dominates: the final image is ~530MB (see the "Three binaries, one image" ADR).
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates curl gnupg; \
    install -d /usr/share/keyrings; \
    # PostgreSQL: debian-bookworm only ships postgresql-client-15.
    # pg_dump refuses to dump from a server two major versions newer
    # than itself (PG 17 server vs pg_dump 15 → "server version
    # mismatch"), so we pull pg_dump 17 from the official PGDG repo.
    # pg_dump 17 is backward-compatible and dumps PG 9.2+ correctly,
    # so a single client version covers every supported source.
    curl -fsSL https://www.postgresql.org/media/keys/ACCC4CF8.asc | gpg --dearmor -o /usr/share/keyrings/postgres.gpg; \
    echo "deb [signed-by=/usr/share/keyrings/postgres.gpg] https://apt.postgresql.org/pub/repos/apt bookworm-pgdg main" > /etc/apt/sources.list.d/pgdg.list; \
    # mysql-community-client-core (Oracle, amd64) and mariadb-client-core
    # both claim `virtual-mysql-client-core` and refuse to coexist —
    # apt-get install -f cannot resolve it. We pick one per arch:
    #   amd64: Oracle mysql-community-client-core. Real mysqldump for
    #          MySQL sources, also dumps MariaDB targets via shared
    #          wire protocol (with --column-statistics=0 to skip the
    #          column_statistics probe — handled by the runtime probe).
    #   arm64: mariadb-client. Oracle does not ship arm64 community
    #          packages; mysqldump resolves to mariadb-dump.
    # The dumper code calls `mysqldump` unconditionally; whichever
    # binary the arch ships responds. Both speak the MySQL wire
    # protocol so source dbType is independent of build arch.
    DBCLIENT_PKG="mariadb-client"; \
    case "${TARGETARCH}" in \
      amd64) \
        # MySQL rotates GPG keys yearly; pin to a specific year so
        # failed rotations don't surface as silent unsigned-repo
        # warnings. Bump when the upstream key expires.
        curl -fsSL https://repo.mysql.com/RPM-GPG-KEY-mysql-2025 | gpg --dearmor -o /usr/share/keyrings/mysql.gpg; \
        echo "deb [signed-by=/usr/share/keyrings/mysql.gpg] http://repo.mysql.com/apt/debian bookworm mysql-8.4-lts" > /etc/apt/sources.list.d/mysql.list; \
        DBCLIENT_PKG="mysql-community-client-core"; \
        ;; \
      arm64) \
        echo "arm64: Oracle MySQL community packages unavailable, using mariadb-client" >&2; \
        ;; \
    esac; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        postgresql-client-17 \
        ${DBCLIENT_PKG} \
        redis-tools; \
    # MongoDB Database Tools as a static tarball — keeps us off Mongo's
    # APT repo (which lags arm64 / has its own GPG ceremony) and gives a
    # single self-contained binary set. Pinned via build-arg so the
    # image is reproducible.
    #
    # Mongo ships debian12-x86_64 but no debian12-arm64; we use the
    # ubuntu2204-arm64 build for arm64 — same glibc generation, same
    # static binaries, works fine on debian-bookworm.
    case "${TARGETARCH}" in \
      amd64) MONGO_PKG="debian12-x86_64" ;; \
      arm64) MONGO_PKG="ubuntu2204-arm64" ;; \
      *) echo "unsupported arch: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL "https://fastdl.mongodb.org/tools/db/mongodb-database-tools-${MONGO_PKG}-${MONGO_TOOLS_VERSION}.tgz" \
      | tar -xz --strip-components=2 -C /usr/local/bin --wildcards '*/bin/mongodump' '*/bin/mongorestore' '*/bin/bsondump'; \
    apt-get purge -y curl gnupg; \
    apt-get autoremove -y; \
    rm -rf /var/lib/apt/lists/* \
           /etc/apt/sources.list.d/mysql.list /usr/share/keyrings/mysql.gpg \
           /etc/apt/sources.list.d/pgdg.list /usr/share/keyrings/postgres.gpg; \
    # `backup` is already taken in debian-slim (system uid 34); use
    # `backupop` for the unprivileged runtime user. Pod manifests
    # reference uid 1000, not the name, so this rename is invisible
    # to Kubernetes.
    useradd -u 1000 -m -s /bin/bash backupop

WORKDIR /app
COPY --from=builder /workspace/backup-operator /app/backup-operator
COPY --from=builder /workspace/backup-worker /app/backup-worker
# Documentation sources for the docs server (served on DOCS_ADDR when
# DOCS_ENABLED=true). Read at runtime, not embedded — keeps the docs
# package portable to repo layouts where these files live outside the
# package directory and out of go:embed reach.
COPY CLAUDE.md README.md /app/docs/
COPY src/go.mod /app/docs/go.mod
USER 1000:1000
ENTRYPOINT ["/app/backup-operator"]
