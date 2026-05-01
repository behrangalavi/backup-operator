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

# Final image needs pg_dump, mysqldump, mongodump for the worker — these are
# the actual backup tools we exec. The operator does not need them but the
# image is shared, which is fine: simpler distribution, no duplicate registry.
FROM alpine:3.21
RUN apk add --no-cache \
    ca-certificates \
    postgresql17-client \
    mariadb-client \
    mongodb-tools \
    && adduser -D -u 1000 backup
WORKDIR /app
COPY --from=builder /workspace/backup-operator /app/backup-operator
COPY --from=builder /workspace/backup-worker /app/backup-worker
USER 1000:1000
ENTRYPOINT ["/app/backup-operator"]
