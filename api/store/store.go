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

// Store is implemented by every object store backend; keys are relative to the backend's prefix.
type Store interface {
	PresignPut(ctx context.Context, key string) (Presigned, error)
	PresignGet(ctx context.Context, key string) (Presigned, error)
	Get(ctx context.Context, key string) ([]byte, error)
	Put(ctx context.Context, key string, data []byte) error
}
