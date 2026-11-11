package service

import (
	"testing"

	"connectrpc.com/connect/v2"
)

func TestCheckAWSLabels(t *testing.T) {
	if err := CheckAWSLabels(map[string]string{"team": "x", "CostCenter": "1"}); err != nil {
		t.Errorf("ordinary labels refused: %v", err)
	}
	for _, key := range []string{"aws:foo", "AWS:Foo"} {
		if err := CheckAWSLabels(map[string]string{key: "x"}); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: %v, want INVALID_ARGUMENT", key, err)
		}
	}
}
