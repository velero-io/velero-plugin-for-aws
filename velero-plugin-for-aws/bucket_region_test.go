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
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// regionFake answers HeadBucket with a canned status and headers, and counts
// signed requests so tests can assert the probe is anonymous.
type regionFake struct {
	status int
	header map[string]string

	mu          sync.Mutex
	signedCount int
	lastHost    string
}

func (f *regionFake) Do(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		_, _ = io.Copy(io.Discard, r.Body)
		r.Body.Close()
	}

	f.mu.Lock()
	if r.Header.Get("Authorization") != "" {
		f.signedCount++
	}
	f.lastHost = r.URL.Host
	f.mu.Unlock()

	hdr := http.Header{}
	for k, v := range f.header {
		hdr.Set(k, v)
	}
	body := ""
	if f.status != http.StatusOK {
		body = `<Error><Code>AccessDenied</Code><Message>denied</Message></Error>`
	}
	return &http.Response{
		StatusCode: f.status,
		Header:     hdr,
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    r,
	}, nil
}

func newRegionClient(t *testing.T, fake *regionFake) *s3.Client {
	t.Helper()

	cfg := aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("ak", "sk", ""),
		HTTPClient:  fake,
	}
	client, err := newS3Client(cfg, "", false)
	require.NoError(t, err)
	return client
}

// TestGetBucketRegion covers the cases getBucketRegion exists to handle. The
// 301/403-with-header cases are the reason this function replaces a plain
// HeadBucket call: S3 answers a cross-region probe with an error whose
// response still carries the bucket's region.
func TestGetBucketRegion(t *testing.T) {
	tests := []struct {
		name         string
		status       int
		header       map[string]string
		expectRegion string
		expectErr    string
	}{
		{
			name:         "success returns region from response",
			status:       http.StatusOK,
			header:       map[string]string{bucketRegionHeader: "eu-west-1"},
			expectRegion: "eu-west-1",
		},
		{
			name:         "403 with region header returns region (cross-region, private bucket)",
			status:       http.StatusForbidden,
			header:       map[string]string{bucketRegionHeader: "eu-west-1"},
			expectRegion: "eu-west-1",
		},
		{
			name:         "301 with region header returns region (cross-region redirect)",
			status:       http.StatusMovedPermanently,
			header:       map[string]string{bucketRegionHeader: "ap-south-1"},
			expectRegion: "ap-south-1",
		},
		{
			name:      "404 reports bucket not found",
			status:    http.StatusNotFound,
			expectErr: "not found",
		},
		{
			name:      "403 without region header returns the error",
			status:    http.StatusForbidden,
			expectErr: "AccessDenied",
		},
		{
			name:      "success without region errors",
			status:    http.StatusOK,
			expectErr: "no region returned",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := &regionFake{status: tc.status, header: tc.header}
			client := newRegionClient(t, fake)

			region, err := getBucketRegion(context.Background(), client, "bucket")

			if tc.expectRegion != "" {
				require.NoError(t, err)
				assert.Equal(t, tc.expectRegion, region)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.expectErr)
			}

			// The probe must be anonymous: region detection must not depend
			// on the caller's credentials being valid.
			assert.Zero(t, fake.signedCount, "HeadBucket probe was signed")
		})
	}
}

// TestGetBucketRegionPassesOptFns guards the variadic options the production
// call site relies on to pin the partition hint region: the client is built
// for eu-west-1, and the probe must go to the us-east-1 endpoint from the
// optFn. If getBucketRegion dropped its optFns, the request would carry the
// client's own region instead.
func TestGetBucketRegionPassesOptFns(t *testing.T) {
	fake := &regionFake{
		status: http.StatusOK,
		header: map[string]string{bucketRegionHeader: "eu-west-1"},
	}
	cfg := aws.Config{
		Region:      "eu-west-1",
		Credentials: credentials.NewStaticCredentialsProvider("ak", "sk", ""),
		HTTPClient:  fake,
	}
	client, err := newS3Client(cfg, "", false)
	require.NoError(t, err)

	region, err := getBucketRegion(context.Background(), client, "bucket",
		func(o *s3.Options) { o.Region = "us-east-1" })

	require.NoError(t, err)
	assert.Equal(t, "eu-west-1", region)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	require.NotEmpty(t, fake.lastHost, "no request was recorded")
	assert.Contains(t, fake.lastHost, "us-east-1",
		"probe did not use the optFn region endpoint")
	assert.NotContains(t, fake.lastHost, "eu-west-1",
		"probe fell back to the client's own region")
}
