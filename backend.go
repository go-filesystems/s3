// SPDX-License-Identifier: BSD-3-Clause

package s3

import (
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	filesystem "github.com/go-filesystems/interface"
)

// The mapping from a Filesystem to the S3 object model.
//
// It is kept behind this one type on purpose, rather than reached for from the
// HTTP handlers: the handlers then deal only in buckets, keys and errors, and
// the question "what is a bucket" is answered in exactly one place.

// A bucket is a top-level directory. See the package doc for why.
type bucket struct {
	name    string
	modTime time.Time
}

// object is what HEAD and a listing entry both need.
type object struct {
	key     string
	size    int64
	modTime time.Time
	etag    string
}

// store is the Filesystem seen as an object store.
type store struct {
	fsys filesystem.Filesystem
	// maxObject bounds what GET will read. The driver interface offers
	// ReadFile and nothing else -- no opener, no reader -- so serving an
	// object means holding all of it in memory. That is fine for the
	// artefacts this is pointed at and ruinous for a 40 GB disk image, so it
	// is a stated limit that answers EntityTooLarge rather than an allocation
	// that takes the process down.
	maxObject int64
}

func (s store) buckets() ([]bucket, error) {
	entries, err := s.fsys.ListDir("/")
	if err != nil {
		return nil, err
	}
	var out []bucket
	for _, e := range entries {
		if !s.isDirPath("/" + e.Name()) {
			// A file at the root is not a bucket and is not reachable over
			// S3 at all. Said in the package doc rather than invented around.
			continue
		}
		if n := e.Name(); n != "." && n != ".." {
			out = append(out, bucket{name: n, modTime: s.modTimeOf("/" + n)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

func (s store) hasBucket(name string) bool {
	if !validBucket(name) {
		return false
	}
	st, err := s.fsys.Stat("/" + name)
	return err == nil && isDirMode(st.Mode())
}

// head is HEAD of one object.
func (s store) head(bkt, key string) (object, error) {
	p := objectPath(bkt, key)
	st, err := s.fsys.Stat(p)
	if err != nil {
		return object{}, err
	}
	if isDirMode(st.Mode()) {
		// A directory is a PREFIX, not an object. Answering 200 for it makes
		// a client believe it can GET the bytes of a folder.
		return object{}, errNoSuchKeyErr
	}
	o := object{key: key, size: int64(st.Size()), modTime: s.modTimeOf(p)}
	// ⛔ The ETag is read from the CONTENT, not invented from the metadata.
	// It is the MD5 of a single-part object, and rclone and the AWS SDKs
	// compare it against what they received: an ETag that does not match the
	// bytes turns every download into a reported corruption.
	if o.size <= s.maxObject {
		if data, err := s.fsys.ReadFile(p); err == nil {
			o.etag = etagOf(data)
		}
	}
	return o, nil
}

// get returns the whole object. Range is applied by the caller, because a
// Range that cannot be satisfied is a different S3 error from a missing key.
func (s store) get(bkt, key string) ([]byte, object, error) {
	o, err := s.head(bkt, key)
	if err != nil {
		return nil, object{}, err
	}
	if o.size > s.maxObject {
		return nil, object{}, errTooLargeErr
	}
	data, err := s.fsys.ReadFile(objectPath(bkt, key))
	if err != nil {
		return nil, object{}, err
	}
	if o.etag == "" {
		o.etag = etagOf(data)
	}
	return data, o, nil
}

// list walks a bucket, honouring prefix and delimiter the way S3 does.
//
// ⚠ The delimiter is NOT assumed to be "/". S3 allows any string, and a
// client that passes one and gets "/" behaviour sees a flat listing where it
// expected groups. The walk is therefore over full keys, with grouping applied
// afterwards -- which is also what makes an arbitrary delimiter work at all.
func (s store) list(bkt, prefix, delimiter, after string, max int) ([]object, []string, bool, error) {
	var keys []object
	if err := s.walk(bkt, "", &keys); err != nil {
		return nil, nil, false, err
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].key < keys[j].key })

	var (
		contents []object
		prefixes []string
		seen     = map[string]bool{}
		trunc    bool
	)
	for _, o := range keys {
		if !strings.HasPrefix(o.key, prefix) {
			continue
		}
		if after != "" && o.key <= after {
			continue
		}
		if delimiter != "" {
			rest := o.key[len(prefix):]
			if i := strings.Index(rest, delimiter); i >= 0 {
				cp := prefix + rest[:i+len(delimiter)]
				if !seen[cp] {
					seen[cp] = true
					if len(contents)+len(prefixes) >= max {
						trunc = true
						break
					}
					prefixes = append(prefixes, cp)
				}
				continue
			}
		}
		if len(contents)+len(prefixes) >= max {
			trunc = true
			break
		}
		contents = append(contents, o)
	}
	return contents, prefixes, trunc, nil
}

// walk collects every file under a bucket, depth first.
func (s store) walk(bkt, rel string, out *[]object) error {
	dir := objectPath(bkt, rel)
	entries, err := s.fsys.ListDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if name == "." || name == ".." {
			continue
		}
		child := name
		if rel != "" {
			child = rel + "/" + name
		}
		if s.isDirPath(objectPath(bkt, child)) {
			if err := s.walk(bkt, child, out); err != nil {
				return err
			}
			continue
		}
		p := objectPath(bkt, child)
		st, err := s.fsys.Stat(p)
		if err != nil {
			continue // a file that vanished between the listing and the stat
		}
		*out = append(*out, object{
			key:     child,
			size:    int64(st.Size()),
			modTime: s.modTimeOf(p),
		})
	}
	return nil
}

func (s store) modTimeOf(p string) time.Time {
	st, err := s.fsys.Stat(p)
	if err != nil {
		return time.Unix(0, 0).UTC()
	}
	if m, ok := st.(interface{ ModTime() time.Time }); ok {
		return m.ModTime().UTC()
	}
	// ⚠ Not every driver carries a modification time, and a listing still
	// needs a LastModified: the field is not optional in the schema and the
	// SDKs parse it. The epoch is used because it is obviously not a real
	// time, where time.Now() would look like a file that just changed and
	// make every sync tool copy everything, every run.
	return time.Unix(0, 0).UTC()
}

// ⛔⛔ DIRECTORY-NESS COMES FROM Stat().Mode(), NOT FROM DirEntry.FileType().
//
// FileType() is a uint8 with no meaning agreed across the family: fat32 passes
// the raw FAT attribute byte, where a directory is 0x10, while a Linux-shaped
// driver passes DT_DIR, which is 4. Measured rather than assumed -- a listing
// built on FileType() showed every fat32 directory as a file, and this server
// would then have had no buckets at all.
//
// Stat().Mode() IS portable: measured on fat32, a directory is 0x41ed and a
// file 0x81a4 -- S_IFDIR|0755 and S_IFREG|0644. The cost is one Stat per
// entry, which the walk was already paying for files.
const modeTypeMask, modeDir = 0xF000, 0x4000

func isDirMode(mode uint16) bool { return mode&modeTypeMask == modeDir }

// isDirPath asks the driver, because a DirEntry cannot answer portably.
func (s store) isDirPath(p string) bool {
	st, err := s.fsys.Stat(p)
	return err == nil && isDirMode(st.Mode())
}

// objectPath joins a bucket and key into a driver path, refusing anything that
// would climb out of the bucket.
//
// ⛔⛔ THIS IS THE ONE PLACE A KEY BECOMES A PATH, and a key is attacker-
// supplied. "../../etc/passwd" is a legal S3 key; path.Clean turns it into an
// escape. Cleaning FIRST and then rejecting a result that no longer starts
// with the bucket is what makes the check unfoolable by encoding: whatever the
// client wrote, the cleaned path either is inside the bucket or it is not.
func objectPath(bkt, key string) string {
	p := path.Clean("/" + bkt + "/" + key)
	base := "/" + bkt
	if p != base && !strings.HasPrefix(p, base+"/") {
		return "\x00invalid" // no driver will open this, and every one errors
	}
	return p
}

// validBucket keeps a bucket name from being a path element of its own.
func validBucket(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > 255 {
		return false
	}
	return !strings.ContainsAny(name, "/\\\x00")
}

func etagOf(b []byte) string {
	sum := md5.Sum(b)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

// Sentinels the handlers translate into apiErrors.
var (
	errNoSuchKeyErr = errors.New("s3: no such key")
	errTooLargeErr  = errors.New("s3: object larger than this server serves")
)

// notExist reports the errors a driver uses for "it is not there".
//
// ⛔⛔ THE DRIVERS DO NOT WRAP fs.ErrNotExist, SO errors.Is IS NOT ENOUGH.
// Measured on go-filesystems/fat32 v0.3.0:
//
//	fsys.Stat("/nope.txt")  ->  fat32: "/nope.txt" not found
//	errors.Is(err, fs.ErrNotExist)  ->  FALSE
//
// ⚠ AN EARLIER VERSION OF THIS COMMENT SAID WEBDAV ANSWERS 500 FOR THE SAME
// REASON. IT DOES NOT. Measured afterwards: GET of a missing file over
// go-filesystems/webdav answers 404, with the old fat32 and the new one alike.
// It tries errors.Is first and then falls back to a table of twelve message
// fragments -- "not found" among them. The claim came from reading the
// errors.Is switch and stopping before the fallback underneath it.
//
// What is true is worse in a quieter way, and webdav's own comment on that
// table says it: the table is "a *last* resort", the same twelve entries are
// duplicated in go-filesystems/nfs, and correctness depends on how each driver
// PHRASES its errors. This package had no such table, which is why a missing
// key surfaced here as InternalError rather than passing unnoticed.
//
// The real fix belongs in the drivers: "not found" should satisfy
// errors.Is(err, fs.ErrNotExist), and the interface package should say so.
// Until it does, this matches the message as well, because answering
// InternalError for a missing key makes an S3 client RETRY -- a 500 is
// retryable and a 404 is not, so the wrong code turns one missing object into
// a storm.
func notExist(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, fs.ErrNotExist) || errors.Is(err, errNoSuchKeyErr) {
		return true
	}
	return strings.HasSuffix(err.Error(), "not found")
}
