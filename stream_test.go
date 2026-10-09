// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package s3

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	filesystem "github.com/go-filesystems/interface"
	"github.com/go-filesystems/osfs"
)

// hostServer serves a directory of the host through osfs, with one bucket
// holding one object of the given bytes.
func hostServer(t *testing.T, body []byte, wrap func(filesystem.Filesystem) filesystem.Filesystem) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "b", "o.bin")
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatal(err)
	}
	fs, err := osfs.Open(dir, osfs.ReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fs.Close() })
	var fsys filesystem.Filesystem = fs
	if wrap != nil {
		fsys = wrap(fs)
	}
	srv, err := New(fsys, func(id string) (string, bool) { return testSecret, id == testKeyID })
	if err != nil {
		t.Fatal(err)
	}
	srv.Clock = func() time.Time { return time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC) }
	return srv, p
}

// countingFS counts the bytes read through ReadAt and the whole-file reads.
type countingFS struct {
	filesystem.Filesystem
	readAt, readFile atomic.Int64
	openErr          error
	nilFile          bool
}

func (c *countingFS) ReadFile(p string) ([]byte, error) {
	c.readFile.Add(1)
	return c.Filesystem.ReadFile(p)
}

func (c *countingFS) OpenFile(p string) (filesystem.File, error) {
	if c.openErr != nil {
		return nil, c.openErr
	}
	if c.nilFile {
		return nil, nil
	}
	f, err := c.Filesystem.(filesystem.Opener).OpenFile(p)
	if err != nil {
		return nil, err
	}
	return countingFile{f, &c.readAt}, nil
}

type countingFile struct {
	filesystem.File
	n *atomic.Int64
}

func (f countingFile) ReadAt(p []byte, off int64) (int, error) {
	n, err := f.File.ReadAt(p, off)
	f.n.Add(int64(n))
	return n, err
}

// noOpenerFS hides Opener: the whole-file way.
type noOpenerFS struct{ filesystem.Filesystem }

func get(t *testing.T, srv *Server, method, key string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	r := signed(t, srv, method, "http://s3.example.org/b/"+key)
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	return do(t, srv, r)
}

func etagOfBytes(b []byte) string {
	sum := md5.Sum(b)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

// A GET streams the object: serving 64 MiB does not allocate 64 MiB. Over a
// real connection, because a ResponseRecorder holds the body in memory.
func TestAGetDoesNotHoldTheObject(t *testing.T) {
	body := bytes.Repeat([]byte("0123456789abcdef"), 64<<20/16)
	srv, _ := hostServer(t, body, nil)
	srv.MaxObjectBytes = 1 << 20 // and no MD5 of 64 MiB either
	ts := httptest.NewServer(srv)
	defer ts.Close()

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	r := signed(t, srv, http.MethodGet, "http://s3.example.org/b/o.bin")
	r.URL.Host = strings.TrimPrefix(ts.URL, "http://")
	r.RequestURI = ""
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := io.CopyBuffer(struct{ io.Writer }{io.Discard}, res.Body, make([]byte, 32<<10))
	res.Body.Close()
	runtime.ReadMemStats(&after)
	if res.StatusCode != http.StatusOK || n != int64(len(body)) {
		t.Fatalf("status %d, %d bytes", res.StatusCode, n)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 16<<20 {
		t.Fatalf("serving %d MiB allocated %d MiB", len(body)>>20, alloc>>20)
	}
}

// Past MaxObjectBytes, an object is still served when the driver opens
// files, and its ETag is multipart-shaped, which no client compares with an
// MD5. A driver without Opener still refuses it.
func TestAnObjectPastTheLimit(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 10<<10)
	cfs := (*countingFS)(nil)
	srv, _ := hostServer(t, body, func(f filesystem.Filesystem) filesystem.Filesystem {
		cfs = &countingFS{Filesystem: f}
		return cfs
	})
	srv.MaxObjectBytes = 1 << 10
	w := get(t, srv, http.MethodGet, "o.bin")
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), body) {
		t.Fatalf("status %d, %d bytes", w.Code, w.Body.Len())
	}
	if et := w.Header().Get("ETag"); !strings.HasSuffix(et, `-1"`) || et == etagOfBytes(body) {
		t.Fatalf("ETag %s, want a multipart-shaped one", et)
	}
	if cfs.readFile.Load() != 0 {
		t.Fatal("a whole-file read for an object past the limit")
	}

	srv2, _ := hostServer(t, body, func(f filesystem.Filesystem) filesystem.Filesystem { return noOpenerFS{f} })
	srv2.MaxObjectBytes = 1 << 10
	if w := get(t, srv2, http.MethodGet, "o.bin"); w.Code != http.StatusBadRequest && w.Code != http.StatusRequestEntityTooLarge && w.Code < 400 {
		t.Fatalf("a driver without Opener served an object past the limit: %d", w.Code)
	}
	srv2.MaxObjectBytes = 0
	if w := get(t, srv2, http.MethodGet, "o.bin"); w.Code != http.StatusOK || w.Header().Get("ETag") != etagOfBytes(body) {
		t.Fatalf("a driver without Opener, under the limit: %d %s", w.Code, w.Header().Get("ETag"))
	}
}

