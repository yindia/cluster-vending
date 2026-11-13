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

// ClusterComponents shows the components the platform installed with a
// cluster (ClusterService.GetCluster: status.platform_components), and
// why one is not ready: which chart, Helm's state, the provider's last
// error, how often it failed and since when. With --output table it
// prints a table itself; else the components are printed as YAML or JSON.
func ClusterComponents(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	res, err := call(ctx, &cloudv1.GetClusterRequest{TenantId: o.Tenant, ClusterId: o.ID}, c.Cluster.GetCluster)
	if err != nil {
		return nil, err
	}
	status := res.(*cloudv1.GetClusterResponse).GetCluster().GetStatus()
	if o.Output != OutputTable {
		// The components only: the rest of the status is `cluster get`.
		return &cloudv1.ClusterStatus{PlatformRelease: status.GetPlatformRelease(), PlatformComponents: status.GetPlatformComponents()}, nil
	}
	return nil, printComponents(o.Out, status)
}

// printComponents writes one line of which release the components are
// from, a row per component, and for each one that is not ready what the
// table has no room for.
func printComponents(w io.Writer, status *cloudv1.ClusterStatus) error {
	components := status.GetPlatformComponents()
	if len(components) == 0 {
		_, err := fmt.Fprintln(w, "No platform components (none are installed before the cluster is running, or on an imported cluster).")
		return err
	}
	if _, err := fmt.Fprintf(w, "Release %s\n", dash(status.GetPlatformRelease())); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "NAME\tVERSION\tPHASE\tSINCE\tMESSAGE"); err != nil {
		return err
	}
	for _, c := range components {
		since := "-"
		if c.GetSince() != nil {
			since = c.GetSince().AsTime().UTC().Format(time.RFC3339)
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", c.GetName(), dash(c.GetVersion()), componentPhaseWord(c.GetPhase()),
			since, dash(oneLine(c.GetMessage()))); err != nil {
			return err
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, c := range components {
		if c.GetChart() == "" && c.GetLastError() == "" {
			continue
		}
		if _, err := fmt.Fprintf(w, "\n%s: chart %s, Helm state %s, %d failed attempts, recovered %d times\n", c.GetName(),
			dash(c.GetChart()), dash(c.GetHelmState()), c.GetFailures(), c.GetRecoveries()); err != nil {
			return err
		}
		if c.GetLastError() != "" {
			if _, err := fmt.Fprintf(w, "  last error: %s\n", c.GetLastError()); err != nil {
				return err
			}
		}
	}
	return nil
}

// maxTableMessage is how much of a message a table row shows; the whole
// error follows the table.
const maxTableMessage = 100

// oneLine is s for a table cell: on one line, cut at maxTableMessage.
func oneLine(s string) string {
	out := make([]rune, 0, maxTableMessage)
	for _, r := range s {
		if r == '\n' || r == '\t' || r == '\r' {
			r = ' '
		}
		if len(out) == maxTableMessage {
			return string(out) + "..."
		}
		out = append(out, r)
	}
	return string(out)
}

func componentPhaseWord(p cloudv1.ComponentPhase) string {
	switch p {
	case cloudv1.ComponentPhase_COMPONENT_PHASE_PENDING:
		return "Pending"
	case cloudv1.ComponentPhase_COMPONENT_PHASE_INSTALLING:
		return "Installing"
	case cloudv1.ComponentPhase_COMPONENT_PHASE_READY:
		return "Ready"
	case cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED:
		return "Failed"
	default:
		return "Unknown"
	}
}
