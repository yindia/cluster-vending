package aws_test

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	protectionv1beta1 "github.com/crossplane/crossplane/apis/v2/protection/v1beta1"
	awss3 "github.com/upbound/provider-aws/v2/apis/namespaced/s3/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

var bucketNamePattern = regexp.MustCompile(`^cv-[a-z0-9][a-z0-9-]*-[0-9a-f]{12}$`)

func bucketPlan() crossplaneaws.ClusterPlan {
	p := clusterPlan()
	p.AccountID, p.ResultsBucket = "111122223333", true
	return p
}

func TestResultsBucketName(t *testing.T) {
	name := crossplaneaws.ResultsBucketName("prod-1a2b3c4d", "111122223333", "us-east-1", "c-1")
	if !bucketNamePattern.MatchString(name) || len(name) > 63 {
		t.Fatalf("name %q is not a valid cv-<prefix>-<12 hex> bucket name", name)
	}
	if again := crossplaneaws.ResultsBucketName("prod-1a2b3c4d", "111122223333", "us-east-1", "c-1"); again != name {
		t.Errorf("not deterministic: %q then %q", name, again)
	}
	for label, other := range map[string]string{
		"account": crossplaneaws.ResultsBucketName("prod-1a2b3c4d", "999988887777", "us-east-1", "c-1"),
		"region":  crossplaneaws.ResultsBucketName("prod-1a2b3c4d", "111122223333", "eu-west-1", "c-1"),
		"cluster": crossplaneaws.ResultsBucketName("prod-1a2b3c4d", "111122223333", "us-east-1", "c-2"),
	} {
		if other == name {
			t.Errorf("a different %s gives the same bucket name %q", label, name)
		}
	}
}

func TestResultsBucketNameFitsAndIsLowerCase(t *testing.T) {
	for _, prefix := range []string{
		strings.Repeat("a-very-long-cluster-name-", 5), "UPPER_Case.name", "---edge---", "ünï-cödé", "",
	} {
		name := crossplaneaws.ResultsBucketName(prefix, "111122223333", "us-east-1", "c-1")
		if len(name) > 63 || len(name) < 3 || name != strings.ToLower(name) || strings.Contains(name, "--") ||
			!regexp.MustCompile(`^[a-z0-9][a-z0-9-]*[a-z0-9]$`).MatchString(name) || !strings.HasPrefix(name, "cv-") {
			t.Errorf("prefix %q gives invalid bucket name %q (%d)", prefix, name, len(name))
		}
	}
}

func TestResultsBucketIsRenderedWithTheCluster(t *testing.T) {
	p := bucketPlan()
	objects := byName(crossplaneaws.ClusterResources(p))
	want := crossplaneaws.ResultsBucketName(p.Prefix, p.AccountID, p.Region, p.ClusterID)

	bucket, ok := objects[crossplaneaws.ResultsBucketObjectName(p.Prefix)].(*awss3.Bucket)
	if !ok {
		t.Fatalf("no Bucket; objects %v", keysOf(objects))
	}
	if bucket.Annotations[crossplaneaws.ExternalNameAnnotation] != want {
		t.Errorf("bucket external name = %q, want %q", bucket.Annotations[crossplaneaws.ExternalNameAnnotation], want)
	}
	if fd := bucket.Spec.ForProvider.ForceDestroy; fd != nil && *fd {
		t.Error("the bucket must not force-destroy: the controller empties it, a bucket with objects is never silently wiped")
	}
	if got := bucket.Spec.ForProvider.Tags[crossplaneaws.TagCluster]; got == nil || *got != "c-1" {
		t.Errorf("bucket tags = %v, want the cluster tag", bucket.Spec.ForProvider.Tags)
	}

	prefix := p.Prefix + "-results"
	pab := objects[prefix+"-public-access"].(*awss3.BucketPublicAccessBlock).Spec.ForProvider
	for name, v := range map[string]*bool{"blockPublicAcls": pab.BlockPublicAcls, "blockPublicPolicy": pab.BlockPublicPolicy,
		"ignorePublicAcls": pab.IgnorePublicAcls, "restrictPublicBuckets": pab.RestrictPublicBuckets} {
		if v == nil || !*v {
			t.Errorf("public access block %s is not on", name)
		}
	}
	own := objects[prefix+"-ownership"].(*awss3.BucketOwnershipControls).Spec.ForProvider
	if own.Rule == nil || *own.Rule.ObjectOwnership != "BucketOwnerEnforced" {
		t.Errorf("ownership rule = %+v, want BucketOwnerEnforced", own.Rule)
	}
	sse := objects[prefix+"-encryption"].(*awss3.BucketServerSideEncryptionConfiguration).Spec.ForProvider
	if len(sse.Rule) != 1 || sse.Rule[0].ApplyServerSideEncryptionByDefault == nil ||
		*sse.Rule[0].ApplyServerSideEncryptionByDefault.SseAlgorithm != "AES256" {
		t.Errorf("encryption rules = %+v, want one AES256 rule", sse.Rule)
	}
	for _, obj := range []client.Object{objects[prefix+"-public-access"], objects[prefix+"-ownership"], objects[prefix+"-encryption"],
		objects[prefix+"-policy"], objects[prefix+"-lifecycle"]} {
		if obj == nil {
			t.Fatalf("a bucket setting is missing; objects %v", keysOf(objects))
		}
	}
}

