package kubestore

import (
	"encoding/json"
	"errors"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	platformv1alpha1 "github.com/yindia/cluster-vending/pkg/apis/platform/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/model"
)

// toJSON is a JSON document column as an object field; nil for none.
func toJSON(j model.JSON) (*apiextensionsv1.JSON, error) {
	if len(j) == 0 {
		return nil, nil
	}
	if !json.Valid(j) {
		return nil, errors.New("model.JSON: invalid JSON document")
	}
	return &apiextensionsv1.JSON{Raw: append([]byte(nil), j...)}, nil
}

func fromJSON(j *apiextensionsv1.JSON) model.JSON {
	if j == nil || len(j.Raw) == 0 {
		return nil
	}
	return append(model.JSON(nil), j.Raw...)
}

func micro(t time.Time) metav1.MicroTime { return metav1.NewMicroTime(t) }

// toRow is the shared columns of a row as stored.
func toRow(m *model.Resource) (platformv1alpha1.Row, error) {
	spec, err := toJSON(m.Spec)
	if err != nil {
		return platformv1alpha1.Row{}, err
	}
	status, err := toJSON(m.Status)
	if err != nil {
		return platformv1alpha1.Row{}, err
	}
	return platformv1alpha1.Row{
		DisplayName: m.DisplayName, Labels: m.Labels, State: m.State,
		Generation: m.Generation, ObservedGeneration: m.ObservedGeneration,
		AppliedGeneration: m.AppliedGeneration, RenderVersion: m.RenderVersion,
		ETag: m.ETag, Spec: spec, Status: status,
		CreatedAt: micro(m.CreatedAt), UpdatedAt: micro(m.UpdatedAt),
	}, nil
}

// fromRow is the shared columns of the row named id. Labels are never
// nil, as the database returns them.
func fromRow(id string, r platformv1alpha1.Row) model.Resource {
	labels := model.Labels{}
	for k, v := range r.Labels {
		labels[k] = v
	}
	return model.Resource{
		Base:        model.Base{ID: id, CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time},
		DisplayName: r.DisplayName, Labels: labels, State: r.State,
		Generation: r.Generation, ObservedGeneration: r.ObservedGeneration,
		AppliedGeneration: r.AppliedGeneration, RenderVersion: r.RenderVersion,
		ETag: r.ETag, Spec: fromJSON(r.Spec), Status: fromJSON(r.Status),
	}
}

func toNetwork(n *model.Network) (platformv1alpha1.NetworkSpec, error) {
	row, err := toRow(&n.Resource)
	if err != nil {
		return platformv1alpha1.NetworkSpec{}, err
	}
	input, err := toJSON(n.Input)
	if err != nil {
		return platformv1alpha1.NetworkSpec{}, err
	}
	return platformv1alpha1.NetworkSpec{Row: row, TenantID: n.TenantID, Name: n.Name, Input: input,
		CloudAccountID: n.CloudAccountID, Provider: string(n.Provider), Region: n.Region,
		ExternalID: n.ExternalID, CreateRequestID: n.CreateRequestID}, nil
}

func fromNetwork(o *platformv1alpha1.Network) *model.Network {
	s := o.Spec
	return &model.Network{Resource: fromRow(o.Name, s.Row), TenantID: s.TenantID, Name: s.Name, Input: fromJSON(s.Input),
		CloudAccountID: s.CloudAccountID, Provider: model.Provider(s.Provider), Region: s.Region,
		ExternalID: s.ExternalID, CreateRequestID: s.CreateRequestID}
}

func networkLabels(n *model.Network) map[string]string {
	return map[string]string{platformv1alpha1.LabelTenant: n.TenantID, platformv1alpha1.LabelCloudAccount: n.CloudAccountID}
}

func toCluster(c *model.Cluster) (platformv1alpha1.ClusterSpec, error) {
	row, err := toRow(&c.Resource)
	if err != nil {
		return platformv1alpha1.ClusterSpec{}, err
	}
	input, err := toJSON(c.Input)
	if err != nil {
		return platformv1alpha1.ClusterSpec{}, err
	}
	adoption, err := toJSON(c.Adoption)
	if err != nil {
		return platformv1alpha1.ClusterSpec{}, err
	}
	return platformv1alpha1.ClusterSpec{Row: row, TenantID: c.TenantID, Name: c.Name, Input: input,
		NetworkID: c.NetworkID, CloudAccountID: c.CloudAccountID, Provider: string(c.Provider), Region: c.Region,
		Imported: c.Imported, Adoption: adoption, CreateRequestID: c.CreateRequestID, DeleteForce: c.DeleteForce}, nil
}

