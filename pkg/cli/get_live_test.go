package cli

import (
	"bytes"
	"testing"
)

// cluster get --live asks GetCluster for the status read now; without it,
// the status last recorded.
func TestClusterGetLive(t *testing.T) {
	for _, live := range []bool{false, true} {
		svc := &oneCluster{cluster: runningCluster(nil)}
		var out bytes.Buffer

		err := Run(t.Context(), ClusterGet, Clients{Cluster: svc},
			Options{Tenant: kubeconfigTenant, ID: kubeconfigCluster, Live: live}, &out, OutputYAML)

		if err != nil || len(svc.asked) != 1 || svc.asked[0].GetLive() != live {
			t.Fatalf("live=%v: err %v, asked %v", live, err, svc.asked)
		}
	}
}
