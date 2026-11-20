package aws

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"

	awss3 "github.com/upbound/provider-aws/v2/apis/namespaced/s3/v1beta1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The bucket of a cluster's scan results (docs/architecture.md#results-in-s3):
// deprecations, recommendations and policy results are written here by the
// controller's scanners and read by the API server, both through the tenant
// role. It lives in the cluster's own AWS account and is rendered with the
// cluster:
//
//   - the Bucket "<prefix>-results", whose AWS name is ResultsBucketName
//     (its external name);
//   - BucketPublicAccessBlock, all four settings on;
//   - BucketOwnershipControls, BucketOwnerEnforced (no ACLs);
//   - BucketServerSideEncryptionConfiguration, SSE-S3 (AES256);
//   - BucketPolicy, refusing requests that are not over TLS;
//   - BucketLifecycleConfiguration: objects under history/ expire after
//     ResultsHistoryDays, incomplete multipart uploads after one day.
//
// Versioning stays off: history is explicit (a timestamped object per scan).
//
// Deletion. The Bucket is NOT force-destroyed (forProvider.forceDestroy is
// left unset): AWS refuses to delete a bucket that has objects
// (BucketNotEmpty), so the provider keeps retrying until the controller has
// emptied it. The controller therefore empties the bucket
// (results.EmptyBucket, with the tenant role) before it deletes the
// cluster's Bucket object, and the settings go before the Bucket (Usages).
// Nothing here ever wipes data on its own.

const (
	// ResultsHistoryDays is how long history/ objects live.
	ResultsHistoryDays = 30
	// resultsAbortMultipartDays abandons incomplete multipart uploads.
	resultsAbortMultipartDays = 1
	// resultsHistoryPrefix is the key prefix of history objects (pkg/results
	// writes them there).
	resultsHistoryPrefix = "history/"

	// bucketNamePrefix starts every results bucket's name; the IAM policy of
	// the tenant role allows exactly arn:aws:s3:::cv-* (pkg/cloudformation).
	bucketNamePrefix = "cv-"
	// maxBucketName is S3's limit. The name is "cv-" + prefix + "-" + 12 hex.
	maxBucketName    = 63
	bucketHashLength = 12
	maxBucketPrefix  = maxBucketName - len(bucketNamePrefix) - 1 - bucketHashLength

	resultsSuffix = "-results"
)

var (
	bucketUnsafe = regexp.MustCompile(`[^a-z0-9-]+`)
	bucketDashes = regexp.MustCompile(`-{2,}`)
)

// ResultsBucketName is the name of a cluster's results bucket:
// "cv-<prefix>-<12 hex of sha256(account, region, cluster ID)>", at most 63
// characters, lower case, the same for the same inputs (so the controller,
// the API server and the renderer agree without storing it) and with no
// tenant data beyond what the prefix says. prefix is the cluster's
// ClusterPrefix; it is lower-cased, cut to what fits and made of letters,
// digits and single hyphens.
func ResultsBucketName(prefix, accountID, region, clusterID string) string {
	sum := sha256.Sum256([]byte(accountID + "\x00" + region + "\x00" + clusterID))
	prefix = bucketDashes.ReplaceAllString(bucketUnsafe.ReplaceAllString(strings.ToLower(prefix), "-"), "-")
	prefix = strings.Trim(prefix, "-")
	if len(prefix) > maxBucketPrefix {
		prefix = strings.TrimRight(prefix[:maxBucketPrefix], "-")
	}
	if prefix != "" {
		prefix += "-"
	}
	return bucketNamePrefix + prefix + hex.EncodeToString(sum[:])[:bucketHashLength]
}

// ResultsBucketObjectName is the name of the Bucket managed resource of the
// cluster with prefix (in its tenant namespace): what the controller reads
// to see whether the bucket exists, and deletes after emptying it.
func ResultsBucketObjectName(prefix string) string { return prefix + resultsSuffix }

// HasResultsBucket reports whether the plan renders a results bucket: the
// plan asks for it (ResultsBucket), the account is known (the name needs it)
// and the cluster is one the platform created, not an imported one.
func (p ClusterPlan) HasResultsBucket() bool {
	return p.ResultsBucket && p.AccountID != "" && !p.Imported()
}

// ResultsBucketName is the AWS name of the plan's results bucket; empty
// when the plan has none (HasResultsBucket).
func (p ClusterPlan) ResultsBucketName() string {
	if !p.HasResultsBucket() {
		return ""
	}
	return ResultsBucketName(p.Prefix, p.AccountID, p.Region, p.ClusterID)
}

// resultsSettings are the suffixes of the objects that configure the bucket,
// all after the Bucket and deleted before it.
var resultsSettings = []string{"-public-access", "-ownership", "-encryption", "-policy", "-lifecycle"}

