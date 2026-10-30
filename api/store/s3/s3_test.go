package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

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

func TestAPresignedGetLastsUntilTheDeadlineUpToAnHourAndAPutKeepsTheTTL(t *testing.T) {
	s, err := New(Options{Endpoint: "127.0.0.1:9000", Region: "us-east-1", Bucket: "cocoonbox", AccessKey: "ak", SecretKey: "sk"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	expires := func(raw string) int {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		n, _ := strconv.Atoi(u.Query().Get("X-Amz-Expires"))
		return n
	}
	tests := []struct {
		name     string
		deadline time.Duration
		get      [2]int
	}{
		{name: "no deadline", get: [2]int{600, 600}},
		{name: "deadline sooner than the ttl", deadline: 5 * time.Minute, get: [2]int{600, 600}},
		{name: "deadline later than the ttl", deadline: 50 * time.Minute, get: [2]int{2990, 3000}},
		{name: "deadline past an hour", deadline: 3 * time.Hour, get: [2]int{3600, 3600}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			if tt.deadline > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tt.deadline)
				defer cancel()
			}
			get, err := s.PresignGet(ctx, "cb-2bd806c97f0e/chunks/abc")
			if err != nil {
				t.Fatalf("PresignGet: %v", err)
			}
			if n := expires(get.URL); n < tt.get[0] || n > tt.get[1] {
				t.Errorf("get expires = %d, want %d..%d", n, tt.get[0], tt.get[1])
			}
			put, err := s.PresignPut(ctx, "cb-2bd806c97f0e/chunks/abc")
			if err != nil {
				t.Fatalf("PresignPut: %v", err)
			}
			if n := expires(put.URL); n != 600 {
				t.Errorf("put expires = %d, want 600", n)
			}
		})
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

func TestDeleteOnGCSRunsBoundedParallelDeletes(t *testing.T) {
	const (
		n       = 320
		latency = 20 * time.Millisecond
	)
	var (
		mu       sync.Mutex
		deleted  = map[string]bool{}
		inFlight int
		peak     int
		requests atomic.Int32
		cancelAt atomic.Int32
	)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		if r.Method != http.MethodDelete {
			t.Errorf("request %s %s, want DELETE", r.Method, r.URL)
			w.WriteHeader(http.StatusNotImplemented)
			_, _ = w.Write([]byte(`<Error><Code>NotImplemented</Code><Message>multi-object delete</Message></Error>`))
			return
		}
		if requests.Add(1) == cancelAt.Load() {
			cancel()
		}
		mu.Lock()
		inFlight++
		peak = max(peak, inFlight)
		mu.Unlock()
		time.Sleep(latency)
		mu.Lock()
		inFlight--
		mu.Unlock()
		switch r.URL.Path {
		case "/cocoonbox/state/alice/chunks/missing":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`<Error><Code>NoSuchKey</Code><Message>missing</Message></Error>`))
		case "/cocoonbox/state/alice/chunks/denied":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`<Error><Code>AccessDenied</Code><Message>denied</Message></Error>`))
		default:
			mu.Lock()
			deleted[r.URL.Path] = true
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)
	s := newGCSStore(t, srv.Listener.Addr().String())
	keys := make([]string, n, n+2)
	for i := range n {
		keys[i] = fmt.Sprintf("alice/chunks/%04d", i)
	}
	start := time.Now()
	err := s.Delete(t.Context(), append(keys, "alice/chunks/missing", "alice/chunks/denied"))
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "delete alice/chunks/denied") || strings.Contains(err.Error(), "missing") || strings.Count(err.Error(), "delete ") != 1 {
		t.Errorf("Delete = %v, want only the denied failure", err)
	}
	if len(deleted) != n || !deleted["/cocoonbox/state/alice/chunks/0000"] || !deleted[fmt.Sprintf("/cocoonbox/state/alice/chunks/%04d", n-1)] {
		t.Errorf("deleted %d objects, want %d", len(deleted), n)
	}
	if peak > deleteConcurrency || peak < 2 {
		t.Errorf("peak in-flight deletes = %d, want 2..%d", peak, deleteConcurrency)
	}
	if serial := (n + 2) * latency; elapsed > serial/4 {
		t.Errorf("Delete took %v, serial is %v", elapsed, serial)
	}

	requests.Store(0)
	cancelAt.Store(40)
	if err := s.Delete(ctx, keys); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled Delete = %v, want context.Canceled", err)
	}
	if got := requests.Load(); got >= n {
		t.Errorf("cancelled Delete sent %d requests, want fewer than %d", got, n)
	}
}

