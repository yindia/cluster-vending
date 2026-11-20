//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

// leakWait is how long the leak check retries: AWS tag queries and the
// management cluster lag the deletions a little.
const leakWait = 5 * time.Minute

// leakCheck runs after every cleanup of the run. It lists what the run
// created and left: managed resources on the management cluster, and (with
// credentials of the account) AWS resources carrying the run's tag or the
// ids of its networks and clusters. A leak fails the run, and is logged with
// enough to delete it by hand.
func (e *env) leakCheck(t *testing.T) {
	t.Helper()
	checked := false
	var left []string
	err := e.waitFor(t, "leak check", leakWait, func() (bool, string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		left, checked = nil, false
		if e.haveKube() {
			checked = true
			l, err := e.leakedManaged(ctx)
			if err != nil {
				return false, "", err
			}
			left = append(left, l...)
		}
		if e.haveAWS(t) {
			checked = true
			l, err := e.leakedAWS(ctx)
			if err != nil {
				return false, "", err
			}
			left = append(left, l...)
		}
		return len(left) == 0, fmt.Sprintf("%d left", len(left)), nil
	})
	switch {
	case !checked:
		t.Log("leak check skipped: no management cluster (CV_E2E_KUBECONTEXT) and no AWS credentials of the account")
	case len(left) > 0:
		sort.Strings(left)
		for _, l := range left {
			t.Logf("LEAK: %s", l)
		}
		t.Errorf("%d resources of run %s were left behind", len(left), e.runID)
	case err != nil:
		t.Errorf("%v", err)
	default:
		t.Logf("leak check: nothing of run %s is left", e.runID)
	}
}

// ours says whether a name belongs to the run: everything it creates is
// named after the run id.
func (e *env) ours(name string) bool { return strings.HasPrefix(name, e.runID) }

// leakedManaged lists the run's managed resources still on the management
// cluster. Resources are named <name>-<hash>-<suffix>, and the names the run
// gave start with its id.
func (e *env) leakedManaged(ctx context.Context) ([]string, error) {
	obj, err := e.kubeJSON(ctx, "managed", "-n", e.cfg.namespace())
	if err != nil {
		return nil, fmt.Errorf("list managed resources: %w", err)
	}
	var out []string
	for _, it := range asList(at(obj, "items")) {
		name := asString(at(it, "metadata", "name"))
		if e.ours(name) {
			out = append(out, fmt.Sprintf("managed resource %s %s (external name %s)", asString(at(it, "kind")), name,
				asString(at(it, "metadata", "annotations", "crossplane.io/external-name"))))
		}
	}
	return out, nil
}

// leakedAWS lists the AWS resources tagged cluster-vending.io/tenant that
// belong to the run: its own tag, or the id of one of its networks or
// clusters in a platform tag. Other resources of the tenant are not the
// run's and are only logged. IAM is global: it is read in us-east-1.
func (e *env) leakedAWS(ctx context.Context) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, region := range slices.Compact([]string{e.cfg.Region, "us-east-1"}) {
		res, err := e.awsJSON(ctx, region, "resourcegroupstaggingapi", "get-resources",
			"--tag-filters", "Key=cluster-vending.io/tenant,Values="+e.cfg.Tenant)
		if err != nil {
			return nil, fmt.Errorf("tag query in %s: %w", region, err)
		}
		for _, m := range asList(res["ResourceTagMappingList"]) {
			arn := asString(at(m, "ResourceARN"))
			if seen[arn] {
				continue
			}
			seen[arn] = true
			if e.runOwns(m) {
				out = append(out, "AWS "+arn)
			}
		}
	}
	// EKS clusters are found by name too: a cluster without tags yet is a leak.
	for _, name := range []string{e.st.clusterName} {
		if name == "" {
			continue
		}
		if _, err := e.awsJSON(ctx, e.cfg.Region, "eks", "describe-cluster", "--name", name); err == nil {
			out = append(out, "AWS EKS cluster "+name)
		} else if !awsNotFound(err) {
			return nil, fmt.Errorf("describe-cluster %s: %w", name, err)
		}
	}
	return out, nil
}

// runOwns says whether a tagged resource is the run's.
func (e *env) runOwns(mapping any) bool {
	for _, tg := range asList(at(mapping, "Tags")) {
		key, val := asString(at(tg, "Key")), asString(at(tg, "Value"))
		switch {
		case key == labelRun && val == e.runID:
			return true
		case (key == "cluster-vending.io/cluster" || key == "cluster-vending.io/network") &&
			(slices.Contains(e.st.allClusterIDs, val) || slices.Contains(e.st.allNetworkIDs, val)):
			return true
		}
	}
	return false
}
