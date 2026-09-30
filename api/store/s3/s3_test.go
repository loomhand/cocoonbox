package s3

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/CMGS/cocoonbox/api/store"
)

func TestPresignUsesPublicEndpointAndPrefix(t *testing.T) {
	s, err := New(Options{
		Endpoint: "127.0.0.1:9000", PublicEndpoint: "10.203.0.1:9000", Region: "us-east-1",
		Bucket: "cocoonbox", Prefix: "state", AccessKey: "ak", SecretKey: "sk",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	put, err := s.PresignPut(t.Context(), "cb-2bd806c97f0e/chunks/abc")
	if err != nil {
		t.Fatalf("PresignPut: %v", err)
	}
	u, err := url.Parse(put.URL)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if u.Host != "10.203.0.1:9000" || u.Path != "/cocoonbox/state/cb-2bd806c97f0e/chunks/abc" || u.Query().Get("X-Amz-Signature") == "" {
		t.Errorf("presigned put = %s", put.URL)
	}
	if u.Query().Get("X-Amz-Expires") != "3600" {
		t.Errorf("expires = %s, want 3600", u.Query().Get("X-Amz-Expires"))
	}
	get, err := s.PresignGet(t.Context(), "cb-2bd806c97f0e/manifests/s1")
	if err != nil || !strings.Contains(get.URL, "/cocoonbox/state/cb-2bd806c97f0e/manifests/s1?") {
		t.Errorf("presigned get = %s, %v", get.URL, err)
	}
}

func TestGetMapsMissingObject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>NoSuchKey</Code><Message>missing</Message></Error>`))
	}))
	t.Cleanup(srv.Close)
	s, err := New(Options{Endpoint: strings.TrimPrefix(srv.URL, "http://"), Region: "us-east-1", Bucket: "cocoonbox", AccessKey: "ak", SecretKey: "sk"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.Get(t.Context(), "cb-2bd806c97f0e/head.json"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get: %v, want ErrNotFound", err)
	}
}