// resultsBucketKind and the kinds of its settings, for Usages.
var (
	resultsBucketKind   = UsageRef{APIVersion: awss3.CRDGroupVersion.String(), Kind: awss3.Bucket_Kind}
	resultsSettingKinds = map[string]UsageRef{
		"-public-access": {APIVersion: awss3.CRDGroupVersion.String(), Kind: awss3.BucketPublicAccessBlock_Kind},
		"-ownership":     {APIVersion: awss3.CRDGroupVersion.String(), Kind: awss3.BucketOwnershipControls_Kind},
		"-encryption":    {APIVersion: awss3.CRDGroupVersion.String(), Kind: awss3.BucketServerSideEncryptionConfiguration_Kind},
		"-policy":        {APIVersion: awss3.CRDGroupVersion.String(), Kind: awss3.BucketPolicy_Kind},
		"-lifecycle":     {APIVersion: awss3.CRDGroupVersion.String(), Kind: awss3.BucketLifecycleConfiguration_Kind},
	}
)

// IsResultsBucketObject reports whether the object of that kind and name is
// the cluster's results Bucket or one of its settings (the kinds
// resultsLists has): what a deletion keeps until the bucket is emptied.
func IsResultsBucketObject(prefix, kind, name string) bool {
	base := ResultsBucketObjectName(prefix)
	if name == base {
		return kind == resultsBucketKind.Kind
	}
	for _, suffix := range resultsSettings {
		if name == base+suffix {
			return kind == resultsSettingKinds[suffix].Kind
		}
	}
	return false
}

// resultsObjects are the objects above, in creation order; none unless the
// plan has a results bucket.
func (b clusterBuilder) resultsObjects() []client.Object {
	if !b.p.HasResultsBucket() {
		return nil
	}
	return []client.Object{b.resultsBucket(), b.resultsPublicAccess(), b.resultsOwnership(), b.resultsEncryption(),
		b.resultsPolicy(), b.resultsLifecycle()}
}

func (b clusterBuilder) resultsName() string { return ResultsBucketObjectName(b.p.Prefix) }

func (b clusterBuilder) resultsBucket() *awss3.Bucket {
	name := b.resultsName()
	meta := b.meta(name)
	meta.Annotations = map[string]string{ExternalNameAnnotation: b.p.ResultsBucketName()}
	return &awss3.Bucket{ObjectMeta: meta,
		Spec: awss3.BucketSpec{ManagedResourceSpec: b.spec(), ForProvider: awss3.BucketParameters{
			Region: ptr.To(b.p.Region), Tags: b.tags(name),
		}}}
}

func (b clusterBuilder) resultsPublicAccess() *awss3.BucketPublicAccessBlock {
	return &awss3.BucketPublicAccessBlock{ObjectMeta: b.meta(b.resultsName() + "-public-access"),
		Spec: awss3.BucketPublicAccessBlockSpec{ManagedResourceSpec: b.spec(), ForProvider: awss3.BucketPublicAccessBlockParameters{
			Region: ptr.To(b.p.Region), BucketRef: b.ref(b.resultsName()),
			BlockPublicAcls: ptr.To(true), BlockPublicPolicy: ptr.To(true),
			IgnorePublicAcls: ptr.To(true), RestrictPublicBuckets: ptr.To(true),
		}}}
}

func (b clusterBuilder) resultsOwnership() *awss3.BucketOwnershipControls {
	return &awss3.BucketOwnershipControls{ObjectMeta: b.meta(b.resultsName() + "-ownership"),
		Spec: awss3.BucketOwnershipControlsSpec{ManagedResourceSpec: b.spec(), ForProvider: awss3.BucketOwnershipControlsParameters{
			Region: ptr.To(b.p.Region), BucketRef: b.ref(b.resultsName()),
			Rule: &awss3.BucketOwnershipControlsRuleParameters{ObjectOwnership: ptr.To("BucketOwnerEnforced")},
		}}}
}

func (b clusterBuilder) resultsEncryption() *awss3.BucketServerSideEncryptionConfiguration {
	return &awss3.BucketServerSideEncryptionConfiguration{ObjectMeta: b.meta(b.resultsName() + "-encryption"),
		Spec: awss3.BucketServerSideEncryptionConfigurationSpec{ManagedResourceSpec: b.spec(),
			ForProvider: awss3.BucketServerSideEncryptionConfigurationParameters{
				Region: ptr.To(b.p.Region), BucketRef: b.ref(b.resultsName()),
				Rule: []awss3.BucketServerSideEncryptionConfigurationRuleParameters{{
					ApplyServerSideEncryptionByDefault: &awss3.RuleApplyServerSideEncryptionByDefaultParameters{SseAlgorithm: ptr.To("AES256")},
				}},
			}}}
}

