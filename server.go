// SPDX-License-Identifier: BSD-3-Clause

package s3

import (
	"bytes"
	"encoding/xml"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	filesystem "github.com/go-filesystems/interface"
)

// Server serves an S3-compatible API over one Filesystem.
//
// The zero value is not usable: New builds one. It is an http.Handler, so it
// composes with anything -- TLS, a reverse proxy, a rate limiter -- rather
// than owning a listener.
type Server struct {
	store store

	decoyOnce sync.Once
	decoy     string

	// Credentials resolves an access key to its secret. Required: a server
	// with no way to check a signature would have to accept every request,
	// and New refuses to build one.
	Credentials CredentialLookup

	// Region the signatures are expected to carry. Empty accepts whatever the
	// client signed with, which is what a private deployment wants -- clients
	// invent a region and would otherwise all have to agree on the same
	// invention.
	Region string

	// ReadOnly refuses every mutating verb. Default TRUE, deliberately: see
	// the package doc.
	ReadOnly bool

	// MaxObjectBytes is the largest object whose ETag is the MD5 of its
	// content (larger ones get a multipart-shaped ETag, which clients do not
	// compare with one), and the largest a driver without Opener may serve,
	// since such a driver can only hand over a whole file in memory. A driver
	// with Opener is streamed whatever the size. 0 takes the default.
	MaxObjectBytes int64

	// ClockSkew is how far a signed request may be from this server's clock.
	// 0 takes the default. It is replay protection, not a courtesy.
	ClockSkew time.Duration

	// Clock is for tests.
	Clock func() time.Time
}

const (
	defaultMaxObject = 256 << 20 // 256 MiB
	defaultSkew      = 15 * time.Minute
)

// New builds a Server over fsys.
func New(fsys filesystem.Filesystem, creds CredentialLookup) (*Server, error) {
	if fsys == nil {
		return nil, errNilFilesystem
	}
	if creds == nil {
		// ⛔ Not defaulted to "allow everything". A server that cannot check a
		// signature and starts anyway is an open export, and the mistake is
		// invisible: every client works.
		return nil, errNoCredentials
	}
	return &Server{
		store:       store{fsys: fsys, etags: newETagCache()},
		Credentials: creds,
		ReadOnly:    true,
	}, nil
}

func (s *Server) now() time.Time {
	if s.Clock != nil {
		return s.Clock()
	}
	return time.Now()
}

// maxObject is MaxObjectBytes or its default. It is read where it is used:
// ServeHTTP used to copy it into the store at every request, a write that
// concurrent requests raced on.
func (s *Server) maxObject() int64 {
	if s.MaxObjectBytes > 0 {
		return s.MaxObjectBytes
	}
	return defaultMaxObject
}

func (s *Server) skew() time.Duration {
	if s.ClockSkew > 0 {
		return s.ClockSkew
	}
	return defaultSkew
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if _, e := s.authorize(r); e.code != "" {
		writeError(w, r, e)
		return
	}

	bkt, key := splitPath(r.URL.Path)
	switch {
	case bkt == "":
		s.listBuckets(w, r)
	case key == "":
		s.bucketOp(w, r, bkt)
	default:
		s.objectOp(w, r, bkt, key)
	}
}

func (s *Server) listBuckets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, r, errNotImplemented)
		return
	}
	bs, err := s.store.buckets()
	if err != nil {
		writeError(w, r, errorFor(err, errInternal))
		return
	}
	out := listAllMyBucketsResult{NS: s3NS, Owner: owner{ID: "go-filesystems", DisplayName: "go-filesystems"}}
	for _, b := range bs {
		out.Buckets = append(out.Buckets, bucketX{Name: b.name, CreationDate: s3Time(b.modTime)})
	}
	writeXML(w, r, http.StatusOK, out)
}

func (s *Server) bucketOp(w http.ResponseWriter, r *http.Request, bkt string) {
	if !s.store.hasBucket(bkt) {
		writeError(w, r, errNoSuchBucket)
		return
	}
	switch r.Method {
	case http.MethodHead:
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		s.listObjects(w, r, bkt)
	case http.MethodPut, http.MethodDelete, http.MethodPost:
		// Creating and deleting buckets means creating and deleting top-level
		// directories, which is a write.
		writeError(w, r, errReadOnly)
	default:
		writeError(w, r, errNotImplemented)
	}
}

func (s *Server) listObjects(w http.ResponseWriter, r *http.Request, bkt string) {
	q := r.URL.Query()
	// ⛔ list-type=2 or nothing. V1 uses `marker` where V2 uses
	// `continuation-token`, and answering a V1 request with a V2 body gives a
	// client a NextContinuationToken it will never send back -- so it re-reads
	// the first page for ever. Refusing by name is the kinder failure.
	if lt := q.Get("list-type"); lt != "" && lt != "2" {
		writeError(w, r, errNotImplemented)
		return
	}
	max := 1000
	if v := q.Get("max-keys"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n < max {
			max = n
		}
	}
	after := q.Get("continuation-token")
	if after == "" {
		after = q.Get("start-after")
	}
	contents, prefixes, trunc, err := s.store.list(bkt, q.Get("prefix"), q.Get("delimiter"), after, max)
	if err != nil {
		writeError(w, r, errorFor(err, errInternal))
		return
	}
	out := listBucketResult{
		NS: s3NS, Name: bkt, Prefix: q.Get("prefix"), Delimiter: q.Get("delimiter"),
		MaxKeys: max, IsTruncated: trunc, ContinuationToken: q.Get("continuation-token"),
		KeyCount: len(contents) + len(prefixes),
	}
	for _, o := range contents {
		out.Contents = append(out.Contents, objectX{
			Key: o.key, LastModified: s3Time(o.modTime), ETag: o.etag,
			Size: o.size, StorageClass: "STANDARD",
		})
	}
	for _, p := range prefixes {
		out.CommonPrefixes = append(out.CommonPrefixes, prefixX{Prefix: p})
	}
	if trunc && len(contents) > 0 {
		out.NextContinuationToken = contents[len(contents)-1].key
	}
	writeXML(w, r, http.StatusOK, out)
}

