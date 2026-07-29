package gcs

import (
	"context"
	"fmt"
	"io"
	"strings"

	"backup-operator/storage"

	"cloud.google.com/go/auth/credentials"
	gcstorage "cloud.google.com/go/storage"
	"github.com/go-logr/logr"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

// Required Secret keys: bucket, service-account-json.
// Optional: path-prefix.
const (
	keyBucket = "bucket"
	keySAJSON = "service-account-json"
	keyPrefix = "path-prefix"
)

type gcsStorage struct {
	name       string
	bucket     string
	pathPrefix string
	client     *gcstorage.Client
	logger     logr.Logger
}

func New(name string, data storage.SecretData, logger logr.Logger) (storage.Storage, error) {
	bucket := strings.TrimSpace(string(data[keyBucket]))
	if bucket == "" {
		return nil, fmt.Errorf("gcs storage %q: missing %q", name, keyBucket)
	}
	saJSON := data[keySAJSON]
	if len(saJSON) == 0 {
		return nil, fmt.Errorf("gcs storage %q: missing %q", name, keySAJSON)
	}

	creds, err := credentials.DetectDefault(&credentials.DetectOptions{
		CredentialsJSON: saJSON,
		Scopes:          []string{gcstorage.ScopeReadWrite},
	})
	if err != nil {
		return nil, fmt.Errorf("gcs storage %q: credentials: %w", name, err)
	}
	client, err := gcstorage.NewClient(context.Background(),
		option.WithAuthCredentials(creds),
	)
	if err != nil {
		return nil, fmt.Errorf("gcs storage %q: client: %w", name, err)
	}

	return &gcsStorage{
		name:       name,
		bucket:     bucket,
		pathPrefix: strings.TrimRight(string(data[keyPrefix]), "/"),
		client:     client,
		logger:     logger,
	}, nil
}

func (g *gcsStorage) Name() string { return g.name }

// full joins the path-prefix to a caller path by plain concatenation, NOT
// path.Join. path.Join runs path.Clean, which strips the trailing slash a
// List prefix carries for target isolation ("db/" → "db"): with a non-empty
// path-prefix that turns List("db/") into the string prefix "prefix/db",
// which also matches sibling targets like "prefix/db-archive/..." — bleeding
// one target's listing (and therefore its retention deletes) into another.
// Concatenation preserves the caller's trailing slash exactly. pathPrefix has
// its trailing slash trimmed at construction, so we add exactly one separator.
func (g *gcsStorage) full(p string) string {
	p = strings.TrimLeft(p, "/")
	if g.pathPrefix == "" {
		return p
	}
	return g.pathPrefix + "/" + p
}

// stripPrefix trims the full "pathPrefix/" (including the separator), not just
// pathPrefix. Trimming the bare prefix would turn a sibling key like
// "prod-eu/db/..." under prefix "prod" into a mangled "-eu/db/...".
func (g *gcsStorage) stripPrefix(key string) string {
	if g.pathPrefix == "" {
		return key
	}
	return strings.TrimPrefix(key, g.pathPrefix+"/")
}

func (g *gcsStorage) Upload(ctx context.Context, p string, r io.Reader) error {
	w := g.client.Bucket(g.bucket).Object(g.full(p)).NewWriter(ctx)
	if _, err := io.Copy(w, r); err != nil {
		_ = w.Close()
		return fmt.Errorf("gcs upload %s: %w", p, err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("gcs upload close %s: %w", p, err)
	}
	return nil
}

func (g *gcsStorage) List(ctx context.Context, prefix string) ([]storage.Object, error) {
	full := g.full(prefix)
	var out []storage.Object
	it := g.client.Bucket(g.bucket).Objects(ctx, &gcstorage.Query{
		Prefix: full,
	})
	for {
		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("gcs list %s: %w", full, err)
		}
		out = append(out, storage.Object{
			Path:         g.stripPrefix(attrs.Name),
			Size:         attrs.Size,
			LastModified: attrs.Updated,
		})
	}
	return out, nil
}

func (g *gcsStorage) Get(ctx context.Context, p string) (io.ReadCloser, error) {
	r, err := g.client.Bucket(g.bucket).Object(g.full(p)).NewReader(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcs get %s: %w", p, err)
	}
	return r, nil
}

func (g *gcsStorage) Delete(ctx context.Context, p string) error {
	err := g.client.Bucket(g.bucket).Object(g.full(p)).Delete(ctx)
	if err != nil {
		return fmt.Errorf("gcs delete %s: %w", p, err)
	}
	return nil
}
