package s3

import (
	"context"
	"fmt"
	"io"
	"path"
	"strings"

	"backup-operator/storage"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/go-logr/logr"
)

// Required Secret keys: bucket, access-key-id, secret-access-key.
// Optional: endpoint (for non-AWS providers), region, path-style, path-prefix.
const (
	keyEndpoint  = "endpoint"
	keyRegion    = "region"
	keyBucket    = "bucket"
	keyAccessKey = "access-key-id"
	keySecretKey = "secret-access-key"
	keyPathStyle = "path-style"
	keyPrefix    = "path-prefix"
)

type s3Storage struct {
	name       string
	bucket     string
	pathPrefix string
	client     *awss3.Client
	uploader   *manager.Uploader
	logger     logr.Logger
}

func New(name string, data storage.SecretData, logger logr.Logger) (storage.Storage, error) {
	bucket := strings.TrimSpace(string(data[keyBucket]))
	if bucket == "" {
		return nil, fmt.Errorf("s3 storage %q: missing %q", name, keyBucket)
	}
	access := strings.TrimSpace(string(data[keyAccessKey]))
	secret := strings.TrimSpace(string(data[keySecretKey]))
	if access == "" || secret == "" {
		return nil, fmt.Errorf("s3 storage %q: missing %q or %q", name, keyAccessKey, keySecretKey)
	}

	region := strings.TrimSpace(string(data[keyRegion]))
	if region == "" {
		region = "us-east-1" // safe default; ignored by most non-AWS providers
	}
	endpoint := strings.TrimSpace(string(data[keyEndpoint]))
	pathStyle := strings.EqualFold(string(data[keyPathStyle]), "true")

	cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(access, secret, "")),
	)
	if err != nil {
		return nil, fmt.Errorf("s3 storage %q: load config: %w", name, err)
	}

	client := awss3.NewFromConfig(cfg, func(o *awss3.Options) {
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
		o.UsePathStyle = pathStyle
	})

	return &s3Storage{
		name:       name,
		bucket:     bucket,
		pathPrefix: strings.TrimRight(string(data[keyPrefix]), "/"),
		client:     client,
		uploader:   manager.NewUploader(client),
		logger:     logger,
	}, nil
}

func (s *s3Storage) Name() string { return s.name }

func (s *s3Storage) full(p string) string {
	p = strings.TrimLeft(p, "/")
	if s.pathPrefix == "" {
		return p
	}
	return strings.TrimLeft(path.Join(s.pathPrefix, p), "/")
}

func (s *s3Storage) stripPrefix(key string) string {
	if s.pathPrefix == "" {
		return key
	}
	rel := strings.TrimPrefix(key, s.pathPrefix)
	return strings.TrimLeft(rel, "/")
}

func (s *s3Storage) Upload(ctx context.Context, p string, r io.Reader) error {
	_, err := s.uploader.Upload(ctx, &awss3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.full(p)),
		Body:   r,
	})
	if err != nil {
		return fmt.Errorf("s3 upload %s: %w", p, err)
	}
	return nil
}

func (s *s3Storage) List(ctx context.Context, prefix string) ([]storage.Object, error) {
	full := s.full(prefix)
	var out []storage.Object
	var token *string
	for {
		resp, err := s.client.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			Prefix:            aws.String(full),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, fmt.Errorf("s3 list %s: %w", full, err)
		}
		for _, obj := range resp.Contents {
			out = append(out, storage.Object{
				Path:         s.stripPrefix(aws.ToString(obj.Key)),
				Size:         aws.ToInt64(obj.Size),
				LastModified: aws.ToTime(obj.LastModified),
			})
		}
		if !aws.ToBool(resp.IsTruncated) {
			break
		}
		token = resp.NextContinuationToken
	}
	return out, nil
}

func (s *s3Storage) Get(ctx context.Context, p string) (io.ReadCloser, error) {
	resp, err := s.client.GetObject(ctx, &awss3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.full(p)),
	})
	if err != nil {
		return nil, fmt.Errorf("s3 get %s: %w", p, err)
	}
	return resp.Body, nil
}

func (s *s3Storage) Delete(ctx context.Context, p string) error {
	_, err := s.client.DeleteObject(ctx, &awss3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.full(p)),
	})
	if err != nil {
		return fmt.Errorf("s3 delete %s: %w", p, err)
	}
	return nil
}

