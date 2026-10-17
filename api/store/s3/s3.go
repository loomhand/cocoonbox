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
	"slices"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/s3utils"
	"golang.org/x/sync/errgroup"

	"github.com/loomhand/cocoonbox/api/store"
)

const (
	defaultPresignTTL = 10 * time.Minute
	deleteConcurrency = 32
)

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

var (
	_ store.Store     = (*S3)(nil)
	_ store.Versioned = (*S3)(nil)
)

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
	object, err := s.object(key)
	if err != nil {
		return store.Presigned{}, err
	}
	u, err := s.presigner.PresignedPutObject(ctx, s.opts.Bucket, object, s.opts.PresignTTL)
	if err != nil {
		return store.Presigned{}, fmt.Errorf("presign put %s: %w", key, err)
	}
	return store.Presigned{URL: u.String()}, nil
}

func (s *S3) PresignGet(ctx context.Context, key string) (store.Presigned, error) {
	object, err := s.object(key)
	if err != nil {
		return store.Presigned{}, err
	}
	u, err := s.presigner.PresignedGetObject(ctx, s.opts.Bucket, object, s.opts.PresignTTL, nil)
	if err != nil {
		return store.Presigned{}, fmt.Errorf("presign get %s: %w", key, err)
	}
	return store.Presigned{URL: u.String()}, nil
}

func (s *S3) Get(ctx context.Context, key string) ([]byte, error) {
	data, _, err := s.GetVersion(ctx, key)
	return data, err
}

func (s *S3) Put(ctx context.Context, key string, data []byte) error {
	return s.put(ctx, key, data, minio.PutObjectOptions{})
}

func (s *S3) GetVersion(ctx context.Context, key string) ([]byte, string, error) {
	object, err := s.object(key)
	if err != nil {
		return nil, "", err
	}
	obj, err := s.client.GetObject(ctx, s.opts.Bucket, object, minio.GetObjectOptions{})
	if err != nil {
		return nil, "", fmt.Errorf("get %s: %w", key, err)
	}
	defer func() { _ = obj.Close() }()
	data, err := io.ReadAll(obj)
	if minio.ToErrorResponse(err).StatusCode == http.StatusNotFound {
		return nil, "", store.ErrNotFound
	}
	if err != nil {
		return nil, "", fmt.Errorf("read %s: %w", key, err)
	}
	// After a read, Stat answers from the same GET response, so the version matches data.
	info, err := obj.Stat()
	if err != nil {
		return nil, "", fmt.Errorf("stat %s: %w", key, err)
	}
	return data, info.ETag, nil
}

func (s *S3) PutIf(ctx context.Context, key string, data []byte, version string) error {
	var opts minio.PutObjectOptions
	if version == "" {
		opts.SetMatchETagExcept("*")
	} else {
		opts.SetMatchETag(version)
	}
	return s.put(ctx, key, data, opts)
}

func (s *S3) List(ctx context.Context, prefix string) ([]store.Object, error) {
	dir, err := s.object(strings.TrimSuffix(prefix, "/"))
	if err != nil {
		return nil, err
	}
	base := s.base()
	var objects []store.Object
	for info := range s.client.ListObjectsIter(ctx, s.opts.Bucket, minio.ListObjectsOptions{Prefix: dir + "/", Recursive: true}) {
		if info.Err != nil {
			return nil, fmt.Errorf("list %s: %w", prefix, info.Err)
		}
		objects = append(objects, store.Object{Key: strings.TrimPrefix(info.Key, base), Size: info.Size})
	}
	return objects, nil
}

func (s *S3) Delete(ctx context.Context, keys []string) error {
	objects := make([]minio.ObjectInfo, 0, len(keys))
	for _, key := range keys {
		object, err := s.object(key)
		if err != nil {
			return err
		}
		objects = append(objects, minio.ObjectInfo{Key: object})
	}
	if s3utils.IsGoogleEndpoint(*s.client.EndpointURL()) {
		return s.deleteEach(ctx, objects)
	}
	results, err := s.client.RemoveObjectsWithIter(ctx, s.opts.Bucket, slices.Values(objects), minio.RemoveObjectsOptions{})
	if err != nil {
		return fmt.Errorf("delete %d objects: %w", len(keys), err)
	}
	var errs []error
	for res := range results {
		if res.Err != nil {
			errs = append(errs, fmt.Errorf("delete %s: %w", strings.TrimPrefix(res.ObjectName, s.base()), res.Err))
		}
	}
	return errors.Join(append(errs, ctx.Err())...)
}

// Copy copies src to dst inside the service, without moving the data through the api.
func (s *S3) Copy(ctx context.Context, src, dst string) error {
	from, err := s.object(src)
	if err != nil {
		return err
	}
	to, err := s.object(dst)
	if err != nil {
		return err
	}
	if _, err := s.client.CopyObject(ctx, minio.CopyDestOptions{Bucket: s.opts.Bucket, Object: to}, minio.CopySrcOptions{Bucket: s.opts.Bucket, Object: from}); err != nil {
		return fmt.Errorf("copy %s to %s: %w", src, dst, err)
	}
	return nil
}

func (s *S3) put(ctx context.Context, key string, data []byte, opts minio.PutObjectOptions) error {
	object, err := s.object(key)
	if err != nil {
		return err
	}
	opts.ContentType = "application/octet-stream"
	_, err = s.client.PutObject(ctx, s.opts.Bucket, object, bytes.NewReader(data), int64(len(data)), opts)
	if minio.ToErrorResponse(err).StatusCode == http.StatusPreconditionFailed {
		err = store.ErrStale
	}
	if err != nil {
		return fmt.Errorf("put %s: %w", key, err)
	}
	return nil
}

func (s *S3) deleteEach(ctx context.Context, objects []minio.ObjectInfo) error {
	errs := make([]error, len(objects))
	var g errgroup.Group
	g.SetLimit(deleteConcurrency)
	for i, object := range objects {
		if ctx.Err() != nil {
			break
		}
		g.Go(func() error {
			err := s.client.RemoveObject(ctx, s.opts.Bucket, object.Key, minio.RemoveObjectOptions{})
			if err != nil && minio.ToErrorResponse(err).Code != minio.NoSuchKey {
				errs[i] = fmt.Errorf("delete %s: %w", strings.TrimPrefix(object.Key, s.base()), err)
			}
			return nil
		})
	}
	_ = g.Wait()
	return errors.Join(append(errs, ctx.Err())...)
}

func (s *S3) base() string {
	if s.opts.Prefix == "" {
		return ""
	}
	return path.Clean(s.opts.Prefix) + "/"
}

func (s *S3) object(key string) (string, error) {
	if key == "" || key == "." || key == ".." || strings.HasPrefix(key, "/") || strings.HasPrefix(key, "../") || path.Clean(key) != key {
		return "", fmt.Errorf("invalid object key %q", key)
	}
	return path.Join(s.opts.Prefix, key), nil
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
