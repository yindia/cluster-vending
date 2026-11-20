package managed

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestIsUsageRefusal(t *testing.T) {
	denied := func(code int32, reason metav1.StatusReason) error {
		return &apierrors.StatusError{ErrStatus: metav1.Status{Status: metav1.StatusFailure, Code: code, Reason: reason,
			Message: `admission webhook "nousages.protection.crossplane.io" denied the request: This resource is in-use by 1 usage(s)`}}
	}
	cases := map[string]struct {
		err  error
		want bool
	}{
		"nil":                      {nil, false},
		"conflict":                 {apierrors.NewConflict(schema.GroupResource{Resource: "releases"}, "x", errors.New("in use")), true},
		"forbidden by the webhook": {denied(403, metav1.StatusReasonForbidden), true},
		"wrapped":                  {fmt.Errorf("delete Release x: %w", denied(403, metav1.StatusReasonForbidden)), true},
		"other forbidden":          {apierrors.NewForbidden(schema.GroupResource{Resource: "releases"}, "x", errors.New("rbac: no")), false},
		"plain":                    {errors.New("boom"), false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) { require.Equal(t, c.want, IsUsageRefusal(c.err)) })
	}
}
