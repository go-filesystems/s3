// SPDX-License-Identifier: BSD-3-Clause

package s3

import (
	"encoding/xml"
	"time"
)

// The response bodies, in the shapes the SDKs parse.
//
// ⛔ THE XML NAMESPACE IS NOT DECORATION. Several clients -- the Java SDK and
// some older Go SDK versions among them -- match elements by qualified name
// and silently return an empty listing when it is missing. An empty listing
// looks like an empty bucket, so the failure arrives as "my files are gone"
// rather than as a parse error.
const s3NS = "http://s3.amazonaws.com/doc/2006-03-01/"

// listAllMyBucketsResult answers GET /.
type listAllMyBucketsResult struct {
	XMLName xml.Name  `xml:"ListAllMyBucketsResult"`
	NS      string    `xml:"xmlns,attr"`
	Owner   owner     `xml:"Owner"`
	Buckets []bucketX `xml:"Buckets>Bucket"`
}

type owner struct {
	ID          string `xml:"ID"`
	DisplayName string `xml:"DisplayName"`
}

type bucketX struct {
	Name         string `xml:"Name"`
	CreationDate string `xml:"CreationDate"`
}

// listBucketResult answers GET /{bucket}?list-type=2.
//
// ⚠ KeyCount is the number of Contents PLUS CommonPrefixes, not the number of
// Contents. A client that pages on KeyCount and gets only the object count
// stops early on a listing that is mostly directories.
type listBucketResult struct {
	XMLName               xml.Name  `xml:"ListBucketResult"`
	NS                    string    `xml:"xmlns,attr"`
	Name                  string    `xml:"Name"`
	Prefix                string    `xml:"Prefix"`
	Delimiter             string    `xml:"Delimiter,omitempty"`
	MaxKeys               int       `xml:"MaxKeys"`
	KeyCount              int       `xml:"KeyCount"`
	IsTruncated           bool      `xml:"IsTruncated"`
	ContinuationToken     string    `xml:"ContinuationToken,omitempty"`
	NextContinuationToken string    `xml:"NextContinuationToken,omitempty"`
	Contents              []objectX `xml:"Contents"`
	CommonPrefixes        []prefixX `xml:"CommonPrefixes"`
	EncodingType          string    `xml:"EncodingType,omitempty"`
	_                     struct{}  `xml:"-"`
}

type objectX struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
}

type prefixX struct {
	Prefix string `xml:"Prefix"`
}

// deleteResult answers POST /{bucket}?delete.
type deleteResult struct {
	XMLName xml.Name       `xml:"DeleteResult"`
	NS      string         `xml:"xmlns,attr"`
	Deleted []deletedX     `xml:"Deleted"`
	Errors  []deleteErrorX `xml:"Error"`
}

type deletedX struct {
	Key string `xml:"Key"`
}

type deleteErrorX struct {
	Key     string `xml:"Key"`
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

// deleteRequest is the body of POST /{bucket}?delete.
type deleteRequest struct {
	XMLName xml.Name `xml:"Delete"`
	Objects []struct {
		Key string `xml:"Key"`
	} `xml:"Object"`
	Quiet bool `xml:"Quiet"`
}

// ⛔ S3 TIMESTAMPS ARE NOT time.RFC3339. They are RFC3339 with exactly three
// decimal places and a literal Z, and Go's RFC3339Nano drops trailing zeros --
// so a whole second formats as "2026-09-23T10:00:00Z" where S3 sends
// "2026-09-23T10:00:00.000Z". Clients that re-sign a listing, or compare an
// If-Modified-Since, notice.
func s3Time(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}
