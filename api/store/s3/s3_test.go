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
	if u.Query().Get("X-Amz-Expires") != "600" {
		t.Errorf("expires = %s, want 600", u.Query().Get("X-Amz-Expires"))
	}
	get, err := s.PresignGet(t.Context(), "cb-2bd806c97f0e/manifests/s1")
	if err != nil || !strings.Contains(get.URL, "/cocoonbox/state/cb-2bd806c97f0e/manifests/s1?") {
		t.Errorf("presigned get = %s, %v", get.URL, err)
	}
}

func TestRefusesKeysOutsideTheirPath(t *testing.T) {
	s, err := New(Options{Endpoint: "127.0.0.1:9000", Region: "us-east-1", Bucket: "cocoonbox", Prefix: "state", AccessKey: "ak", SecretKey: "sk"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, key := range []string{"", ".", "..", "../cb-other/head.json", "cb-a/../cb-b/head.json", "cb-a/chunks/../../cb-b/x", "/cb-a/head.json", "cb-a//head.json", "cb-a/head.json/.."} {
		if _, err := s.PresignPut(t.Context(), key); err == nil {
			t.Errorf("PresignPut(%q) accepted", key)
		}
		if err := s.Put(t.Context(), key, nil); err == nil {
			t.Errorf("Put(%q) accepted", key)
		}
	}
}

func TestListAndCopyStayUnderThePrefix(t *testing.T) {
	var copySource, copyPath, listPrefix string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		if r.Method == http.MethodPut {
			copySource, copyPath = r.Header.Get("X-Amz-Copy-Source"), r.URL.Path
			_, _ = w.Write([]byte(`<CopyObjectResult><ETag>"e"</ETag><LastModified>2026-10-02T00:00:00.000Z</LastModified></CopyObjectResult>`))
			return
		}
		listPrefix = r.URL.Query().Get("prefix")
		_, _ = w.Write([]byte(`<ListBucketResult><Name>cocoonbox</Name><Prefix>state/cb-a/</Prefix><KeyCount>2</KeyCount><MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated>` +
			`<Contents><Key>state/cb-a/chunks/c1</Key><Size>1</Size></Contents><Contents><Key>state/cb-a/head.json</Key><Size>1</Size></Contents></ListBucketResult>`))
	}))
	t.Cleanup(srv.Close)
	s, err := New(Options{Endpoint: strings.TrimPrefix(srv.URL, "http://"), Region: "us-east-1", Bucket: "cocoonbox", Prefix: "state", AccessKey: "ak", SecretKey: "sk"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	keys, err := s.List(t.Context(), "cb-a/")
	if err != nil || listPrefix != "state/cb-a/" || strings.Join(keys, ",") != "cb-a/chunks/c1,cb-a/head.json" {
		t.Errorf("List = %v, %v (prefix %q)", keys, err, listPrefix)
	}
	if err := s.Copy(t.Context(), "cb-a/chunks/c1", "alice/chunks/c1"); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if copySource != "cocoonbox/state/cb-a/chunks/c1" || copyPath != "/cocoonbox/state/alice/chunks/c1" {
		t.Errorf("copy from %q to %q", copySource, copyPath)
	}
	if _, err := s.List(t.Context(), "../cb-b/"); err == nil {
		t.Error("List outside the prefix accepted")
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