func TestResultsBucketPolicyDeniesPlainHTTP(t *testing.T) {
	p := bucketPlan()
	policy := byName(crossplaneaws.ClusterResources(p))[crossplaneaws.ResultsBucketObjectName(p.Prefix)+"-policy"].(*awss3.BucketPolicy)
	var doc struct {
		Statement []struct {
			Effect, Principal string
			Action            any
			Resource          []string
			Condition         map[string]map[string]string
		}
	}
	if err := json.Unmarshal([]byte(*policy.Spec.ForProvider.Policy), &doc); err != nil {
		t.Fatal(err)
	}
	name := crossplaneaws.ResultsBucketName(p.Prefix, p.AccountID, p.Region, p.ClusterID)
	if len(doc.Statement) != 1 || doc.Statement[0].Effect != "Deny" || doc.Statement[0].Principal != "*" ||
		doc.Statement[0].Condition["Bool"]["aws:SecureTransport"] != "false" {
		t.Fatalf("policy = %s, want one Deny of non-TLS requests", *policy.Spec.ForProvider.Policy)
	}
	if got := strings.Join(doc.Statement[0].Resource, ","); got != "arn:aws:s3:::"+name+",arn:aws:s3:::"+name+"/*" {
		t.Errorf("policy resources = %s, want the bucket and its objects", got)
	}
}

func TestResultsBucketLifecycle(t *testing.T) {
	p := bucketPlan()
	lc := byName(crossplaneaws.ClusterResources(p))[crossplaneaws.ResultsBucketObjectName(p.Prefix)+"-lifecycle"].(*awss3.BucketLifecycleConfiguration).Spec.ForProvider
	var expire, abort bool
	for _, r := range lc.Rule {
		if *r.Status != "Enabled" {
			t.Errorf("rule %s is %s", *r.ID, *r.Status)
		}
		for _, e := range r.Expiration {
			if r.Filter[0].Prefix == nil || *r.Filter[0].Prefix != "history/" || *e.Days != 30 {
				t.Errorf("expiration rule = %+v, want history/ after 30 days", r)
			}
			expire = true
		}
		for _, a := range r.AbortIncompleteMultipartUpload {
			if *a.DaysAfterInitiation != 1 {
				t.Errorf("abort multipart after %v days, want 1", *a.DaysAfterInitiation)
			}
			abort = true
		}
	}
	if !expire || !abort {
		t.Errorf("lifecycle rules %+v: expire %v, abort multipart %v", lc.Rule, expire, abort)
	}
}

func TestResultsBucketIsNotRenderedWhenItCannotBeNamedOrIsNotOurs(t *testing.T) {
	for name, mutate := range map[string]func(*crossplaneaws.ClusterPlan){
		"not asked for":      func(p *crossplaneaws.ClusterPlan) { p.ResultsBucket = false },
		"no account":         func(p *crossplaneaws.ClusterPlan) { p.AccountID = "" },
		"imported cluster":   func(p *crossplaneaws.ClusterPlan) { p.ClusterRoleName = "tf-cluster" },
		"adopted by control": func(p *crossplaneaws.ClusterPlan) { p.ExternalNames = map[string]string{"x": "y"} },
	} {
		p := bucketPlan()
		mutate(&p)
		for _, o := range crossplaneaws.ClusterResources(p) {
			if strings.Contains(o.GetName(), "-results") {
				t.Errorf("%s: rendered %s", name, o.GetName())
			}
		}
		if got := p.ResultsBucketName(); got != "" {
			t.Errorf("%s: ResultsBucketName() = %q, want empty", name, got)
		}
	}
	if got := bucketPlan().ResultsBucketName(); !bucketNamePattern.MatchString(got) {
		t.Errorf("ResultsBucketName() = %q", got)
	}
}

