package network

import (
	"fmt"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/yindia/cluster-vending/pkg/database/model"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// Lenient reads stored JSON: fields the contract dropped since (e.g.
// spec.tags, now labels; spec.aws) are skipped, not errors.
var Lenient = protojson.UnmarshalOptions{DiscardUnknown: true}

// SpecOf decodes the stored spec.
func SpecOf(row *model.Network) (*cloudv1.NetworkSpec, error) {
	spec := &cloudv1.NetworkSpec{}
	if err := Lenient.Unmarshal(row.Spec, spec); err != nil {
		return nil, fmt.Errorf("network %s: stored spec: %w", row.ID, err)
	}
	return spec, nil
}

// Encode is m as stored JSON (protojson).
func Encode(m proto.Message) (model.JSON, error) {
	raw, err := protojson.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("encode %T: %w", m, err)
	}
	return model.JSON(raw), nil
}

// ToProto is the API view of a stored network with its observed status.
func ToProto(row *model.Network, spec *cloudv1.NetworkSpec, st *cloudv1.NetworkStatus) *cloudv1.Network {
	return &cloudv1.Network{
		TenantId:    row.TenantID,
		Id:          row.ID,
		Name:        row.Name,
		DisplayName: row.DisplayName,
		Labels:      row.Labels,
		Spec:        spec,
		Status:      st,
		Generation:  row.Generation,
		Etag:        row.ETag,
		CreateTime:  timestamppb.New(row.CreatedAt),
		UpdateTime:  timestamppb.New(row.UpdatedAt),
	}
}
