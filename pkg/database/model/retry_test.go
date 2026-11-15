package model

import (
	"testing"
	"time"
)

func TestRetryAnnotationRoundTrip(t *testing.T) {
	at := time.Date(2026, 11, 15, 1, 2, 3, 0, time.UTC)
	r := &Retry{At: at, By: "alice", Components: []string{"karpenter"}, Upgrade: true}

	got := ParseRetry(r.Annotation())

	if got == nil || !got.At.Equal(at) || got.By != "alice" || !got.Has("karpenter") || got.Has("istiod") || !got.Upgrade {
		t.Fatalf("parsed = %+v", got)
	}
}

func TestRetryUnreadableOrEmpty(t *testing.T) {
	if ParseRetry("") != nil {
		t.Error("empty annotation parsed as a retry")
	}
	if ParseRetry("{not json") != nil {
		t.Error("unreadable annotation parsed as a retry")
	}
	var none *Retry
	if none.Has("x") {
		t.Error("nil retry has something")
	}
}

// A second request before the controller acted adds to the first.
func TestRetryMerge(t *testing.T) {
	first := &Retry{At: time.Unix(1, 0), By: "alice", Components: []string{"karpenter"}}
	next := Retry{At: time.Unix(2, 0), By: "bob", Components: []string{"istiod", "karpenter"}, Upgrade: true}

	got := first.Merge(next)

	if !got.At.Equal(next.At) || got.By != "bob" || !got.Upgrade || len(got.Components) != 2 || !got.Has("istiod") || !got.Has("karpenter") {
		t.Fatalf("merged = %+v", got)
	}
	if fresh := (*Retry)(nil).Merge(next); len(fresh.Components) != 2 || !fresh.Upgrade {
		t.Fatalf("merge into none = %+v", fresh)
	}
}
