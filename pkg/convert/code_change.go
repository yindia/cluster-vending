package convert

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// Code change phases as the kinds spell them.
const (
	CodeChangePending  = "Pending"
	CodeChangeApplying = "Applying"
	CodeChangeDone     = "Done"
	CodeChangeHalted   = "Halted"
)

// CodeChangeToAPI is the contract's code change as the kinds report it;
// nil when unset.
func CodeChangeToAPI(p *cloudv1.CodeChange) *cloudv1alpha1.CodeChange {
	if p == nil {
		return nil
	}
	out := &cloudv1alpha1.CodeChange{Phase: camel(p.GetPhase(), "CODE_CHANGE_PHASE_"), Summary: p.GetSummary(),
		Objects: p.GetObjects(), Message: p.GetMessage()}
	if t := p.GetStartTime(); t != nil {
		out.StartTime = &metav1.Time{Time: t.AsTime()}
	}
	if t := p.GetFinishTime(); t != nil {
		out.FinishTime = &metav1.Time{Time: t.AsTime()}
	}
	return out
}

// maxCatalogPending bounds the pending entries the kind lists
// (CatalogChange.pending, MaxItems).
const maxCatalogPending = 64

// CatalogChangeToAPI is the contract's catalog change as the kinds report
// it; nil when unset. The applied fingerprints stay in contractStatus.
func CatalogChangeToAPI(p *cloudv1.CatalogChange) *cloudv1alpha1.CatalogChange {
	if p == nil {
		return nil
	}
	out := &cloudv1alpha1.CatalogChange{Phase: camel(p.GetPhase(), "CODE_CHANGE_PHASE_"), Summary: p.GetSummary(),
		Component: p.GetComponent(), Step: p.GetStep(), Steps: p.GetSteps(), Message: p.GetMessage()}
	if pending := p.GetPending(); len(pending) > 0 {
		out.Pending = append([]string(nil), pending[:min(len(pending), maxCatalogPending)]...)
	}
	if t := p.GetStartTime(); t != nil {
		out.StartTime = &metav1.Time{Time: t.AsTime()}
	}
	if t := p.GetFinishTime(); t != nil {
		out.FinishTime = &metav1.Time{Time: t.AsTime()}
	}
	return out
}