func (s *Server) objectOp(w http.ResponseWriter, r *http.Request, bkt, key string) {
	if !s.store.hasBucket(bkt) {
		writeError(w, r, errNoSuchBucket)
		return
	}
	switch r.Method {
	case http.MethodHead:
		o, err := s.store.head(bkt, key, s.maxObject())
		if err != nil {
			writeError(w, r, mapObjectErr(err))
			return
		}
		setObjectHeaders(w, o)
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		s.getObject(w, r, bkt, key)
	case http.MethodPut, http.MethodDelete, http.MethodPost:
		writeError(w, r, errReadOnly)
	default:
		writeError(w, r, errNotImplemented)
	}
}

func (s *Server) getObject(w http.ResponseWriter, r *http.Request, bkt, key string) {
	body, done, o, err := s.store.get(bkt, key, s.maxObject())
	if err != nil {
		writeError(w, r, mapObjectErr(err))
		return
	}
	defer done()
	setObjectHeaders(w, o)

	start, end := int64(0), o.size-1
	status := http.StatusOK
	if rangeHdr := r.Header.Get("Range"); rangeHdr != "" {
		var ok bool
		start, end, ok = parseRange(rangeHdr, o.size)
		if !ok {
			// ⛔ 416 must carry Content-Range with the real size, or a client
			// cannot tell "you asked past the end" from "the server is
			// confused", and retries the same request.
			w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(o.size, 10))
			writeError(w, r, errInvalidRange)
			return
		}
		w.Header().Set("Content-Range", "bytes "+strconv.FormatInt(start, 10)+"-"+
			strconv.FormatInt(end, 10)+"/"+strconv.FormatInt(o.size, 10))
		status = http.StatusPartialContent
	}
	n := end - start + 1
	w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
	w.WriteHeader(status)
	if r.Method == http.MethodHead || n <= 0 {
		return
	}
	_, _ = io.Copy(w, s.sendable(body, start, n))
}

// sendable is the range to send. A file of the host goes as itself, seeked
// and bounded: net/http hands it to the connection's ReadFrom, which on plain
// TCP is sendfile(2) and copies nothing through this process. Anything else
// is read a section at a time.
func (s *Server) sendable(body io.ReaderAt, start, n int64) io.Reader {
	if hf, ok := body.(filesystem.HostFile); ok {
		if _, err := hf.Seek(start, io.SeekStart); err == nil {
			return &io.LimitedReader{R: hf, N: n}
		}
	}
	return io.NewSectionReader(body, start, n)
}

func setObjectHeaders(w http.ResponseWriter, o object) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Last-Modified", o.modTime.UTC().Format(http.TimeFormat))
	w.Header().Set("Accept-Ranges", "bytes")
	if o.etag != "" {
		w.Header().Set("ETag", o.etag)
	}
	w.Header().Set("Content-Length", strconv.FormatInt(o.size, 10))
}

// parseRange reads the one form S3 clients send: bytes=start-end, either end
// open. Multiple ranges are not answered, and saying so is better than
// answering the first and letting a client believe it got them all.
func parseRange(v string, size int64) (int64, int64, bool) {
	if !strings.HasPrefix(v, "bytes=") || strings.Contains(v, ",") {
		return 0, 0, false
	}
	spec := strings.TrimPrefix(v, "bytes=")
	i := strings.Index(spec, "-")
	if i < 0 {
		return 0, 0, false
	}
	lo, hi := spec[:i], spec[i+1:]
	switch {
	case lo == "" && hi == "":
		return 0, 0, false
	case lo == "": // bytes=-N, the LAST n bytes
		n, err := strconv.ParseInt(hi, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, size > 0
	default:
		start, err := strconv.ParseInt(lo, 10, 64)
		if err != nil || start < 0 || start >= size {
			return 0, 0, false
		}
		end := size - 1
		if hi != "" {
			if e, err := strconv.ParseInt(hi, 10, 64); err == nil && e < end {
				end = e
			}
		}
		if end < start {
			return 0, 0, false
		}
		return start, end, true
	}
}

func mapObjectErr(err error) apiError {
	switch {
	case notExist(err):
		return errNoSuchKey
	case err == errTooLargeErr:
		return errEntityTooLarge
	}
	return errorFor(err, errInternal)
}

func splitPath(p string) (string, string) {
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return "", ""
	}
	if i := strings.Index(p, "/"); i >= 0 {
		return p[:i], p[i+1:]
	}
	return p, ""
}

func writeXML(w http.ResponseWriter, r *http.Request, status int, v any) {
	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	if err := xml.NewEncoder(&buf).Encode(v); err != nil {
		writeError(w, r, errInternal)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("x-amz-request-id", requestID(r))
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, &buf)
	}
}
