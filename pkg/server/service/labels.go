package service

import "github.com/yindia/cluster-vending/pkg/reconcile"

// CheckAWSLabels refuses labels AWS cannot take as tags
// (reconcile.CheckAWSLabels), as INVALID_ARGUMENT.
func CheckAWSLabels(labels map[string]string) error {
	return ToConnect(reconcile.CheckAWSLabels(labels))
}
