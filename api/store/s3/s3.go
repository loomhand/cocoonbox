// Package s3 implements store.Store on any S3-compatible service.
package s3

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/CMGS/cocoonbox/api/store"
)

const defaultPresignTTL = time.Hour

// Options configures the bucket and credentials.
type Options struct {
	// Endpoint is host:port the api reaches the service on.
	Endpoint string
	// PublicEndpoint is host:port box agents reach the service on; empty uses Endpoint.
	PublicEndpoint string
	Secure         bool
	Region         string
	Bucket         string
	Prefix         string
	AccessKey      string
	SecretKey      string
	PresignTTL     time.Duration
}

var _ store.Store = (*S3)(nil)

// S3 is a store.Store on one bucket and prefix.
type S3 struct {
	opts      Options
	client    *minio.Client
	presigner *minio.Client
}

// New builds clients for opts; it makes no network call.
func New(opts Options) (*S3, error) {
	if opts.Bucket == "" || opts.Region == "" {
		return nil, errors.New("s3 bucket and region must be set")
	}
	opts.PresignTTL = cmp.Or(opts.PresignTTL, defaultPresignTTL)
	client, err := newClient(opts.Endpoint, opts)
	if err != nil {
		return nil, err
	}
	presigner, err := newClient(cmp.Or(opts.PublicEndpoint, opts.Endpoint), opts)
	if err != nil {
		return nil, err
	}
	return &S3{opts: opts, client: client, presigner: presigner}, nil
}

func (s *S3) PresignPut(ctx context.Context, key string) (store.Presigned, error) {
	u, err := s.presigner.PresignedPutObject(ctx, s.opts.Bucket, s.object(key), s.opts.PresignTTL)
	if err != nil {
		return store.Presigned{}, fmt.Errorf("presign put %s: %w", key, err)
	}
	return store.Presigned{URL: u.String()}, nil
}

func (s *S3) PresignGet(ctx context.Context, key string) (store.Presigned, error) {
	u, err := s.presigner.PresignedGetObject(ctx, s.opts.Bucket, s.object(key), s.opts.PresignTTL, nil)
	if err != nil {
		return store.Presigned{}, fmt.Errorf("presign get %s: %w", key, err)
	}
	return store.Presigned{URL: u.String()}, nil
}

func (s *S3) Get(ctx context.Context, key string) ([]byte, error) {
	obj, err := s.client.GetObject(ctx, s.opts.Bucket, s.object(key), minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", key, err)
	}
	defer func() { _ = obj.Close() }()
	data, err := io.ReadAll(obj)
	if minio.ToErrorResponse(err).StatusCode == http.StatusNotFound {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", key, err)
	}
	return data, nil
}

func (s *S3) Put(ctx context.Context, key string, data []byte) error {
	_, err := s.client.PutObject(ctx, s.opts.Bucket, s.object(key), bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: "application/octet-stream"})
	if err != nil {
		return fmt.Errorf("put %s: %w", key, err)
	}
	return nil
}

func (s *S3) object(key string) string {
	return path.Join(s.opts.Prefix, key)
}

func newClient(endpoint string, opts Options) (*minio.Client, error) {
	client, err := minio.New(endpoint, &minio.Options{
		Creds:        credentials.NewStaticV4(opts.AccessKey, opts.SecretKey, ""),
		Secure:       opts.Secure,
		Region:       opts.Region,
		BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		return nil, fmt.Errorf("s3 client for %s: %w", endpoint, err)
	}
	return client, nil
}
