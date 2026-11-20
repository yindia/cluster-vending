package results

import (
	"context"
	"errors"
	"fmt"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// deleteBatch is S3's limit of keys in one DeleteObjects call.
const deleteBatch = 1000

// EmptyBucket deletes every object under latest/ and history/ of bucket,
// the objects of a cluster's results, so that the Bucket managed resource
// can be deleted (AWS refuses to delete a bucket with objects, and the
// platform never force-destroys one). It returns how many it deleted. A
// bucket that does not exist is empty. An object it cannot delete is an
// error (ErrAccessDenied for a refusal) after the others were tried; objects
// under other prefixes are not the platform's and are left, so the bucket
// stays undeletable until a person looks. The versioning of the bucket is
// off, so there are no versions to remove. It makes no call of its own
// timeout: bound ctx.
func EmptyBucket(ctx context.Context, client S3API, bucket string) (int, error) {
	deleted := 0
	var failed []string
	for _, prefix := range []string{LatestPrefix, HistoryPrefix} {
		var token *string
		for {
			page, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: awssdk.String(bucket),
				Prefix: awssdk.String(prefix), ContinuationToken: token, MaxKeys: awssdk.Int32(deleteBatch)})
			if err != nil {
				c := classify(err, "list s3://"+bucket+"/"+prefix)
				if errors.Is(c, ErrNotFound) {
					return deleted, nil // the bucket is gone: nothing left to empty
				}
				return deleted, c
			}
			if len(page.Contents) > 0 {
				n, bad, err := deleteKeys(ctx, client, bucket, page.Contents)
				deleted += n
				failed = append(failed, bad...)
				if err != nil {
					return deleted, err
				}
			}
			if !awssdk.ToBool(page.IsTruncated) || page.NextContinuationToken == nil {
				break
			}
			token = page.NextContinuationToken
		}
	}
	if len(failed) > 0 {
		return deleted, fmt.Errorf("%w: could not delete %d objects of s3://%s, for example %s", ErrAccessDenied, len(failed), bucket, failed[0])
	}
	return deleted, nil
}

// deleteKeys deletes a page of objects; it returns how many went and the
// keys that did not.
func deleteKeys(ctx context.Context, client S3API, bucket string, objects []types.Object) (int, []string, error) {
	ids := make([]types.ObjectIdentifier, 0, len(objects))
	for _, o := range objects {
		ids = append(ids, types.ObjectIdentifier{Key: o.Key})
	}
	out, err := client.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: awssdk.String(bucket),
		Delete: &types.Delete{Objects: ids, Quiet: awssdk.Bool(true)}})
	if err != nil {
		return 0, nil, classify(err, "delete objects of s3://"+bucket)
	}
	var bad []string
	for _, e := range out.Errors {
		bad = append(bad, fmt.Sprintf("%s (%s)", awssdk.ToString(e.Key), awssdk.ToString(e.Code)))
	}
	// Quiet mode lists failures only; the rest of the page went.
	return len(ids) - len(out.Errors), bad, nil
}