// The MD5 is computed once per version of the object: a second HEAD reads
// nothing, a changed object is hashed again.
func TestTheETagIsComputedOnce(t *testing.T) {
	body := bytes.Repeat([]byte("ab"), 50<<10)
	cfs := (*countingFS)(nil)
	srv, p := hostServer(t, body, func(f filesystem.Filesystem) filesystem.Filesystem {
		cfs = &countingFS{Filesystem: f}
		return cfs
	})
	for i := range 3 {
		w := get(t, srv, http.MethodHead, "o.bin")
		if w.Header().Get("ETag") != etagOfBytes(body) || w.Body.Len() != 0 {
			t.Fatalf("HEAD %d: ETag %s, %d body bytes", i, w.Header().Get("ETag"), w.Body.Len())
		}
	}
	if got := cfs.readAt.Load(); got != int64(len(body)) {
		t.Fatalf("three HEADs read %d bytes, want the object once (%d)", got, len(body))
	}
	changed := append(bytes.Clone(body), 'z')
	if err := os.WriteFile(p, changed, 0o644); err != nil {
		t.Fatal(err)
	}
	if et := get(t, srv, http.MethodHead, "o.bin").Header().Get("ETag"); et != etagOfBytes(changed) {
		t.Fatalf("after a change the ETag is %s, want %s", et, etagOfBytes(changed))
	}
}

func TestARangeIsStreamed(t *testing.T) {
	body := bytes.Repeat([]byte("0123456789"), 1000)
	for name, wrap := range map[string]func(filesystem.Filesystem) filesystem.Filesystem{
		"host file": nil,
		"other":     func(f filesystem.Filesystem) filesystem.Filesystem { return &countingFS{Filesystem: f} },
	} {
		srv, _ := hostServer(t, body, wrap)
		w := get(t, srv, http.MethodGet, "o.bin", "Range", "bytes=10-29")
		if w.Code != http.StatusPartialContent || w.Body.String() != string(body[10:30]) {
			t.Fatalf("%s: status %d, body %q", name, w.Code, w.Body.String())
		}
	}
}

// A driver whose OpenFile fails, or returns nothing, is an error, not a panic.
func TestADriverThatCannotOpen(t *testing.T) {
	for name, set := range map[string]func(*countingFS){
		"fails": func(c *countingFS) { c.openErr = os.ErrPermission },
		"nil":   func(c *countingFS) { c.nilFile = true },
	} {
		srv, _ := hostServer(t, []byte("abc"), func(f filesystem.Filesystem) filesystem.Filesystem {
			c := &countingFS{Filesystem: f}
			set(c)
			return c
		})
		if w := get(t, srv, http.MethodGet, "o.bin"); w.Code < 400 {
			t.Fatalf("%s: status %d", name, w.Code)
		}
	}
}

func TestTheETagCacheIsBounded(t *testing.T) {
	c := newETagCache()
	mod := time.Unix(1, 0)
	for i := range etagCacheSize + 10 {
		c.put(string(rune(i)), etagEntry{size: 1, modTime: mod, etag: "e"})
	}
	if len(c.m) != etagCacheSize || len(c.order) != etagCacheSize {
		t.Fatalf("%d entries, %d in order; want %d", len(c.m), len(c.order), etagCacheSize)
	}
	if _, ok := c.get(string(rune(0)), 1, mod); ok {
		t.Fatal("the oldest entry survived")
	}
	c.put(string(rune(20)), etagEntry{size: 2, modTime: mod, etag: "f"})
	if e, ok := c.get(string(rune(20)), 2, mod); !ok || e.etag != "f" {
		t.Fatal("an update in place was lost")
	}
}
