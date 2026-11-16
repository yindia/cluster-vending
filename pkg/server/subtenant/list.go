package subtenant

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"slices"

	"github.com/google/uuid"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	v1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
)

// Cursors are scoped even though authorization is repeated for every request.
// The complete tenant list is sorted before pagination; no first-page-only view.
type cursor struct {
	Tenant  string `json:"tenant"`
	Cluster string `json:"cluster"`
	Last    string `json:"last"`
}

func (s *Service) ListSubTenants(ctx context.Context, req *v1.ListSubTenantsRequest) (*v1.ListSubTenantsResponse, error) {
	if err := s.access(ctx, req.GetTenantId(), false, req); err != nil {
		return nil, err
	}
	tenant, err := s.tenant(ctx, req.TenantId, false)
	if err != nil {
		return nil, err
	}
	selected := cursor{Tenant: req.TenantId, Cluster: req.ClusterId}
	if req.PageToken != "" {
		raw, err := base64.RawURLEncoding.DecodeString(req.PageToken)
		if err != nil || json.Unmarshal(raw, &selected) != nil || selected.Tenant != req.TenantId || selected.Cluster != req.ClusterId {
			return nil, invalid("page_token is invalid or belongs to another tenant/cluster")
		}
		if _, err := uuid.Parse(selected.Last); err != nil {
			return nil, invalid("page_token contains an invalid cursor")
		}
	}
	items := &cloud.SubTenantList{}
	labels := client.MatchingLabels{cloudstore.LabelTenant: req.TenantId}
	if req.ClusterId != "" {
		labels[cloudstore.LabelClusterID] = req.ClusterId
	}
	if err := s.reader().List(ctx, items, client.InNamespace(kube.TenantNamespace(req.TenantId)), labels); err != nil {
		return nil, kubeError(err)
	}
	slices.SortFunc(items.Items, func(a, b cloud.SubTenant) int {
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})
	size := int(req.PageSize)
	if size == 0 {
		size = 50
	}
	out := &v1.ListSubTenantsResponse{}
	for i := range items.Items {
		obj := &items.Items[i]
		if !belongs(obj, tenant, req.ClusterId) || obj.Name <= selected.Last {
			continue
		}
		if len(out.Subtenants) == size {
			selected.Last = out.Subtenants[len(out.Subtenants)-1].Id
			raw, err := json.Marshal(selected)
			if err != nil {
				return nil, kubeError(err)
			}
			out.NextPageToken = base64.RawURLEncoding.EncodeToString(raw)
			break
		}
		out.Subtenants = append(out.Subtenants, resourceView(obj))
	}
	return out, nil
}
