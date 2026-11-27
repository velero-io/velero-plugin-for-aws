/*
Copyright the Velero contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	smithy "github.com/aws/smithy-go"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// checksumHeader matches S3's per-object checksum headers, e.g.
// x-amz-checksum-crc32. x-amz-sdk-checksum-algorithm is deliberately not
// matched: it only names an algorithm and is set when the caller explicitly
// configures one, which is not what issue #10543 is about.
var checksumHeader = regexp.MustCompile(`(?i)^x-amz-checksum-`)

// fakeS3 stands in for the S3 endpoint at the http.Client level, so it
// observes the exact request the SDK would put on the socket -- after
// signing and after any checksum middleware has run. It answers the four
// operations a multipart upload needs and records every request.
type fakeS3 struct {
	mu       sync.Mutex
	requests []*http.Request

	// partError makes UploadPart answer 403 AccessDenied. Set before the
	// upload starts and never mutated during it.
	partError bool
}

func (f *fakeS3) Do(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Clone(r.Context()))
	f.mu.Unlock()

	// The SDK streams a real body; it must be consumed before responding.
	if r.Body != nil {
		_, _ = io.Copy(io.Discard, r.Body)
		r.Body.Close()
	}

	q := r.URL.Query()
	hdr := http.Header{}
	body := ""
	status := http.StatusOK

	switch {
	case f.partError && r.Method == http.MethodPut && q.Has("partNumber"):
		status = http.StatusForbidden
		body = `<Error><Code>AccessDenied</Code><Message>denied</Message></Error>`
	case r.Method == http.MethodPost && q.Has("uploads"):
		body = `<InitiateMultipartUploadResult><Bucket>bucket</Bucket>` +
			`<Key>key</Key><UploadId>test-upload-id</UploadId>` +
			`</InitiateMultipartUploadResult>`
	case r.Method == http.MethodPost && q.Has("uploadId"):
		body = `<CompleteMultipartUploadResult><Bucket>bucket</Bucket>` +
			`<Key>key</Key><ETag>"final"</ETag></CompleteMultipartUploadResult>`
	default: // UploadPart and single-part PutObject
		hdr.Set("ETag", `"part"`)
	}

	return &http.Response{
		StatusCode: status,
		Header:     hdr,
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    r,
	}, nil
}

// uploadParts returns the recorded UploadPart requests.
func (f *fakeS3) uploadParts() []*http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []*http.Request
	for _, r := range f.requests {
		if r.Method == http.MethodPut && r.URL.Query().Has("partNumber") {
			out = append(out, r)
		}
	}
	return out
}

// zeroReader yields size bytes of zeros without allocating them, so a test can
// push an object past the multipart threshold cheaply.
type zeroReader struct{ remaining int64 }

func (z *zeroReader) Read(p []byte) (int, error) {
	if z.remaining <= 0 {
		return 0, io.EOF
	}
	n := min(int64(len(p)), z.remaining)
	clear(p[:n])
	z.remaining -= n
	return int(n), nil
}

// newFakeObjectStore wires an ObjectStore to fakeS3 through the same
// constructors production uses, so a regression in either newS3Uploader or
// its options is visible here.
func newFakeObjectStore(t *testing.T) (*ObjectStore, *fakeS3) {
	t.Helper()

	fake := &fakeS3{}
	cfg := aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("ak", "sk", ""),
		HTTPClient:  fake,
	}
	client, err := newS3Client(cfg, "", false)
	require.NoError(t, err)

	return &ObjectStore{log: newLogger(), s3Uploader: newS3Uploader(client)}, fake
}

// TestPutObjectMultipartOmitsTrailingChecksum is the wire-level guard for
// https://github.com/velero-io/velero/issues/10543: some S3-compatible
// backends reject UploadPart requests carrying a trailing checksum. The
// plugin's S3 client sets RequestChecksumCalculationWhenRequired and the
// transfer manager must inherit it rather than fall back to its own
// WhenSupported default.
//
// Unlike the options-level test, this exercises newS3Uploader and the real
// request path, so dropping the option function at the call site fails here.
func TestPutObjectMultipartOmitsTrailingChecksum(t *testing.T) {
	store, fake := newFakeObjectStore(t)

	// transfermanager's default MultipartUploadThreshold is 16MiB; exceed it
	// so the upload genuinely takes the multipart path.
	require.NoError(t, store.PutObject("bucket", "key", &zeroReader{remaining: 17 << 20}))

	parts := fake.uploadParts()
	require.NotEmpty(t, parts, "expected a multipart upload; threshold may have changed")

	for i, p := range parts {
		assert.Empty(t, p.Header.Get("x-amz-trailer"), "UploadPart %d sent a trailing checksum", i)
		for name := range p.Header {
			assert.False(t, checksumHeader.MatchString(name),
				"UploadPart %d sent checksum header %s", i, name)
		}
	}
}

// TestPutObjectMultipartSendsSSECHeadersOnEveryPart guards SSE-C against the
// migration: S3 requires the customer key on every UploadPart, not just on
// CreateMultipartUpload, and a part missing it fails the whole upload. The
// transfer manager derives each part's request from UploadObjectInput, so a
// future change to that mapping would silently break SSE-C backups.
func TestPutObjectMultipartSendsSSECHeadersOnEveryPart(t *testing.T) {
	store, fake := newFakeObjectStore(t)
	store.sseCustomerKey = "0123456789abcdef0123456789abcdef"
	store.sseCustomerKeyMd5 = "md5-of-customer-key"

	require.NoError(t, store.PutObject("bucket", "key", &zeroReader{remaining: 17 << 20}))

	parts := fake.uploadParts()
	require.NotEmpty(t, parts, "expected a multipart upload")

	for i, p := range parts {
		assert.Equal(t, "AES256",
			p.Header.Get("x-amz-server-side-encryption-customer-algorithm"),
			"UploadPart %d missing SSE-C algorithm", i)
		assert.NotEmpty(t,
			p.Header.Get("x-amz-server-side-encryption-customer-key"),
			"UploadPart %d missing SSE-C key", i)
		assert.NotEmpty(t,
			p.Header.Get("x-amz-server-side-encryption-customer-key-md5"),
			"UploadPart %d missing SSE-C key MD5", i)
	}
}

// TestPutObjectMultipartPreservesAPIError guards wrapSSECError across the
// migration. It only fires when errors.As can reach a smithy.APIError, so if
// the transfer manager ever wrapped part failures in a type without Unwrap,
// users would lose the SSE-C diagnostic added in #318 with no test failure
// anywhere else.
func TestPutObjectMultipartPreservesAPIError(t *testing.T) {
	store, fake := newFakeObjectStore(t)
	store.sseCustomerKey = "0123456789abcdef0123456789abcdef"
	fake.partError = true

	err := store.PutObject("bucket", "key", &zeroReader{remaining: 17 << 20})
	require.Error(t, err)

	var apiErr smithy.APIError
	require.True(t, errors.As(err, &apiErr),
		"smithy.APIError must survive the transfer manager's error wrapping")
	assert.Equal(t, "AccessDenied", apiErr.ErrorCode())
	assert.Contains(t, err.Error(), "April 2026",
		"SSE-C guidance from wrapSSECError was lost on the multipart path")
}

// TestPutObjectMultipartExplicitChecksumSent pins the other half of the
// checksum contract: Init defaults checksumAlg to CRC32, and an explicitly
// configured algorithm is honored regardless of
// RequestChecksumCalculationWhenRequired, so UploadPart requests DO carry
// the checksum. TestPutObjectMultipartOmitsTrailingChecksum covers only the
// empty-algorithm configuration; this one covers the production default.
func TestPutObjectMultipartExplicitChecksumSent(t *testing.T) {
	store, fake := newFakeObjectStore(t)
	store.checksumAlg = "CRC32" // Init default when checksumAlgorithm is unset

	require.NoError(t, store.PutObject("bucket", "key", &zeroReader{remaining: 17 << 20}))

	parts := fake.uploadParts()
	require.NotEmpty(t, parts, "expected a multipart upload")

	for i, p := range parts {
		hasChecksum := false
		if strings.Contains(p.Header.Get("x-amz-trailer"), "checksum") {
			hasChecksum = true
		}
		for name := range p.Header {
			if checksumHeader.MatchString(name) {
				hasChecksum = true
			}
		}
		assert.True(t, hasChecksum,
			"UploadPart %d should carry the explicitly configured CRC32 checksum", i)
	}
}
