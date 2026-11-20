package results_test

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// fakeS3 is an in-memory S3 for the tests: objects by bucket and key, ETags
// that change with the body, conditional GETs, paged listings and
// batch deletes. Errors are injected per operation.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string]map[string]*fakeObject
	// buckets that exist; a bucket not in it answers NoSuchBucket.
	buckets map[string]bool
	// fail returns an error for an operation ("GetObject", ...) on a bucket
	// and key, or nil.
	fail func(op, bucket, key string) error
	// pageSize is how many keys one ListObjectsV2 page holds (default 1000).
	pageSize int
	// calls counts operations; gets lists the keys read, in order.
	calls map[string]int
	puts  []string
	// block makes GetObject wait for the context (a hung request).
	block bool
}

type fakeObject struct {
	body            []byte
	etag            string
	contentEncoding string
	contentType     string
	metadata        map[string]string
	modified        time.Time
}

func newFake(buckets ...string) *fakeS3 {
	f := &fakeS3{objects: map[string]map[string]*fakeObject{}, buckets: map[string]bool{}, calls: map[string]int{}}
	for _, b := range buckets {
		f.buckets[b] = true
		f.objects[b] = map[string]*fakeObject{}
	}
	return f
}

func (f *fakeS3) enter(op, bucket, key string) error {
	f.calls[op]++
	if f.fail != nil {
		if err := f.fail(op, bucket, key); err != nil {
			return err
		}
	}
	if !f.buckets[bucket] {
		return &smithy.GenericAPIError{Code: "NoSuchBucket", Message: "the bucket does not exist"}
	}
	return nil
}

func (f *fakeS3) GetObject(ctx context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	f.mu.Lock()
	block := f.block
	f.mu.Unlock()
	if block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	bucket, key := awssdk.ToString(in.Bucket), awssdk.ToString(in.Key)
	if err := f.enter("GetObject", bucket, key); err != nil {
		return nil, err
	}
	obj, ok := f.objects[bucket][key]
	if !ok {
		return nil, &smithy.GenericAPIError{Code: "NoSuchKey", Message: "the key does not exist"}
	}
	if in.IfNoneMatch != nil && *in.IfNoneMatch == obj.etag {
		return nil, notModified()
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(obj.body)), ETag: awssdk.String(obj.etag),
		ContentEncoding: awssdk.String(obj.contentEncoding), Metadata: obj.metadata, LastModified: awssdk.Time(obj.modified)}, nil
}

func notModified() error {
	return &awshttp.ResponseError{ResponseError: &smithyhttp.ResponseError{
		Response: &smithyhttp.Response{Response: &http.Response{StatusCode: http.StatusNotModified}},
		Err:      &smithy.GenericAPIError{Code: "NotModified", Message: "not modified"}}}
}

func (f *fakeS3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	bucket, key := awssdk.ToString(in.Bucket), awssdk.ToString(in.Key)
	if err := f.enter("PutObject", bucket, key); err != nil {
		return nil, err
	}
	body, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	sum := md5.Sum(body)
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	f.objects[bucket][key] = &fakeObject{body: body, etag: etag, contentEncoding: awssdk.ToString(in.ContentEncoding),
		contentType: awssdk.ToString(in.ContentType), metadata: in.Metadata, modified: time.Now()}
	f.puts = append(f.puts, bucket+"/"+key)
	return &s3.PutObjectOutput{ETag: awssdk.String(etag)}, nil
}

func (f *fakeS3) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	bucket := awssdk.ToString(in.Bucket)
	if err := f.enter("ListObjectsV2", bucket, ""); err != nil {
		return nil, err
	}
	var keys []string
	for k := range f.objects[bucket] {
		if strings.HasPrefix(k, awssdk.ToString(in.Prefix)) && k > awssdk.ToString(in.ContinuationToken) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	size := f.pageSize
	if size == 0 {
		size = 1000
	}
	out := &s3.ListObjectsV2Output{}
	if len(keys) > size {
		keys = keys[:size]
		out.IsTruncated = awssdk.Bool(true)
		out.NextContinuationToken = awssdk.String(keys[len(keys)-1])
	}
	for _, k := range keys {
		out.Contents = append(out.Contents, types.Object{Key: awssdk.String(k)})
	}
	return out, nil
}

func (f *fakeS3) DeleteObjects(_ context.Context, in *s3.DeleteObjectsInput, _ ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	bucket := awssdk.ToString(in.Bucket)
	if err := f.enter("DeleteObjects", bucket, ""); err != nil {
		return nil, err
	}
	if len(in.Delete.Objects) > 1000 {
		return nil, &smithy.GenericAPIError{Code: "MalformedXML", Message: "too many keys"}
	}
	out := &s3.DeleteObjectsOutput{}
	for _, o := range in.Delete.Objects {
		key := awssdk.ToString(o.Key)
		if f.fail != nil {
			if err := f.fail("DeleteObject:"+key, bucket, key); err != nil {
				out.Errors = append(out.Errors, types.Error{Key: o.Key, Code: awssdk.String("AccessDenied"), Message: awssdk.String(err.Error())})
				continue
			}
		}
		delete(f.objects[bucket], key)
		out.Deleted = append(out.Deleted, types.DeletedObject{Key: o.Key})
	}
	return out, nil
}

func (f *fakeS3) keys(bucket string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for k := range f.objects[bucket] {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
