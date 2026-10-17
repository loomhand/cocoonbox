// Package store defines the object store that holds user state.
package store

import (
	"context"
	"errors"
	"time"
)

var (
	// ErrNotFound reports a missing object.
	ErrNotFound = errors.New("object not found")
	// ErrStale reports a conditional write that lost to a write after the read.
	ErrStale = errors.New("object changed since it was read")
)

// Presigned is a URL a box agent uses without holding store credentials.
type Presigned struct {
	URL     string
	Headers map[string]string
}

// Object is one stored object as a listing returns it.
type Object struct {
	Key      string
	Size     int64
	Modified time.Time
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

// Versioned is implemented by a store that writes conditionally on the version a read returned.
type Versioned interface {
	// GetVersion returns the object and its version; a missing object is ErrNotFound with version "".
	GetVersion(ctx context.Context, key string) ([]byte, string, error)
	// PutIf writes data while the object still has version, or while it is missing for ""; else ErrStale.
	PutIf(ctx context.Context, key string, data []byte, version string) error
}