func fromCluster(o *platformv1alpha1.Cluster) *model.Cluster {
	s := o.Spec
	return &model.Cluster{Resource: fromRow(o.Name, s.Row), TenantID: s.TenantID, Name: s.Name, Input: fromJSON(s.Input),
		NetworkID: s.NetworkID, CloudAccountID: s.CloudAccountID, Provider: model.Provider(s.Provider), Region: s.Region,
		Imported: s.Imported, Adoption: fromJSON(s.Adoption), CreateRequestID: s.CreateRequestID, DeleteForce: s.DeleteForce}
}

func clusterLabels(c *model.Cluster) map[string]string {
	return map[string]string{platformv1alpha1.LabelTenant: c.TenantID, platformv1alpha1.LabelNetwork: c.NetworkID}
}

func toNodePool(p *model.NodePool) (platformv1alpha1.NodePoolSpec, error) {
	row, err := toRow(&p.Resource)
	if err != nil {
		return platformv1alpha1.NodePoolSpec{}, err
	}
	return platformv1alpha1.NodePoolSpec{Row: row, ClusterID: p.ClusterID, Name: p.Name,
		CarriedOver: p.CarriedOver, CreateRequestID: p.CreateRequestID}, nil
}

func fromNodePool(o *platformv1alpha1.NodePool) *model.NodePool {
	s := o.Spec
	return &model.NodePool{Resource: fromRow(o.Name, s.Row), ClusterID: s.ClusterID, Name: s.Name,
		CarriedOver: s.CarriedOver, CreateRequestID: s.CreateRequestID}
}

func fromTenant(o *platformv1alpha1.Tenant) *model.Tenant {
	return &model.Tenant{ID: o.Name, DisplayName: o.Spec.DisplayName,
		CreatedAt: o.Spec.CreatedAt.Time, UpdatedAt: o.Spec.UpdatedAt.Time}
}

func toAccount(a *model.CloudAccount) platformv1alpha1.CloudAccountSpec {
	return platformv1alpha1.CloudAccountSpec{TenantID: a.TenantID, Provider: string(a.Provider), AccountID: a.AccountID,
		ProviderConfigName: a.ProviderConfigName, Namespace: a.Namespace, Identity: a.Identity,
		DirectoryID: a.DirectoryID, AccountName: a.AccountName,
		CreatedAt: micro(a.CreatedAt), UpdatedAt: micro(a.UpdatedAt)}
}

func fromAccount(o *platformv1alpha1.CloudAccount) *model.CloudAccount {
	s := o.Spec
	return &model.CloudAccount{Base: model.Base{ID: o.Name, CreatedAt: s.CreatedAt.Time, UpdatedAt: s.UpdatedAt.Time},
		TenantID: s.TenantID, Provider: model.Provider(s.Provider), AccountID: s.AccountID,
		ProviderConfigName: s.ProviderConfigName, Namespace: s.Namespace, Identity: s.Identity,
		DirectoryID: s.DirectoryID, AccountName: s.AccountName}
}

func fromManaged(o *platformv1alpha1.ManagedResource) model.ManagedResource {
	s := o.Spec
	out := model.ManagedResource{Base: model.Base{ID: o.Name, CreatedAt: s.CreatedAt.Time, UpdatedAt: s.UpdatedAt.Time},
		TenantID: s.TenantID, CloudAccountID: s.CloudAccountID, OwnerKind: model.OwnerKind(s.OwnerKind), OwnerID: s.OwnerID,
		APIVersion: s.APIVersion, Kind: s.Kind, Namespace: s.Namespace, Name: s.Name,
		ExternalName: s.ExternalName, CarriedOver: s.CarriedOver}
	if s.GoneAt != nil {
		at := s.GoneAt.Time
		out.GoneAt = &at
	}
	return out
}
