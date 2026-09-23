// SPDX-License-Identifier: BSD-3-Clause

package s3

import (
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fat32 "github.com/go-filesystems/fat32"
	filesystem "github.com/go-filesystems/interface"
	"github.com/go-volumes/s3/sigv4"
)

// The tests drive a REAL driver and a REAL client signature.
//
// A fake Filesystem would have let the FileType()/Mode() confusion through:
// fat32 reports the FAT attribute byte from DirEntry.FileType and a proper
// Unix mode from Stat, and a mock would have reported whatever the mock author
// believed. Likewise the signature is produced by the same package a client
// uses, not asserted against a string this test wrote.

const (
	testKeyID  = "AKIAIOSFODNN7EXAMPLE"
	testSecret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
)

func testFS(t *testing.T) filesystem.Filesystem {
	t.Helper()
	img := filepath.Join(t.TempDir(), "fs.img")
	fsys, err := fat32.Format(img, 16<<20, fat32.FormatConfig{Label: "S3TEST"})
	if err != nil {
		t.Fatalf("formatting: %v", err)
	}
	mk := func(dir string) {
		if err := fsys.MkDir(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	wr := func(p, body string) {
		if err := fsys.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	mk("/photos")
	mk("/photos/2026")
	mk("/scratch")
	wr("/photos/a.txt", "alpha")
	wr("/photos/2026/b.txt", "bravo")
	wr("/scratch/c.txt", "charlie")
	// A FILE at the root is not a bucket, and the listing must not show one.
	wr("/loose.txt", "loose")
	t.Cleanup(func() { _ = fsys.Close() })
	return fsys
}

func testServer(t *testing.T) *Server {
	t.Helper()
	srv, err := New(testFS(t), func(id string) (string, bool) {
		if id == testKeyID {
			return testSecret, true
		}
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	srv.Clock = func() time.Time { return time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC) }
	return srv
}

// signed builds a request signed the way a client signs it.
func signed(t *testing.T, srv *Server, method, target string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(method, target, nil)
	r.Host = "s3.example.org"
	payload := sigv4.HashSHA256(nil)
	r.Header.Set("x-amz-content-sha256", payload)
	s := sigv4.New("eu-west-1", "s3", sigv4.Credentials{
		AccessKeyID: testKeyID, SecretAccessKey: testSecret,
	})
	s.SignRaw(r, payload, srv.now())
	return r
}

func do(t *testing.T, srv *Server, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	return w
}

func TestListBuckets_TopLevelDirectoriesOnly(t *testing.T) {
	srv := testServer(t)
	w := do(t, srv, signed(t, srv, http.MethodGet, "http://s3.example.org/"))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var got listAllMyBucketsResult
	if err := xml.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("parsing: %v\n%s", err, w.Body.String())
	}
	var names []string
	for _, b := range got.Buckets {
		names = append(names, b.Name)
	}
	want := []string{"photos", "scratch"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("buckets = %v, want %v", names, want)
	}
	// The control that catches the FileType/Mode confusion: if directory
	// detection were wrong, this list would be empty or would contain the
	// loose file.
	for _, n := range names {
		if n == "loose.txt" {
			t.Error("a file at the root was listed as a bucket")
		}
	}
}

func TestListObjects_PrefixAndDelimiter(t *testing.T) {
	srv := testServer(t)
	w := do(t, srv, signed(t, srv, http.MethodGet,
		"http://s3.example.org/photos?list-type=2&delimiter=%2F"))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var got listBucketResult
	if err := xml.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("parsing: %v\n%s", err, w.Body.String())
	}
	if len(got.Contents) != 1 || got.Contents[0].Key != "a.txt" {
		t.Errorf("contents = %+v, want just a.txt", got.Contents)
	}
	if len(got.CommonPrefixes) != 1 || got.CommonPrefixes[0].Prefix != "2026/" {
		t.Errorf("prefixes = %+v, want [2026/]", got.CommonPrefixes)
	}
	// KeyCount counts prefixes too -- a client paging on it stops early
	// otherwise.
	if got.KeyCount != 2 {
		t.Errorf("KeyCount = %d, want 2 (one key and one prefix)", got.KeyCount)
	}
}

func TestListObjects_WithoutDelimiterIsFlat(t *testing.T) {
	srv := testServer(t)
	w := do(t, srv, signed(t, srv, http.MethodGet, "http://s3.example.org/photos?list-type=2"))
	var got listBucketResult
	if err := xml.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("parsing: %v", err)
	}
	var keys []string
	for _, c := range got.Contents {
		keys = append(keys, c.Key)
	}
	want := "2026/b.txt,a.txt"
	if strings.Join(keys, ",") != want {
		t.Errorf("keys = %v, want %s", keys, want)
	}
}

func TestGetObject_BodyAndETagMatchTheBytes(t *testing.T) {
	srv := testServer(t)
	w := do(t, srv, signed(t, srv, http.MethodGet, "http://s3.example.org/photos/a.txt"))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if w.Body.String() != "alpha" {
		t.Errorf("body = %q", w.Body.String())
	}
	if got, want := w.Header().Get("ETag"), etagOf([]byte("alpha")); got != want {
		t.Errorf("ETag = %s, want %s (the MD5 clients verify against)", got, want)
	}
}

func TestGetObject_Range(t *testing.T) {
	srv := testServer(t)
	for _, tc := range []struct {
		hdr, want, contentRange string
		status                  int
	}{
		{"bytes=0-1", "al", "bytes 0-1/5", http.StatusPartialContent},
		{"bytes=2-", "pha", "bytes 2-4/5", http.StatusPartialContent},
		{"bytes=-2", "ha", "bytes 3-4/5", http.StatusPartialContent},
	} {
		r := signed(t, srv, http.MethodGet, "http://s3.example.org/photos/a.txt")
		r.Header.Set("Range", tc.hdr)
		w := do(t, srv, r)
		if w.Code != tc.status {
			t.Errorf("%s: status %d, want %d", tc.hdr, w.Code, tc.status)
			continue
		}
		if w.Body.String() != tc.want {
			t.Errorf("%s: body %q, want %q", tc.hdr, w.Body.String(), tc.want)
		}
		if got := w.Header().Get("Content-Range"); got != tc.contentRange {
			t.Errorf("%s: Content-Range %q, want %q", tc.hdr, got, tc.contentRange)
		}
	}
}

// A 416 must carry the real size, or a client cannot tell "past the end" from
// "the server is confused" and retries the same request.
func TestGetObject_UnsatisfiableRangeSaysTheSize(t *testing.T) {
	srv := testServer(t)
	r := signed(t, srv, http.MethodGet, "http://s3.example.org/photos/a.txt")
	r.Header.Set("Range", "bytes=99-")
	w := do(t, srv, r)
	if w.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("status %d", w.Code)
	}
	if got := w.Header().Get("Content-Range"); got != "bytes */5" {
		t.Errorf("Content-Range = %q, want bytes */5", got)
	}
}

