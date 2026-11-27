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
	"fmt"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/pkg/errors"
)

// bucketRegionHeader is set by S3 on HeadBucket responses, including error
// responses, and carries the region the bucket resides in.
const bucketRegionHeader = "X-Amz-Bucket-Region"

// getBucketRegion determines the region a bucket resides in. It replaces
// manager.GetBucketRegion from the deprecated feature/s3/manager module,
// which has no transfermanager equivalent; see
// https://github.com/aws/aws-sdk-go-v2/discussions/3306.
//
// Probing a bucket from a client configured for a different region makes S3
// answer with a 301/403 error whose response still carries the bucket's
// region in the X-Amz-Bucket-Region header, so the region is read from the
// failed response. A plain HeadBucket cannot substitute: its output is nil on
// error, hiding the header.
//
// The probe is sent unsigned, as manager.GetBucketRegion did: S3 answers an
// anonymous HeadBucket for a private bucket with 403 plus the region header,
// so region detection does not depend on the caller's credentials.
func getBucketRegion(ctx context.Context, client *s3.Client, bucket string, optFns ...func(*s3.Options)) (string, error) {
	opts := append([]func(*s3.Options){
		func(o *s3.Options) {
			o.Credentials = nil
		},
	}, optFns...)

	out, err := client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)}, opts...)
	if err == nil {
		if region := aws.ToString(out.BucketRegion); region != "" {
			return region, nil
		}
		return "", fmt.Errorf("no region returned for bucket %q", bucket)
	}

	var re *awshttp.ResponseError
	if errors.As(err, &re) && re.Response != nil {
		if re.Response.StatusCode == http.StatusNotFound {
			return "", fmt.Errorf("bucket %q not found: %w", bucket, err)
		}
		if region := re.Response.Header.Get(bucketRegionHeader); region != "" {
			return region, nil
		}
	}
	return "", err
}
