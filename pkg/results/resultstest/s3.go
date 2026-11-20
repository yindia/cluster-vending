// Package resultstest is an in-memory S3 for the tests of what reads and
// writes the results buckets (pkg/results).
package resultstest

import (
	"bytes"
	"context"
	"crypto/sha256"
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

// S3 is an in-memory S3 for the tests: objects by bucket and key, ETags
// that change with the body, conditional GETs, paged listings and
// batch deletes. Errors are injected per operation.
type S3 struct {
	mu      sync.Mutex
	objects map[string]map[string]*object
	// Buckets that exist; a bucket not in it answers NoSuchBucket.
	Buckets map[string]bool
	// Fail returns an error for an operation ("GetObject", ...) on a bucket
	// and key, or nil.
	Fail func(op, bucket, key string) error
	// PageSize is how many keys one ListObjectsV2 page holds (default 1000).
	PageSize int
	// Calls counts operations; Puts lists the keys written, in order.
	Calls map[string]int
	Puts  []string
	// Block makes GetObject wait for the context (a hung request).
	Block bool
}

type object struct {
	body            []byte
	etag            string
	contentEncoding string
	contentType     string
	metadata        map[string]string
	modified        time.Time
}

// New returns a fake with the buckets existing and empty.
func New(buckets ...string) *S3 {
	f := &S3{objects: map[string]map[string]*object{}, Buckets: map[string]bool{}, Calls: map[string]int{}}
	for _, b := range buckets {
		f.Buckets[b] = true
		f.objects[b] = map[string]*object{}
	}
	return f
}

func (f *S3) enter(op, bucket, key string) error {
	f.Calls[op]++
	if f.Fail != nil {
		if err := f.Fail(op, bucket, key); err != nil {
			return err
		}
	}
	if !f.Buckets[bucket] {
		return &smithy.GenericAPIError{Code: "NoSuchBucket", Message: "the bucket does not exist"}
	}
	return nil
}

func (f *S3) GetObject(ctx context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	f.mu.Lock()
	block := f.Block
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

func (f *S3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
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
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	f.objects[bucket][key] = &object{body: body, etag: etag, contentEncoding: awssdk.ToString(in.ContentEncoding),
		contentType: awssdk.ToString(in.ContentType), metadata: in.Metadata, modified: time.Now()}
	f.Puts = append(f.Puts, bucket+"/"+key)
	return &s3.PutObjectOutput{ETag: awssdk.String(etag)}, nil
}

func (f *S3) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
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
	size := f.PageSize
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

func (f *S3) DeleteObjects(_ context.Context, in *s3.DeleteObjectsInput, _ ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error) {
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
		if f.Fail != nil {
			if err := f.Fail("DeleteObject:"+key, bucket, key); err != nil {
				out.Errors = append(out.Errors, types.Error{Key: o.Key, Code: awssdk.String("AccessDenied"), Message: awssdk.String(err.Error())})
				continue
			}
		}
		delete(f.objects[bucket], key)
		out.Deleted = append(out.Deleted, types.DeletedObject{Key: o.Key})
	}
	return out, nil
}

// Keys are the keys of bucket, sorted.
func (f *S3) Keys(bucket string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.objects[bucket]))
	for k := range f.objects[bucket] {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Object is the body and metadata of a stored key; nil when there is none.
func (f *S3) Object(bucket, key string) (body []byte, etag string, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objects[bucket][key]
	if !ok {
		return nil, "", false
	}
	return o.body, o.etag, true
}

// Set stores an object directly (a key the platform did not write).
func (f *S3) Set(bucket, key string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.objects[bucket] == nil {
		f.objects[bucket] = map[string]*object{}
	}
	f.Buckets[bucket] = true
	f.objects[bucket][key] = &object{body: body, etag: `"set"`, modified: time.Now()}
}
