package cli

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// maxRecommendationPages bounds how many pages one command reads.
const maxRecommendationPages = 20

// ClusterRecommendations calls ClusterService.ListClusterRecommendations
// for every page: what the platform recommends the cluster's containers
// ask for. With --output table it prints a table itself; else the result
// is printed as YAML or JSON.
func ClusterRecommendations(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	all := &cloudv1.ListClusterRecommendationsResponse{}
	token := o.PageToken
	for range maxRecommendationPages {
		res, err := call(ctx, &cloudv1.ListClusterRecommendationsRequest{TenantId: o.Tenant, ClusterId: o.ID,
			Namespace: o.Namespace, OnlyChanges: o.OnlyChanges, PageSize: o.PageSize, PageToken: token},
			c.Cluster.ListClusterRecommendations)
		if err != nil {
			return nil, err
		}
		page := res.(*cloudv1.ListClusterRecommendationsResponse)
		rows := append(all.GetRecommendations(), page.GetRecommendations()...)
		all = proto.Clone(page).(*cloudv1.ListClusterRecommendationsResponse)
		all.Recommendations, all.NextPageToken = rows, ""
		if token = page.GetNextPageToken(); token == "" || o.PageSize > 0 {
			all.NextPageToken = token // one page was asked for: say where the next starts
			break
		}
	}
	if !isTable(o.Output) {
		return all, nil
	}
	return nil, printRecommendations(o.Out, all)
}

// printRecommendations writes one line of what is recorded, then a row
// per container.
func printRecommendations(w io.Writer, res *cloudv1.ListClusterRecommendationsResponse) error {
	sum := res.GetSummary()
	if sum.GetSampledAt() == nil && len(res.GetRecommendations()) == 0 {
		msg := "The cluster was not sampled yet: the controller samples running clusters every five minutes."
		if sum.GetError() != "" {
			msg = "The cluster could not be sampled: " + sum.GetError()
		}
		_, err := fmt.Fprintln(w, msg)
		return err
	}
	if _, err := fmt.Fprintf(w, "Sampled %s: %d containers of %d workloads, %d days kept; metrics-server: %s; VPA: %s\n",
		sum.GetSampledAt().AsTime().UTC().Format(time.RFC3339), sum.GetContainers(), sum.GetWorkloads(), sum.GetWindowDays(),
		yesNo(sum.GetMetricsAvailable()), yesNo(sum.GetVpaAvailable())); err != nil {
		return err
	}
	for _, line := range []string{sum.GetError(), sum.GetMessage()} {
		if line != "" {
			if _, err := fmt.Fprintln(w, line); err != nil {
				return err
			}
		}
	}
	tw := newTable(w)
	if _, err := fmt.Fprintln(tw, "NAMESPACE\tWORKLOAD\tCONTAINER\tCPU REQ\tMEM REQ\tMEM LIMIT\tSOURCE\tDATA"); err != nil {
		return err
	}
	for _, r := range res.GetRecommendations() {
		if _, err := fmt.Fprintf(tw, "%s\t%s/%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.GetNamespace(), strings.ToLower(r.GetWorkloadKind()),
			r.GetWorkloadName(), r.GetContainer(),
			change(r.GetCurrentRequests().GetCpuMillis(), r.GetRecommendedRequests().GetCpuMillis(), r.GetCpuPercent(), millicores),
			change(r.GetCurrentRequests().GetMemoryBytes(), r.GetRecommendedRequests().GetMemoryBytes(), r.GetMemoryPercent(), binary),
			change(r.GetCurrentLimits().GetMemoryBytes(), r.GetRecommendedLimits().GetMemoryBytes(), 0, binary),
			sourceName(r.GetSource()), dataAge(r.GetDataHours())); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// change is "current -> recommended (pct%)"; "-" for an unset current
// value, and the current value alone without a recommendation.
func change(current, recommended int64, pct int32, format func(int64) string) string {
	from := "-"
	if current > 0 {
		from = format(current)
	}
	if recommended == 0 {
		return from
	}
	out := from + " -> " + format(recommended)
	if pct != 0 {
		out += fmt.Sprintf(" (%+d%%)", pct)
	}
	return out
}

func millicores(m int64) string { return strconv.FormatInt(m, 10) + "m" }

// binary is a memory amount in the largest whole binary unit, rounded up.
func binary(b int64) string {
	for _, u := range []struct {
		size int64
		name string
	}{{1 << 30, "Gi"}, {1 << 20, "Mi"}} {
		if b >= u.size {
			if b%u.size == 0 || u.name == "Mi" {
				return strconv.FormatInt((b+u.size-1)/u.size, 10) + u.name
			}
		}
	}
	return strconv.FormatInt(b, 10) + "B"
}

func sourceName(s cloudv1.RecommendationSource) string {
	name := strings.TrimPrefix(s.String(), "RECOMMENDATION_SOURCE_")
	return strings.ReplaceAll(strings.ToLower(name), "_", "-")
}

// dataAge is how long usage is recorded: "5h", "2d"; "-" for none.
func dataAge(hours int32) string {
	switch {
	case hours <= 0:
		return "-"
	case hours < 24:
		return strconv.Itoa(int(hours)) + "h"
	}
	return strconv.Itoa(int(hours/24)) + "d"
}