// The bucket outlives its settings: they are deleted first (Usages), so a
// setting is never recreated against a bucket on its way out.
func TestResultsBucketUsagesKeepTheBucketUntilItsSettingsAreGone(t *testing.T) {
	p := bucketPlan()
	bucket := crossplaneaws.ResultsBucketObjectName(p.Prefix)
	users := map[string]bool{}
	for _, o := range crossplaneaws.ClusterResources(p) {
		if u := usageOf(o); u != nil && u.Of == bucket {
			users[u.By] = true
		}
	}
	for _, suffix := range []string{"-public-access", "-ownership", "-encryption", "-policy", "-lifecycle"} {
		if !users[bucket+suffix] {
			t.Errorf("no Usage keeps the bucket while %s exists; users %v", bucket+suffix, users)
		}
	}
}

// The settings are applied with the bucket: they refer to it (bucketRef) and
// the provider resolves the reference when the bucket has its name, so
// nothing in the set waits, and a bucket that cannot be created (a name taken
// elsewhere) holds nothing else of the cluster back.
func TestResultsBucketSettingsDoNotHoldTheSetBack(t *testing.T) {
	found := 0
	for _, item := range crossplaneaws.ClusterSet(bucketPlan()) {
		if strings.Contains(item.Object.GetName(), "-results") {
			found++
			if len(item.After) != 0 {
				t.Errorf("%s waits for %d conditions, want none", item.Object.GetName(), len(item.After))
			}
		}
	}
	if found < 6+5 { // the bucket, its five settings and their Usages
		t.Errorf("%d bucket objects in the set, want the bucket, five settings and five Usages", found)
	}
}

func TestResultsBucketFieldsAreClassified(t *testing.T) {
	for kind, fields := range map[string][]string{
		"Bucket":                                  {"region", "tags.x"},
		"BucketPublicAccessBlock":                 {"region", "blockPublicAcls", "restrictPublicBuckets"},
		"BucketOwnershipControls":                 {"region", "rule.objectOwnership"},
		"BucketServerSideEncryptionConfiguration": {"region", "rule"},
		"BucketPolicy":                            {"region", "policy"},
		"BucketLifecycleConfiguration":            {"region", "rule"},
	} {
		for _, f := range fields {
			if _, ok := crossplaneaws.ClassifyField(kind, f); !ok {
				t.Errorf("%s.%s is not classified", kind, f)
			}
		}
	}
	if c, _ := crossplaneaws.ClassifyField("Bucket", "region"); c != crossplaneaws.ChangeImmutable {
		t.Error("the bucket's region cannot change in place")
	}
	if c, _ := crossplaneaws.ClassifyField("Bucket", "tags.team"); c != crossplaneaws.ChangeInPlace {
		t.Error("bucket tags change in place")
	}
}

type usageRefs struct{ Of, By string }

func usageOf(o client.Object) *usageRefs {
	if u, ok := o.(*protectionv1beta1.Usage); ok && u.Spec.By != nil && u.Spec.By.ResourceRef != nil {
		return &usageRefs{Of: u.Spec.Of.ResourceRef.Name, By: u.Spec.By.ResourceRef.Name}
	}
	return nil
}

func keysOf(m map[string]client.Object) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// A bucket is a cluster's own when its name ends with the hash of the
// cluster's account, region and ID: what the API server checks before it
// reads a bucket a summary names.
func TestResultsBucketBelongsTo(t *testing.T) {
	name := crossplaneaws.ResultsBucketName("prod-1a2b3c4d", "111122223333", "us-east-1", "c-1")
	for desc, tt := range map[string]struct {
		bucket, account, region, cluster string
		want                             bool
	}{
		"its own":          {name, "111122223333", "us-east-1", "c-1", true},
		"another cluster":  {name, "111122223333", "us-east-1", "c-2", false},
		"another account":  {name, "999988887777", "us-east-1", "c-1", false},
		"another region":   {name, "111122223333", "eu-west-1", "c-1", false},
		"another prefix":   {crossplaneaws.ResultsBucketName("other", "111122223333", "us-east-1", "c-1"), "111122223333", "us-east-1", "c-1", true},
		"not a cv bucket":  {"somebody-else-" + name[len(name)-12:], "111122223333", "us-east-1", "c-1", false},
		"no hash":          {"cv-prod", "111122223333", "us-east-1", "c-1", false},
		"empty":            {"", "111122223333", "us-east-1", "c-1", false},
		"too long":         {"cv-" + strings.Repeat("a", 60) + name[len(name)-13:], "111122223333", "us-east-1", "c-1", false},
		"suffix of a word": {"cv-prod" + name[len(name)-12:], "111122223333", "us-east-1", "c-1", false},
		"upper case":       {strings.ToUpper(name), "111122223333", "us-east-1", "c-1", false},
	} {
		if got := crossplaneaws.ResultsBucketBelongsTo(tt.bucket, tt.account, tt.region, tt.cluster); got != tt.want {
			t.Errorf("%s: %v, want %v", desc, got, tt.want)
		}
	}
}
