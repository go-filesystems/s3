// SPDX-License-Identifier: BSD-3-Clause

package s3

import (
	"crypto/hmac"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-volumes/s3/sigv4"
)

// Authenticating a request.
//
// ⭐ THE VERIFIER IS THE SIGNER. Rather than reimplement SigV4 in the
// checking direction -- two implementations of one algorithm, which drift --
// this rebuilds the request as the client signed it, asks
// [github.com/go-volumes/s3/sigv4] to sign it with our copy of the secret, and
// compares. That package reproduces the official AWS "Signature Version 4 Test
// Suite" byte for byte, so the check inherits those vectors instead of needing
// its own.
//
// ⛔ The comparison is hmac.Equal, not ==. A signature check that returns
// early on the first wrong byte tells an attacker how much of a guess was
// right, one request at a time.

// CredentialLookup returns the secret for an access key, and whether that key
// exists here.
//
// ⚠ Returning ("", false) for an unknown key and a real secret otherwise is
// the whole contract. Do NOT return ("", true) for an unknown key hoping the
// signature will fail anyway: it would, but the two cases then take different
// amounts of work and a caller can tell them apart.
type CredentialLookup func(accessKeyID string) (secret string, ok bool)

// credentialScope is what the Credential= part of the header carries.
type credentialScope struct {
	accessKeyID string
	date        string
	region      string
	service     string
}

// authorize checks the signature on r and returns the access key it was
// signed with.
func (s *Server) authorize(r *http.Request) (string, apiError) {
	if q := r.URL.Query(); q.Get("X-Amz-Signature") != "" {
		return s.authorizePresigned(r, q)
	}
	return s.authorizeHeader(r)
}

func (s *Server) authorizeHeader(r *http.Request) (string, apiError) {
	raw := r.Header.Get("Authorization")
	if raw == "" {
		return "", errMissingSignature
	}
	scope, signedHeaders, claimed, ok := parseAuthorization(raw)
	if !ok {
		return "", errInvalidRequest
	}
	secret, ok := s.Credentials(scope.accessKeyID)
	if !ok {
		return "", errUnknownKeyID
	}
	t, err := amzTime(r.Header.Get("x-amz-date"), r.Header.Get("Date"))
	if err != nil {
		return "", errInvalidRequest
	}
	if s.skewed(t) {
		return "", errExpired
	}

	payload := r.Header.Get("x-amz-content-sha256")
	if payload == "" {
		payload = sigv4.HashSHA256(nil)
	}

	// Rebuilt with EXACTLY the headers the client said it signed. The signer
	// derives the signed-headers list from the request it is given, so handing
	// it the original -- proxy headers and all -- would canonicalise a
	// different request and fail every signature for the wrong reason.
	probe := r.Clone(r.Context())
	probe.Header = make(http.Header, len(signedHeaders))
	for _, h := range signedHeaders {
		if strings.EqualFold(h, "host") {
			continue // net/http keeps Host out of the map; the signer forces it
		}
		if v, ok := r.Header[http.CanonicalHeaderKey(h)]; ok {
			probe.Header[http.CanonicalHeaderKey(h)] = v
		}
	}
	probe.Header.Del("Authorization")

	signer := sigv4.New(scope.region, scope.service, sigv4.Credentials{
		AccessKeyID: scope.accessKeyID, SecretAccessKey: secret,
	})
	got := signer.SignRaw(probe, payload, t)
	if !hmac.Equal([]byte(got.Signature), []byte(claimed)) {
		return "", errSignatureFailed
	}
	return scope.accessKeyID, apiError{}
}

// authorizePresigned checks a URL that carries its own signature, which is
// what a browser or a `curl` of a shared link sends.
func (s *Server) authorizePresigned(r *http.Request, q url.Values) (string, apiError) {
	scope, ok := parseCredential(q.Get("X-Amz-Credential"))
	if !ok {
		return "", errInvalidRequest
	}
	secret, ok := s.Credentials(scope.accessKeyID)
	if !ok {
		return "", errUnknownKeyID
	}
	t, err := time.Parse("20060102T150405Z", q.Get("X-Amz-Date"))
	if err != nil {
		return "", errInvalidRequest
	}
	// ⛔ A presigned URL expires by its OWN X-Amz-Expires, not by the server's
	// clock-skew window. Applying the skew window instead would reject a link
	// meant to last a week, ten minutes after it was made.
	expires, err := strconv.Atoi(q.Get("X-Amz-Expires"))
	if err != nil || expires <= 0 {
		return "", errInvalidRequest
	}
	if s.now().After(t.Add(time.Duration(expires) * time.Second)) {
		return "", errExpired
	}

	probe := r.Clone(r.Context())
	probe.Header = make(http.Header)
	rq := q
	rq.Del("X-Amz-Signature")
	probe.URL.RawQuery = rq.Encode()

	signer := sigv4.New(scope.region, scope.service, sigv4.Credentials{
		AccessKeyID: scope.accessKeyID, SecretAccessKey: secret,
	})
	got := signer.SignRaw(probe, "UNSIGNED-PAYLOAD", t)
	if !hmac.Equal([]byte(got.Signature), []byte(q.Get("X-Amz-Signature"))) {
		return "", errSignatureFailed
	}
	return scope.accessKeyID, apiError{}
}

// parseAuthorization splits the header S3 clients send.
func parseAuthorization(v string) (credentialScope, []string, string, bool) {
	const prefix = "AWS4-HMAC-SHA256 "
	if !strings.HasPrefix(v, prefix) {
		return credentialScope{}, nil, "", false
	}
	var (
		scope   credentialScope
		headers []string
		sig     string
		okCred  bool
	)
	for _, part := range strings.Split(v[len(prefix):], ",") {
		part = strings.TrimSpace(part)
		switch {
		case strings.HasPrefix(part, "Credential="):
			scope, okCred = parseCredential(strings.TrimPrefix(part, "Credential="))
		case strings.HasPrefix(part, "SignedHeaders="):
			headers = strings.Split(strings.TrimPrefix(part, "SignedHeaders="), ";")
		case strings.HasPrefix(part, "Signature="):
			sig = strings.TrimPrefix(part, "Signature=")
		}
	}
	return scope, headers, sig, okCred && sig != "" && len(headers) > 0
}

// parseCredential reads AK/20260923/eu-west-1/s3/aws4_request.
func parseCredential(v string) (credentialScope, bool) {
	p := strings.Split(v, "/")
	if len(p) != 5 || p[4] != "aws4_request" {
		return credentialScope{}, false
	}
	return credentialScope{accessKeyID: p[0], date: p[1], region: p[2], service: p[3]}, true
}

// amzTime reads the timestamp the request was signed at.
func amzTime(amz, date string) (time.Time, error) {
	if amz != "" {
		return time.Parse("20060102T150405Z", amz)
	}
	return http.ParseTime(date)
}

// skewed reports a request signed too long ago to still be honoured.
//
// This is replay protection, and it is the only thing standing between a
// captured Authorization header and unlimited reuse: the signature stays valid
// for ever otherwise.
func (s *Server) skewed(t time.Time) bool {
	d := s.now().Sub(t)
	if d < 0 {
		d = -d
	}
	return d > s.skew()
}