func TestHeadObject_HasNoBody(t *testing.T) {
	srv := testServer(t)
	w := do(t, srv, signed(t, srv, http.MethodHead, "http://s3.example.org/photos/a.txt"))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("HEAD returned a body of %d bytes", w.Body.Len())
	}
	if got := w.Header().Get("Content-Length"); got != "5" {
		t.Errorf("Content-Length = %q, want 5", got)
	}
}

// A directory is a prefix, not an object.
func TestGetObject_ADirectoryIsNotAnObject(t *testing.T) {
	srv := testServer(t)
	w := do(t, srv, signed(t, srv, http.MethodGet, "http://s3.example.org/photos/2026"))
	if w.Code != http.StatusNotFound {
		t.Errorf("status %d, want 404: %s", w.Code, w.Body.String())
	}
}

func TestMissing_BucketAndKeyAreDifferentErrors(t *testing.T) {
	srv := testServer(t)
	for _, tc := range []struct{ target, code string }{
		{"http://s3.example.org/nope?list-type=2", "NoSuchBucket"},
		{"http://s3.example.org/photos/nope.txt", "NoSuchKey"},
	} {
		w := do(t, srv, signed(t, srv, http.MethodGet, tc.target))
		var e errorResponse
		if err := xml.Unmarshal(w.Body.Bytes(), &e); err != nil {
			t.Fatalf("%s: parsing: %v\n%s", tc.target, err, w.Body.String())
		}
		if e.Code != tc.code {
			t.Errorf("%s: code %q, want %q", tc.target, e.Code, tc.code)
		}
	}
}

// ⛔⛔ The traversal case. "../.." is a legal S3 key.
func TestKeyCannotEscapeItsBucket(t *testing.T) {
	srv := testServer(t)
	for _, key := range []string{
		"../scratch/c.txt",
		"..%2Fscratch%2Fc.txt",
		"2026/../../scratch/c.txt",
	} {
		w := do(t, srv, signed(t, srv, http.MethodGet, "http://s3.example.org/photos/"+key))
		if w.Code == http.StatusOK && strings.Contains(w.Body.String(), "charlie") {
			t.Errorf("key %q escaped its bucket and read another one", key)
		}
	}
}