func (b clusterBuilder) resultsPolicy() *awss3.BucketPolicy {
	return &awss3.BucketPolicy{ObjectMeta: b.meta(b.resultsName() + "-policy"),
		Spec: awss3.BucketPolicySpec{ManagedResourceSpec: b.spec(), ForProvider: awss3.BucketPolicyParameters{
			Region: ptr.To(b.p.Region), BucketRef: b.ref(b.resultsName()),
			Policy: ptr.To(resultsPolicyDocument(Partition(b.p.Region), b.p.ResultsBucketName())),
		}}}
}

// resultsPolicyDocument refuses every request to the bucket and its objects
// that is not over TLS.
func resultsPolicyDocument(partition, bucket string) string {
	arn := "arn:" + partition + ":s3:::" + bucket
	raw, _ := json.Marshal(map[string]any{"Version": "2012-10-17", "Statement": []any{
		map[string]any{"Sid": "DenyInsecureTransport", "Effect": "Deny", "Principal": "*", "Action": "s3:*",
			"Resource":  []string{arn, arn + "/*"},
			"Condition": map[string]any{"Bool": map[string]any{"aws:SecureTransport": "false"}}},
	}}) // plain values: cannot fail
	return string(raw)
}

func (b clusterBuilder) resultsLifecycle() *awss3.BucketLifecycleConfiguration {
	return &awss3.BucketLifecycleConfiguration{ObjectMeta: b.meta(b.resultsName() + "-lifecycle"),
		Spec: awss3.BucketLifecycleConfigurationSpec{ManagedResourceSpec: b.spec(), ForProvider: awss3.BucketLifecycleConfigurationParameters{
			Region: ptr.To(b.p.Region), BucketRef: b.ref(b.resultsName()),
			Rule: []awss3.BucketLifecycleConfigurationRuleParameters{
				{ID: ptr.To("expire-history"), Status: ptr.To("Enabled"),
					Filter:     []awss3.RuleFilterParameters{{Prefix: ptr.To(resultsHistoryPrefix)}},
					Expiration: []awss3.RuleExpirationParameters{{Days: ptr.To(float64(ResultsHistoryDays))}}},
				{ID: ptr.To("abort-incomplete-multipart"), Status: ptr.To("Enabled"),
					Filter: []awss3.RuleFilterParameters{{Prefix: ptr.To("")}},
					AbortIncompleteMultipartUpload: []awss3.AbortIncompleteMultipartUploadParameters{
						{DaysAfterInitiation: ptr.To(float64(resultsAbortMultipartDays))}}},
			},
		}}}
}

// resultsUsagePairs keep the bucket while its settings exist: they go
// first, the bucket (emptied by the controller) last.
func (b clusterBuilder) resultsUsagePairs() []UsagePair {
	if !b.p.HasResultsBucket() {
		return nil
	}
	bucket := resultsBucketKind.named(b.resultsName())
	pairs := make([]UsagePair, 0, len(resultsSettings))
	for _, s := range resultsSettings {
		pairs = append(pairs, UsagePair{Of: bucket, By: resultsSettingKinds[s].named(b.resultsName() + s)})
	}
	return pairs
}

// resultsLists are empty lists of the kinds above, for ClusterLists.
func resultsLists() []client.ObjectList {
	return []client.ObjectList{&awss3.BucketList{}, &awss3.BucketPublicAccessBlockList{}, &awss3.BucketOwnershipControlsList{},
		&awss3.BucketServerSideEncryptionConfigurationList{}, &awss3.BucketPolicyList{}, &awss3.BucketLifecycleConfigurationList{}}
}

// ResultsBucketBelongsTo reports whether bucket is the results bucket of the
// cluster with this account, region and ID: a name of the shape
// ResultsBucketName makes, ending with the hash of those three. The prefix
// in between is not looked at, so it is true of a bucket named for the same
// cluster under another prefix. The API server checks it before it reads a
// bucket a cluster's summary names.
func ResultsBucketBelongsTo(bucket, accountID, region, clusterID string) bool {
	if len(bucket) > maxBucketName || !strings.HasPrefix(bucket, bucketNamePrefix) {
		return false
	}
	want := ResultsBucketName("", accountID, region, clusterID) // cv-<hash>
	hash := strings.TrimPrefix(want, bucketNamePrefix)
	rest, ok := strings.CutSuffix(bucket, hash)
	if !ok {
		return false
	}
	rest = strings.TrimPrefix(rest, bucketNamePrefix)
	return rest == "" || strings.HasSuffix(rest, "-") && bucketPrefixOnly.MatchString(strings.TrimSuffix(rest, "-"))
}

// bucketPrefixOnly is what ResultsBucketName leaves of a prefix.
var bucketPrefixOnly = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
