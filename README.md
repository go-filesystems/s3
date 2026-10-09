<p align="center"><img src="https://raw.githubusercontent.com/go-filesystems/brand/main/social/go-filesystems.png" alt="go-filesystems/s3" width="640"></p>

# s3

[![Go Reference](https://pkg.go.dev/badge/github.com/go-filesystems/s3.svg)](https://pkg.go.dev/github.com/go-filesystems/s3)
[![License](https://img.shields.io/badge/license-BSD--3--Clause-0A6E96?style=flat-square)](LICENSE)
[![CI](https://github.com/go-filesystems/s3/actions/workflows/ci.yml/badge.svg)](https://github.com/go-filesystems/s3/actions/workflows/ci.yml)
[![cgo](https://img.shields.io/badge/cgo-none-0079A8?style=flat-square)](https://github.com/go-filesystems/s3)

**An S3-compatible object API over any [`go-filesystems`](https://github.com/go-filesystems) driver.** Pure Go, `CGO_ENABLED=0`, an `http.Handler`.

```go
srv, err := s3.New(fsys, func(id string) (string, bool) {
    if id == "AKIA..." { return secret, true }
    return "", false
})
http.ListenAndServe(":9000", srv)
```

## A bucket is a top-level directory

```
GET /                      the top-level DIRECTORIES, as buckets
GET /photos?list-type=2    the files under /photos, as keys
GET /photos/holiday.jpg    the file /photos/holiday.jpg
```

A key is a path and a "directory" is a prefix, which is what S3 clients already
believe. A **file** at the root is not a bucket and is not reachable at all.

## What is here

`ListBuckets`, `HeadBucket`, `ListObjectsV2` (prefix, delimiter, max-keys,
continuation), `HeadObject`, `GetObject` with `Range`. Authenticated with AWS
Signature Version 4, header or presigned.

- **An unknown access key is answered as a wrong signature**
  (`SignatureDoesNotMatch`), through the same checks in the same order, an
  HMAC with a decoy secret included: AWS's `InvalidAccessKeyId` would tell
  anybody which access keys (which user names) exist. Since v0.3.0.
- **A presigned URL lasts a week at most** (`X-Amz-Expires` ≤ 604800, AWS's
  bound) and is refused before its own `X-Amz-Date`. Before v0.3.0 presigned
  URLs were refused whatever their signature: the signature was deleted from
  the query before it was compared.

**Exports are read-only by default**, following the rest of the family. Most of
what this is pointed at is a forensic or build artefact, and an accidental
write to one is unrecoverable.

## What is refused by name

Multipart upload, versioning, ACLs, policies, lifecycle rules, replication and
tagging. A `Filesystem` has nowhere to keep any of them, and answering *yes* to
a policy nobody enforces is worse than answering *no*. A client that asks gets
`NotImplemented`, which is what makes it fall back to a single `PUT`.

## Two things worth knowing before you point it at something large

**`GetObject` streams the object** (since v0.5.0) when the driver opens files
(`filesystem.Opener`), whatever its size; a file of the host
([`osfs`](https://github.com/go-filesystems/osfs)) goes out with `sendfile(2)`
over plain HTTP. Before, every GET and HEAD read the whole object into memory
— a few concurrent requests for large objects cost the server gigabytes, and
nothing over 256 MiB could be served. A driver without `Opener` can only hand
over a whole file, so for it `MaxObjectBytes` (256 MiB by default) still bounds
what is read into memory and answers `EntityTooLarge` past it.

**The ETag is the MD5 of the content**, which rclone and the AWS SDKs check: it
is computed once per version of an object — streamed through the hash, and
kept while its size and modification time are unchanged (4096 objects
remembered). Past `MaxObjectBytes` the ETag is multipart-shaped (`"…-1"`),
which S3 says is not an MD5 and clients do not compare with one: a HEAD of a
40 GB object does not read 40 GB.

**`LastModified` is the epoch.** `filesystem.Stat` carries `Mode`, `Size` and
`Inode`, and no modification time at all. The field is not optional in the S3
schema, so something must be sent; the epoch is obviously not a real time,
where `time.Now()` would look like a file that just changed and make every sync
tool copy everything, every run.

## Licence

BSD-3-Clause.
