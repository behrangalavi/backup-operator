package azure

import (
	"context"
	"fmt"
	"io"
	"path"
	"strings"

	"backup-operator/storage"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	"github.com/go-logr/logr"
)

// Required Secret keys: account-name, account-key, container.
// Optional: path-prefix.
const (
	keyAccountName = "account-name"
	keyAccountKey  = "account-key"
	keyContainer   = "container"
	keyPrefix      = "path-prefix"
)

type azureStorage struct {
	name       string
	container  string
	pathPrefix string
	client     *azblob.Client
	logger     logr.Logger
}

func New(name string, data storage.SecretData, logger logr.Logger) (storage.Storage, error) {
	account := strings.TrimSpace(string(data[keyAccountName]))
	if account == "" {
		return nil, fmt.Errorf("azure storage %q: missing %q", name, keyAccountName)
	}
	key := strings.TrimSpace(string(data[keyAccountKey]))
	if key == "" {
		return nil, fmt.Errorf("azure storage %q: missing %q", name, keyAccountKey)
	}
	containerName := strings.TrimSpace(string(data[keyContainer]))
	if containerName == "" {
		return nil, fmt.Errorf("azure storage %q: missing %q", name, keyContainer)
	}

	cred, err := azblob.NewSharedKeyCredential(account, key)
	if err != nil {
		return nil, fmt.Errorf("azure storage %q: credential: %w", name, err)
	}
	serviceURL := fmt.Sprintf("https://%s.blob.core.windows.net", account)
	client, err := azblob.NewClientWithSharedKeyCredential(serviceURL, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("azure storage %q: client: %w", name, err)
	}

	return &azureStorage{
		name:       name,
		container:  containerName,
		pathPrefix: strings.TrimRight(string(data[keyPrefix]), "/"),
		client:     client,
		logger:     logger,
	}, nil
}

func (a *azureStorage) Name() string { return a.name }

func (a *azureStorage) full(p string) string {
	p = strings.TrimLeft(p, "/")
	if a.pathPrefix == "" {
		return p
	}
	return strings.TrimLeft(path.Join(a.pathPrefix, p), "/")
}

func (a *azureStorage) stripPrefix(key string) string {
	if a.pathPrefix == "" {
		return key
	}
	rel := strings.TrimPrefix(key, a.pathPrefix)
	return strings.TrimLeft(rel, "/")
}

func (a *azureStorage) Upload(ctx context.Context, p string, r io.Reader) error {
	_, err := a.client.UploadStream(ctx, a.container, a.full(p), r, nil)
	if err != nil {
		return fmt.Errorf("azure upload %s: %w", p, err)
	}
	return nil
}

func (a *azureStorage) List(ctx context.Context, prefix string) ([]storage.Object, error) {
	full := a.full(prefix)
	var out []storage.Object
	pager := a.client.NewListBlobsFlatPager(a.container, &container.ListBlobsFlatOptions{
		Prefix: &full,
	})
	for pager.More() {
		resp, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("azure list %s: %w", full, err)
		}
		for _, blob := range resp.Segment.BlobItems {
			obj := storage.Object{Path: a.stripPrefix(*blob.Name)}
			if blob.Properties != nil {
				if blob.Properties.ContentLength != nil {
					obj.Size = *blob.Properties.ContentLength
				}
				if blob.Properties.LastModified != nil {
					obj.LastModified = *blob.Properties.LastModified
				}
			}
			out = append(out, obj)
		}
	}
	return out, nil
}

func (a *azureStorage) Get(ctx context.Context, p string) (io.ReadCloser, error) {
	resp, err := a.client.DownloadStream(ctx, a.container, a.full(p), nil)
	if err != nil {
		return nil, fmt.Errorf("azure get %s: %w", p, err)
	}
	return resp.Body, nil
}

func (a *azureStorage) Delete(ctx context.Context, p string) error {
	_, err := a.client.DeleteBlob(ctx, a.container, a.full(p), nil)
	if err != nil {
		return fmt.Errorf("azure delete %s: %w", p, err)
	}
	return nil
}
