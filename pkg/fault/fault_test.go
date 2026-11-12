package fault_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/yindia/cluster-vending/pkg/fault"
)

func TestKindSurvivesWrapping(t *testing.T) {
	err := fmt.Errorf("apply: %w", fault.Newf(fault.Unavailable, "cannot read %s", "VPC x"))
	if !fault.Is(err, fault.Unavailable) || fault.Is(err, fault.Internal) {
		t.Errorf("kind of %v", err)
	}
	if e, ok := fault.As(err); !ok || e.Message != "cannot read VPC x" {
		t.Errorf("As = %v, %t", e, ok)
	}
	if _, ok := fault.As(errors.New("plain")); ok || fault.Is(nil, fault.Internal) {
		t.Error("a plain error has a kind")
	}
}
