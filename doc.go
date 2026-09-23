// SPDX-License-Identifier: BSD-3-Clause

// Package s3 implements an S3-compatible object API over any
// [github.com/go-filesystems/interface.Filesystem], as an [net/http.Handler].
//
// Every driver in the family — ext4, xfs, btrfs, zfs, ntfs, fat32, exfat,
// hfsplus, apfs, iso9660, squashfs, ufs, ffs, uefi, oci — becomes something
// the AWS SDK, minio-go, rclone, a backup tool or a browser's fetch() can
// read and write, from a pure-Go binary with no cgo and no root.
//
// # Why S3 alongside the mounts
//
// [github.com/go-filesystems/nfs], /sftp and /webdav make a Filesystem
// MOUNTABLE. S3 is the opposite shape and that is the point: nothing mounts,
// nothing is a path on somebody's machine, and every operation is one HTTP
// request that can be authorised, logged, rate-limited and served from
// anywhere. It is what programs speak now — a backup tool, a container
// registry's storage, a data pipeline, a phone app — and none of them wants a
// mount.
//
// # A bucket is a top-level directory
//
// The mapping is the whole design decision, so it is worth stating plainly:
//
//	GET /                      the top-level DIRECTORIES, as buckets
//	GET /photos?list-type=2    the files under /photos, as keys
//	GET /photos/holiday.jpg    the file /photos/holiday.jpg
//
// A key is a path and a "directory" is a prefix, which is what S3 clients
// already believe: they display prefixes ending in the delimiter as folders.
// A driver whose root holds files rather than directories therefore has no
// buckets, and says so rather than inventing one.
//
// # What is here, and what is refused by name
//
// ListBuckets, HeadBucket, CreateBucket, DeleteBucket, ListObjectsV2 (with
// prefix, delimiter, max-keys and continuation), HeadObject, GetObject with
// Range, PutObject, DeleteObject and DeleteObjects.
//
// Multipart upload is NOT here: it is a session across requests with its own
// storage of parts, and pretending otherwise would make a client's 5 GB upload
// fail at the end rather than at the start. A client that asks gets
// NotImplemented, which is what makes it fall back to a single PUT.
//
// Nor are versioning, ACLs, policies, lifecycle rules, replication or tagging:
// a Filesystem has nowhere to keep any of them, and answering "yes" to a
// policy nobody enforces is worse than answering "no".
//
// # Signatures
//
// Requests are authenticated with AWS Signature Version 4, header or
// presigned, which is what every client sends. Exports are READ-ONLY by
// default, following the rest of the family: most of what this is pointed at
// is a forensic or build artefact, and an accidental write to one is
// unrecoverable.
package s3
