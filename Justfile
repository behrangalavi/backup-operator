export CGO_ENABLED := "0"

set dotenv-load

[private]
default:
    just --list --unsorted

# Build a native binary
build:
    #!/usr/bin/env sh
    VERSION=$(git describe --tags $(git rev-list --tags --max-count=1) 2>/dev/null || echo "dev")
    cd src && go build -tags timetzdata -trimpath -gcflags="all=-l" \
        -ldflags="-s -w -X main.Version=${VERSION}" \
        -o ../dist/native/backup-operator ./cmd/main.go

# Build the worker binary (one-shot backup runner, launched by CronJob pods)
build-worker:
    cd src && go build -tags timetzdata -trimpath -gcflags="all=-l" \
        -ldflags="-s -w" \
        -o ../dist/native/backup-worker ./cmd/worker

# Build the restore CLI (runs locally, not inside the cluster)
build-restore:
    cd src && go build -tags timetzdata -trimpath -gcflags="all=-l" \
        -ldflags="-s -w" \
        -o ../dist/native/backup-restore ./cmd/restore

# Build binaries for all targets
build-all: build-linux-amd64 build-linux-arm64

build-linux-amd64:
    cd src && GOOS=linux GOARCH=amd64 go build -tags timetzdata -trimpath -gcflags="all=-l" \
        -ldflags="-s -w" -o ../dist/amd64/backup-operator ./cmd/main.go

build-linux-arm64:
    cd src && GOOS=linux GOARCH=arm64 go build -tags timetzdata -trimpath -gcflags="all=-l" \
        -ldflags="-s -w" -o ../dist/arm64/backup-operator ./cmd/main.go

# Tidy module deps
tidy:
    cd src && go mod tidy

# Run tests and linters for quick local iteration
check: golangci-lint test-unit

# Execute unit tests
test-unit:
    cd src && go run gotest.tools/gotestsum@latest --format="testname" --hide-summary="skipped" --format-hide-empty-pkg --rerun-fails="0" -- -count=1 ./...

# Execute golangci-lint (pinned to match .github/workflows/ci.yaml — bump both together).
# Uses the prebuilt release binary, not `go run`: building golangci-lint from
# source honours its own `toolchain go1.25.x` pin, which then refuses to lint a
# module whose `go` directive is newer (ours is 1.26.2). The release binary is
# built with go1.26.2, so it lints cleanly. Cached outside the repo, keyed by
# version, so a bump re-downloads automatically.
golangci-lint:
    #!/usr/bin/env sh
    set -e
    VERSION=v2.12.2
    BINDIR="${XDG_CACHE_HOME:-$HOME/.cache}/backup-operator-tools/golangci-lint/$VERSION"
    if [ ! -x "$BINDIR/golangci-lint" ]; then
        curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/HEAD/install.sh | sh -s -- -b "$BINDIR" "$VERSION"
    fi
    cd src && "$BINDIR/golangci-lint" run '--max-same-issues=0' '--timeout=1h' ./...

# Build a docker image (multi-arch via buildx)
build-docker image arch="amd64":
    #!/usr/bin/env sh
    VERSION=$(git describe --tags $(git rev-list --tags --max-count=1) 2>/dev/null || echo "dev")
    docker buildx build --platform=linux/{{arch}} -f Dockerfile \
        --build-arg VERSION=$VERSION \
        -t {{image}}:$VERSION-{{arch}} \
        -t {{image}}:latest-{{arch}} \
        --load .

# === Local Test Setup (Docker Desktop K8s) ===
# Docker Desktop's Kubernetes shares the local Docker daemon, so an image
# built with `docker build` is immediately available to the cluster — no
# registry push, no `kind load`. The `.env` (copy from .env.example) wires
# the operator's required envs for `just run`.

# Build the worker/operator image into the local Docker daemon
build-image:
    docker build -t backup-operator:dev .

# Run the operator locally against the current kubeconfig (reads .env)
run: build
    dist/native/backup-operator

# Generate age key pair (idempotent) and apply the public-key Secret
gen-age-key:
    #!/usr/bin/env sh
    set -e
    KEYFILE="$HOME/age-backup-test.key"
    if [ ! -f "$KEYFILE" ]; then
        cd src && go run filippo.io/age/cmd/age-keygen -o "$KEYFILE"
    fi
    PUB=$(grep "public key:" "$KEYFILE" | cut -d' ' -f4)
    echo "Public key: $PUB"
    kubectl create namespace backup --dry-run=client -o yaml | kubectl apply -f -
    kubectl -n backup create secret generic backup-operator-age \
        --from-literal=AGE_PUBLIC_KEYS="$PUB" \
        --dry-run=client -o yaml | kubectl apply -f -

# Apply the local test stack (namespace, RBAC, Postgres, MinIO, secrets)
test-up:
    kubectl apply -f test/local/

# Trigger a manual backup run (bypasses the schedule)
# `kubectl create job --from=cronjob/...` does not copy ttlSecondsAfterFinished,
# so we patch it back in to keep the manual run subject to the same 24h cleanup.
test-trigger:
    #!/usr/bin/env sh
    set -e
    NAME=manual-$(date +%s)
    kubectl -n backup create job --from=cronjob/backup-test-postgres $NAME
    kubectl -n backup patch job $NAME --type=merge -p '{"spec":{"ttlSecondsAfterFinished":86400}}'

# Tear down the test stack and the age Secret
test-down:
    kubectl delete -f test/local/ --ignore-not-found
    kubectl -n backup delete secret backup-operator-age --ignore-not-found
