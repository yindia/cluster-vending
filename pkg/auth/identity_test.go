package auth_test

import (
	"context"
	"testing"

	"github.com/yindia/cluster-vending/pkg/auth"
)

func TestIdentityTravelsInTheContext(t *testing.T) {
	if id := auth.FromContext(context.Background()); !id.Anonymous {
		t.Errorf("a context without a caller = %+v, want anonymous", id)
	}
	want := &auth.Identity{Subject: "u1"}

	got := auth.FromContext(auth.WithIdentity(context.Background(), want))

	if got != want {
		t.Errorf("FromContext = %+v", got)
	}
}
