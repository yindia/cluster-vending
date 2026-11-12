package aws

import "github.com/yindia/cluster-vending/pkg/crossplane/sequence"

// ClusterDeleteOrder is the order a cluster's objects are deleted in: no
// tiers, every object at once. The Usages rendered with them
// (ClusterResources) order the deletion instead: the webhook holds each
// object until what uses it is gone and replays its deletion then, so
// node groups go before the EKS cluster and the EKS cluster before its
// IAM roles.
var ClusterDeleteOrder = sequence.DeleteOrder{}
