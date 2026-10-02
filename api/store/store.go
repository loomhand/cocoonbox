// Package store defines the object store that holds user state.
package store

import (
	"context"
	"errors"
)

// ErrNotFound reports a missing object.
var ErrNotFound = errors.New("object not found")

// Presigned is a URL a box agent uses without holding store credentials.
type Presigned struct {
	URL     string
	Headers map[string]string
}

// Object is one stored object as a listing returns it.
type Object struct {
	Key  string
	Size int64
}

// Store is implemented by every object store backend; keys are relative to the backend's prefix.
type Store interface {
	PresignPut(ctx context.Context, key string) (Presigned, error)
	PresignGet(ctx context.Context, key string) (Presigned, error)
	Get(ctx context.Context, key string) ([]byte, error)
	Put(ctx context.Context, key string, data []byte) error
	// List returns the objects under prefix, which ends in a slash.
	List(ctx context.Context, prefix string) ([]Object, error)
	// Delete removes keys; a missing key is not an error.
	Delete(ctx context.Context, keys []string) error
}
