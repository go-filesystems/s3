// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package s3

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io"
	"strconv"
	"sync"
	"time"

	filesystem "github.com/go-filesystems/interface"
)

// An object's ETag is the MD5 of its content, which rclone and the AWS SDKs
// compare against what they received. Computing it means reading the whole
// object, so it is computed once, streamed through the hash a megabyte at a
// time, and kept for as long as the object's size and modification time stay
// what they were.
//
// Above the hashing limit (Server.MaxObjectBytes) the ETag is shaped like a
// multipart upload's, "<hex>-1": S3 says such an ETag is not the MD5 of the
// content, clients do not compare it with one, and it changes when the
// object does. A HEAD of a 40 GB object no longer reads 40 GB.

// errNilFile is a driver's OpenFile returning neither a file nor an error.
var errNilFile = errors.New("s3: driver OpenFile returned a nil File with no error")

// etagEntry is one remembered ETag and what it was computed for.
type etagEntry struct {
	size    int64
	modTime time.Time
	etag    string
	// hashed is an MD5 of the content, not the multipart-shaped stand-in: a
	// stand-in is only good while the object is still past the limit.
	hashed bool
}

// etagCache remembers ETags, at most etagCacheSize of them.
type etagCache struct {
	mu    sync.Mutex
	m     map[string]etagEntry
	order []string // insertion order, for eviction
}

const etagCacheSize = 4096

func newETagCache() *etagCache { return &etagCache{m: map[string]etagEntry{}} }

func (c *etagCache) get(p string, size int64, mod time.Time) (etagEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[p]
	if !ok || e.size != size || !e.modTime.Equal(mod) {
		return etagEntry{}, false
	}
	return e, true
}

func (c *etagCache) put(p string, e etagEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.m[p]; !ok {
		if len(c.order) >= etagCacheSize {
			delete(c.m, c.order[0])
			c.order = c.order[1:]
		}
		c.order = append(c.order, p)
	}
	c.m[p] = e
}

// etagFor returns the object's ETag, from the cache when its size and
// modification time are unchanged. ok is false when it could not be read.
func (s store) etagFor(p string, size int64, mod time.Time, max int64) (string, bool) {
	if e, ok := s.etags.get(p, size, mod); ok && (e.hashed || size > max) {
		return e.etag, true
	}
	var etag string
	hashed := size <= max
	if !hashed {
		sum := md5.Sum([]byte(p + "\x00" + strconv.FormatInt(size, 10) + "\x00" + mod.UTC().Format(time.RFC3339Nano)))
		etag = `"` + hex.EncodeToString(sum[:]) + `-1"`
	} else {
		r, done, err := s.open(p, max)
		if err != nil {
			return "", false
		}
		h := md5.New()
		_, err = io.CopyBuffer(h, io.NewSectionReader(r, 0, size), make([]byte, min(size+1, 1<<20)))
		done()
		if err != nil {
			return "", false
		}
		etag = `"` + hex.EncodeToString(h.Sum(nil)) + `"`
	}
	s.etags.put(p, etagEntry{size: size, modTime: mod, etag: etag, hashed: hashed})
	return etag, true
}

// open returns the object's bytes as an io.ReaderAt and what closes it:
// the driver's opened file when it has Opener, whatever its size; the whole
// file read into memory otherwise, which only an object of at most max bytes
// is allowed.
func (s store) open(p string, max int64) (io.ReaderAt, func(), error) {
	if o, ok := s.fsys.(filesystem.Opener); ok {
		f, err := o.OpenFile(p)
		if err != nil {
			return nil, nil, err
		}
		if f == nil {
			return nil, nil, errNilFile
		}
		return f, func() { _ = f.Close() }, nil
	}
	st, err := s.fsys.Stat(p)
	if err != nil {
		return nil, nil, err
	}
	if int64(st.Size()) > max {
		return nil, nil, errTooLargeErr
	}
	data, err := s.fsys.ReadFile(p)
	if err != nil {
		return nil, nil, err
	}
	return bytes.NewReader(data), func() {}, nil
}
