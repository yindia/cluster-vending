package deprecations

import (
	"strings"
	"testing"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func TestScanEvent(t *testing.T) {
	scan := func(target string, total, removed int32, failure string) *cloudv1.ClusterDeprecations {
		return &cloudv1.ClusterDeprecations{TargetVersion: target, Total: total, Removed: removed, Error: failure}
	}
	tests := []struct {
		name         string
		last, result *cloudv1.ClusterDeprecations
		want         string // "Type Reason"; "" for no event
		message      string
	}{
		{name: "the first scan finds nothing", result: scan("1.37", 0, 0, "")},
		{name: "the first scan finds only deprecated versions", result: scan("1.37", 3, 0, "")},
		{name: "the first scan finds removed versions", result: scan("1.37", 5, 2, ""), want: "Warning DeprecatedAPIs",
			message: "2 objects use API versions that Kubernetes 1.37 removes (5 deprecated in all)"},
		{name: "the same findings again", last: scan("1.37", 5, 2, ""), result: scan("1.37", 5, 2, "")},
		{name: "more removed versions", last: scan("1.37", 5, 2, ""), result: scan("1.37", 6, 3, ""), want: "Warning DeprecatedAPIs"},
		{name: "another target", last: scan("1.36", 5, 2, ""), result: scan("1.37", 5, 2, ""), want: "Warning DeprecatedAPIs"},
		{name: "the findings are gone", last: scan("1.37", 5, 2, ""), result: scan("1.37", 3, 0, ""), want: "Normal DeprecationScanClean"},
		{name: "the scan fails", last: scan("1.37", 0, 0, ""), result: scan("1.37", 0, 0, "kubeconfig: secret not found"),
			want: "Warning DeprecationScanFailed", message: "secret not found"},
		{name: "the first scan fails", result: scan("1.37", 0, 0, "unreachable"), want: "Warning DeprecationScanFailed"},
		{name: "it fails again", last: scan("1.37", 0, 0, "unreachable"), result: scan("1.37", 0, 0, "unreachable on another port")},
		{name: "it works again, clean", last: scan("1.37", 0, 0, "unreachable"), result: scan("1.37", 0, 0, ""), want: "Normal DeprecationScanClean"},
		{name: "it works again, with findings", last: scan("1.37", 5, 2, "unreachable"), result: scan("1.37", 5, 2, ""),
			want: "Warning DeprecatedAPIs"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scanEvent(tt.last, tt.result)

			have := ""
			if got != nil {
				have = got.Type + " " + got.Reason
			}
			if have != tt.want {
				t.Fatalf("event = %q, want %q", have, tt.want)
			}
			if tt.message != "" && !strings.Contains(got.Message, tt.message) {
				t.Errorf("message %q lacks %q", got.Message, tt.message)
			}
		})
	}
}