func TestDeleteSendsBatchesInBoundedParallel(t *testing.T) {
	const (
		n       = (deleteConcurrency + 4) * deleteBatchSize
		latency = 50 * time.Millisecond
	)
	var (
		mu       sync.Mutex
		keys     int
		inFlight int
		peak     int
		largest  int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || !r.URL.Query().Has("delete") {
			t.Errorf("request %s %s, want POST ?delete", r.Method, r.URL)
		}
		batch := strings.Count(string(raw), "<Key>")
		mu.Lock()
		keys += batch
		largest = max(largest, batch)
		inFlight++
		peak = max(peak, inFlight)
		mu.Unlock()
		time.Sleep(latency)
		mu.Lock()
		inFlight--
		mu.Unlock()
		w.Header().Set("Content-Type", "application/xml")
		result := "<DeleteResult>"
		if strings.Contains(string(raw), "<Key>state/alice/chunks/denied</Key>") {
			result += "<Error><Key>state/alice/chunks/denied</Key><Code>AccessDenied</Code><Message>denied</Message></Error>"
		}
		_, _ = w.Write([]byte(result + "</DeleteResult>"))
	}))
	t.Cleanup(srv.Close)
	s, err := New(Options{Endpoint: strings.TrimPrefix(srv.URL, "http://"), Region: "us-east-1", Bucket: "cocoonbox", Prefix: "state", AccessKey: "ak", SecretKey: "sk"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	all := make([]string, n, n+1)
	for i := range n {
		all[i] = fmt.Sprintf("alice/chunks/%05d", i)
	}
	start := time.Now()
	err = s.Delete(t.Context(), append(all, "alice/chunks/denied"))
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "delete alice/chunks/denied") || strings.Count(err.Error(), "delete ") != 1 {
		t.Errorf("Delete = %v, want only the denied failure", err)
	}
	if keys != n+1 || largest != deleteBatchSize {
		t.Errorf("sent %d keys, at most %d per request; want %d, at most %d", keys, largest, n+1, deleteBatchSize)
	}
	if peak > deleteConcurrency || peak < 2 {
		t.Errorf("peak in-flight batches = %d, want 2..%d", peak, deleteConcurrency)
	}
	if serial := (deleteConcurrency + 5) * latency; elapsed > serial/4 {
		t.Errorf("Delete took %v, serial is %v", elapsed, serial)
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

func TestPutIfSendsTheVersionAndMapsAPreconditionFailure(t *testing.T) {
	var mu sync.Mutex
	etag, body := "", []byte(nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			if etag == "" {
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>NoSuchKey</Code><Message>missing</Message></Error>`))
				return
			}
			w.Header().Set("ETag", `"`+etag+`"`)
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
			_, _ = w.Write(body)
		case http.MethodPut:
			match, none := strings.Trim(r.Header.Get("If-Match"), `"`), r.Header.Get("If-None-Match")
			if (none == "*" && etag != "") || (match != "" && match != etag) {
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(http.StatusPreconditionFailed)
				_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>PreconditionFailed</Code><Message>stale</Message></Error>`))
				return
			}
			raw, _ := io.ReadAll(r.Body)
			size, _ := strconv.Atoi(r.Header.Get("X-Amz-Decoded-Content-Length"))
			_, chunk, _ := strings.Cut(string(raw), "\r\n")
			body = []byte(chunk[:size])
			etag = "v" + strconv.Itoa(len(body))
			w.Header().Set("ETag", `"`+etag+`"`)
		}
	}))
	t.Cleanup(srv.Close)
	s, err := New(Options{Endpoint: strings.TrimPrefix(srv.URL, "http://"), Region: "us-east-1", Bucket: "cocoonbox", AccessKey: "ak", SecretKey: "sk"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, v, err := s.GetVersion(t.Context(), "u/index.json"); !errors.Is(err, store.ErrNotFound) || v != "" {
		t.Fatalf("GetVersion of a missing object = %q, %v", v, err)
	}
	if err := s.PutIf(t.Context(), "u/index.json", []byte("a"), ""); err != nil {
		t.Fatalf("PutIf absent: %v", err)
	}
	if err := s.PutIf(t.Context(), "u/index.json", []byte("bb"), ""); !errors.Is(err, store.ErrStale) {
		t.Fatalf("PutIf absent over an object = %v, want ErrStale", err)
	}
	data, v, err := s.GetVersion(t.Context(), "u/index.json")
	if err != nil || string(data) != "a" || v != "v1" {
		t.Fatalf("GetVersion = %q %q %v", data, v, err)
	}
	if err := s.PutIf(t.Context(), "u/index.json", []byte("cc"), v); err != nil {
		t.Fatalf("PutIf current: %v", err)
	}
	if err := s.PutIf(t.Context(), "u/index.json", []byte("ddd"), v); !errors.Is(err, store.ErrStale) {
		t.Errorf("PutIf stale = %v, want ErrStale", err)
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

func newGCSStore(t *testing.T, addr string) *S3 {
	t.Helper()
	s, err := New(Options{Endpoint: addr, Region: "us-east-1", Bucket: "cocoonbox", Prefix: "state", AccessKey: "ak", SecretKey: "sk"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	dial := func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	s.client, err = minio.New("storage.googleapis.com", &minio.Options{
		Creds:        credentials.NewStaticV4("ak", "sk", ""),
		Region:       "us-east-1",
		BucketLookup: minio.BucketLookupPath,
		Transport:    &http.Transport{DialContext: dial},
	})
	if err != nil {
		t.Fatalf("gcs client: %v", err)
	}
	return s
}
