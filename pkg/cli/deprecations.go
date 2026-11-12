package cli

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"google.golang.org/protobuf/proto"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// OutputTable is --output table: the default of cluster deprecations.
const OutputTable = "table"

// maxDeprecationPages bounds how many pages one command reads.
const maxDeprecationPages = 20

// ClusterDeprecations calls ClusterService.ListClusterDeprecations for
// every page: what the controller's last scan of the cluster found. With
// --output table it prints a table itself; else the result is printed as
// YAML or JSON.
func ClusterDeprecations(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	all := &cloudv1.ListClusterDeprecationsResponse{}
	token := o.PageToken
	for range maxDeprecationPages {
		res, err := call(ctx, &cloudv1.ListClusterDeprecationsRequest{TenantId: o.Tenant, ClusterId: o.ID,
			PageSize: o.PageSize, PageToken: token, OnlyRemoved: o.OnlyRemoved}, c.Cluster.ListClusterDeprecations)
		if err != nil {
			return nil, err
		}
		page := res.(*cloudv1.ListClusterDeprecationsResponse)
		findings := append(all.GetFindings(), page.GetFindings()...)
		all = proto.Clone(page).(*cloudv1.ListClusterDeprecationsResponse)
		all.Findings, all.NextPageToken = findings, ""
		if token = page.GetNextPageToken(); token == "" || o.PageSize > 0 {
			all.NextPageToken = token // one page was asked for: say where the next starts
			break
		}
	}
	if o.Output != OutputTable {
		return all, nil
	}
	return nil, printDeprecations(o.Out, all)
}

// printDeprecations writes the scan as a table: one line of what was
// scanned, then a row per finding.
func printDeprecations(w io.Writer, res *cloudv1.ListClusterDeprecationsResponse) error {
	if res.GetError() != "" {
		if _, err := fmt.Fprintf(w, "The last scan failed: %s\n", res.GetError()); err != nil {
			return err
		}
	}
	if res.GetScannedAt() == nil {
		_, err := fmt.Fprintln(w, "The cluster was not scanned yet (the controller scans a running cluster every six hours).")
		return err
	}
	summary := fmt.Sprintf("Kubernetes %s, scanned %s: %d found, %d removed", res.GetTargetVersion(),
		res.GetScannedAt().AsTime().UTC().Format(time.RFC3339), res.GetTotal(), res.GetRemoved())
	if res.GetTruncated() {
		summary += " (the first 200 are kept)"
	}
	if _, err := fmt.Fprintln(w, summary); err != nil {
		return err
	}
	if len(res.GetFindings()) == 0 {
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "NAMESPACE\tKIND\tNAME\tAPI VERSION\tSTATE\tREMOVED IN\tREPLACEMENT\tSOURCE"); err != nil {
		return err
	}
	for _, f := range res.GetFindings() {
		state := "deprecated"
		if f.GetRemoved() {
			state = "removed"
		}
		source := "manifest"
		if f.GetSource() == cloudv1.DeprecationSource_DEPRECATION_SOURCE_HELM_RELEASE {
			source = "helm " + f.GetHelmRelease()
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", dash(f.GetNamespace()), f.GetKind(), f.GetName(),
			f.GetApiVersion(), state, dash(f.GetRemovedIn()), dash(f.GetReplacementApi()), source); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