func TestUnsigned_IsRefused(t *testing.T) {
	srv := testServer(t)
	w := do(t, srv, httptest.NewRequest(http.MethodGet, "http://s3.example.org/", nil))
	if w.Code != http.StatusForbidden {
		t.Errorf("an unsigned request got %d, want 403", w.Code)
	}
}

func TestWrongSecret_IsRefused(t *testing.T) {
	srv := testServer(t)
	r := httptest.NewRequest(http.MethodGet, "http://s3.example.org/", nil)
	r.Host = "s3.example.org"
	payload := sigv4.HashSHA256(nil)
	r.Header.Set("x-amz-content-sha256", payload)
	sigv4.New("eu-west-1", "s3", sigv4.Credentials{
		AccessKeyID: testKeyID, SecretAccessKey: "not-the-secret",
	}).SignRaw(r, payload, srv.now())

	w := do(t, srv, r)
	var e errorResponse
	_ = xml.Unmarshal(w.Body.Bytes(), &e)
	if e.Code != "SignatureDoesNotMatch" {
		t.Errorf("code = %q, want SignatureDoesNotMatch (body %s)", e.Code, w.Body.String())
	}
}

func TestUnknownAccessKey_IsRefused(t *testing.T) {
	srv := testServer(t)
	r := httptest.NewRequest(http.MethodGet, "http://s3.example.org/", nil)
	r.Host = "s3.example.org"
	payload := sigv4.HashSHA256(nil)
	r.Header.Set("x-amz-content-sha256", payload)
	sigv4.New("eu-west-1", "s3", sigv4.Credentials{
		AccessKeyID: "AKIANOBODY", SecretAccessKey: testSecret,
	}).SignRaw(r, payload, srv.now())

	w := do(t, srv, r)
	var e errorResponse
	_ = xml.Unmarshal(w.Body.Bytes(), &e)
	if e.Code != "InvalidAccessKeyId" {
		t.Errorf("code = %q, want InvalidAccessKeyId", e.Code)
	}
}

// Replay protection. Without it a captured Authorization header is valid for
// ever.
func TestAnOldSignature_IsRefused(t *testing.T) {
	srv := testServer(t)
	old := srv.now().Add(-2 * time.Hour)
	r := httptest.NewRequest(http.MethodGet, "http://s3.example.org/", nil)
	r.Host = "s3.example.org"
	payload := sigv4.HashSHA256(nil)
	r.Header.Set("x-amz-content-sha256", payload)
	sigv4.New("eu-west-1", "s3", sigv4.Credentials{
		AccessKeyID: testKeyID, SecretAccessKey: testSecret,
	}).SignRaw(r, payload, old)

	w := do(t, srv, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("a two-hour-old signature got %d, want 403", w.Code)
	}
}

// Read-only is the default, and it is what keeps an accidental sync from
// writing into a forensic image.
func TestWritesAreRefusedByDefault(t *testing.T) {
	srv := testServer(t)
	for _, m := range []string{http.MethodPut, http.MethodDelete} {
		w := do(t, srv, signed(t, srv, m, "http://s3.example.org/photos/new.txt"))
		if w.Code != http.StatusForbidden {
			t.Errorf("%s got %d, want 403", m, w.Code)
		}
	}
}

func TestNew_RefusesAServerThatCannotCheckASignature(t *testing.T) {
	if _, err := New(testFS(t), nil); err == nil {
		t.Error("New accepted a nil CredentialLookup; that is an open export")
	}
}

// The timestamp format: three decimals and a literal Z, not RFC3339Nano.
func TestS3Time_HasThreeDecimals(t *testing.T) {
	got := s3Time(time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC))
	if got != "2026-09-23T10:00:00.000Z" {
		t.Errorf("s3Time = %q, want 2026-09-23T10:00:00.000Z", got)
	}
}

// The listing must carry the namespace, or several SDKs parse an empty result
// and report an empty bucket.
func TestListingCarriesTheXMLNamespace(t *testing.T) {
	srv := testServer(t)
	w := do(t, srv, signed(t, srv, http.MethodGet, "http://s3.example.org/photos?list-type=2"))
	if !strings.Contains(w.Body.String(), s3NS) {
		t.Errorf("the listing has no xmlns:\n%s", w.Body.String())
	}
}

var _ = io.Discard
