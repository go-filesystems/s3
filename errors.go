// SPDX-License-Identifier: BSD-3-Clause

package s3

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
)

// An apiError is one of S3's error codes, which clients match on by NAME.
//
// The code is not decoration: an SDK decides whether to retry, to fall back to
// a single PUT, or to give up, by reading it. "NoSuchKey" and "NoSuchBucket"
// mean different things to a client walking a prefix, and answering the wrong
// one sends it looking in the wrong place.
type apiError struct {
	code    string
	status  int
	message string
}

func (e apiError) Error() string { return e.code + ": " + e.message }

var (
	errNoSuchBucket    = apiError{"NoSuchBucket", http.StatusNotFound, "the specified bucket does not exist"}
	errNoSuchKey       = apiError{"NoSuchKey", http.StatusNotFound, "the specified key does not exist"}
	errAccessDenied    = apiError{"AccessDenied", http.StatusForbidden, "access denied"}
	errNotImplemented  = apiError{"NotImplemented", http.StatusNotImplemented, "this server does not implement that operation"}
	errInvalidRequest  = apiError{"InvalidRequest", http.StatusBadRequest, "the request could not be understood"}
	errBucketNotEmpty  = apiError{"BucketNotEmpty", http.StatusConflict, "the bucket is not empty"}
	errBucketExists    = apiError{"BucketAlreadyOwnedByYou", http.StatusConflict, "the bucket already exists"}
	errEntityTooLarge  = apiError{"EntityTooLarge", http.StatusRequestEntityTooLarge, "the object is larger than this server accepts"}
	errInvalidRange    = apiError{"InvalidRange", http.StatusRequestedRangeNotSatisfiable, "the requested range is not satisfiable"}
	errInternal        = apiError{"InternalError", http.StatusInternalServerError, "the server could not complete the request"}
	errSignatureFailed = apiError{"SignatureDoesNotMatch", http.StatusForbidden,
		"the request signature does not match the signature this server computed"}
	errMissingSignature = apiError{"AccessDenied", http.StatusForbidden, "the request was not signed"}
	errUnknownKeyID     = apiError{"InvalidAccessKeyId", http.StatusForbidden, "there is no access key of that name here"}
	errExpired          = apiError{"AccessDenied", http.StatusForbidden, "the request signature has expired"}
	errReadOnly         = apiError{"AccessDenied", http.StatusForbidden, "this export is read-only"}
)

// errorFor maps a driver's error onto the code a client acts on.
func errorFor(err error, fallback apiError) apiError {
	switch {
	case err == nil:
		return fallback
	case errors.Is(err, os.ErrNotExist), errors.Is(err, fs.ErrNotExist):
		return errNoSuchKey
	case errors.Is(err, os.ErrPermission), errors.Is(err, fs.ErrPermission):
		return errAccessDenied
	}
	return fallback
}

// errorResponse is the XML body every S3 client knows how to read. A client
// that gets an empty body with a status code guesses; one that gets this
// reports the reason to the person running it.
type errorResponse struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	Resource  string   `xml:"Resource,omitempty"`
	RequestID string   `xml:"RequestId,omitempty"`
}

func writeError(w http.ResponseWriter, r *http.Request, e apiError) {
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("x-amz-request-id", requestID(r))
	w.WriteHeader(e.status)
	body := errorResponse{
		Code: e.code, Message: e.message,
		Resource: r.URL.Path, RequestID: requestID(r),
	}
	// A HEAD has no body by definition, and writing one is a protocol error
	// that some clients report as a corrupt response.
	if r.Method == http.MethodHead {
		return
	}
	_ = xml.NewEncoder(w).Encode(body)
}

// requestID is stable for one request, so a client's error message and a
// server's log line can be matched by a person reading both.
func requestID(r *http.Request) string {
	if id := r.Header.Get("x-amz-request-id"); id != "" {
		return id
	}
	return fmt.Sprintf("%016x", r.Context().Value(requestIDKey{}))
}

type requestIDKey struct{}

// errNilFilesystem and errNoCredentials are construction failures, not
// request failures: a Server built without either cannot serve anything
// safely, and saying so at New is better than at the first request.
var (
	errNilFilesystem = errors.New("s3: New needs a Filesystem")
	errNoCredentials = errors.New("s3: New needs a CredentialLookup; " +
		"a server that cannot check a signature would accept every request")
)
