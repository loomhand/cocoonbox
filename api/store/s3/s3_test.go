package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"

	"github.com/loomhand/cocoonbox/api/store"
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
			`<Contents><Key>state/cb-a/chunks/c1</Key><Size>5</Size></Contents><Contents><Key>state/cb-a/head.json</Key><Size>1</Size></Contents></ListBucketResult>`))
	}))
	t.Cleanup(srv.Close)
	s, err := New(Options{Endpoint: strings.TrimPrefix(srv.URL, "http://"), Region: "us-east-1", Bucket: "cocoonbox", Prefix: "state", AccessKey: "ak", SecretKey: "sk"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	objects, err := s.List(t.Context(), "cb-a/")
	if want := []store.Object{{Key: "cb-a/chunks/c1", Size: 5}, {Key: "cb-a/head.json", Size: 1}}; err != nil || listPrefix != "state/cb-a/" || !slices.Equal(objects, want) {
		t.Errorf("List = %v, %v (prefix %q)", objects, err, listPrefix)
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

func TestDeleteSendsKeysUnderThePrefixAndReportsFailures(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || !r.URL.Query().Has("delete") {
			t.Errorf("request %s %s, want POST ?delete", r.Method, r.URL)
		}
		body = string(raw)
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<DeleteResult><Deleted><Key>state/alice/chunks/c1</Key></Deleted>` +
			`<Error><Key>state/alice/chunks/c2</Key><Code>AccessDenied</Code><Message>denied</Message></Error></DeleteResult>`))
	}))
	t.Cleanup(srv.Close)
	s, err := New(Options{Endpoint: strings.TrimPrefix(srv.URL, "http://"), Region: "us-east-1", Bucket: "cocoonbox", Prefix: "state", AccessKey: "ak", SecretKey: "sk"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = s.Delete(t.Context(), []string{"alice/chunks/c1", "alice/chunks/c2"})
	if err == nil || !strings.Contains(err.Error(), "delete alice/chunks/c2") || strings.Contains(err.Error(), "c1") {
		t.Errorf("Delete = %v, want only the c2 failure", err)
	}
	if !strings.Contains(body, "<Key>state/alice/chunks/c1</Key>") || !strings.Contains(body, "<Key>state/alice/chunks/c2</Key>") {
		t.Errorf("delete body = %s", body)
	}
	if err := s.Delete(t.Context(), []string{"alice/../bob/chunks/c1"}); err == nil {
		t.Error("Delete outside the prefix accepted")
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

func TestListAndDeleteOnMinIO(t *testing.T) {
	raw := os.Getenv("COCOONBOX_TEST_S3")
	if raw == "" {
		t.Skip("COCOONBOX_TEST_S3 names no MinIO server (http://ACCESS:SECRET@HOST:PORT)")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse COCOONBOX_TEST_S3: %v", err)
	}
	secret, _ := u.User.Password()
	bucket := "cb-test-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	s, err := New(Options{Endpoint: u.Host, Region: "us-east-1", Bucket: bucket, Prefix: "state", AccessKey: u.User.Username(), SecretKey: secret})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.client.MakeBucket(t.Context(), bucket, minio.MakeBucketOptions{Region: "us-east-1"}); err != nil {
		t.Fatalf("make bucket: %v", err)
	}
	t.Cleanup(func() {
		for info := range s.client.ListObjectsIter(context.Background(), bucket, minio.ListObjectsOptions{Recursive: true}) {
			_ = s.client.RemoveObject(context.Background(), bucket, info.Key, minio.RemoveObjectOptions{})
		}
		_ = s.client.RemoveBucket(context.Background(), bucket)
	})
	const n = 1100
	keys := make([]string, n)
	for i := range n {
		keys[i] = fmt.Sprintf("alice/chunks/%04d", i)
		if err := s.Put(t.Context(), keys[i], []byte(strings.Repeat("x", i%7))); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if err := s.Put(t.Context(), "bob/chunks/0000", []byte("b")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	objects, err := s.List(t.Context(), "alice/chunks/")
	if err != nil || len(objects) != n || objects[3] != (store.Object{Key: "alice/chunks/0003", Size: 3}) {
		t.Fatalf("List = %d objects (%v), %v", len(objects), objects[:min(len(objects), 4)], err)
	}
	if err := s.Delete(t.Context(), append(keys[1:], "alice/chunks/missing")); err != nil {
		t.Fatalf("Delete of %d keys: %v", n, err)
	}
	if objects, err = s.List(t.Context(), "alice/"); err != nil || !slices.Equal(objects, []store.Object{{Key: "alice/chunks/0000", Size: 0}}) {
		t.Errorf("after Delete, List = %v, %v", objects, err)
	}
	if objects, err = s.List(t.Context(), "bob/"); err != nil || len(objects) != 1 {
		t.Errorf("bob's objects after alice's delete = %v, %v", objects, err)
	}
}
