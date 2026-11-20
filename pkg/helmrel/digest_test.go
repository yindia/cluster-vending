package helmrel_test

import (
	"testing"
	"time"

	"github.com/yindia/cluster-vending/pkg/helmrel"
)

func TestDigestIsStableAndSensitive(t *testing.T) {
	base := helmrel.Spec{Repository: "https://r", Chart: "c", Version: "1.0.0", Timeout: 10 * time.Minute,
		Values: map[string]any{"a": map[string]any{"x": int64(1), "y": "z"}, "b": []any{"p", "q"}}}
	same := helmrel.Spec{Repository: "https://r", Chart: "c", Version: "1.0.0", Timeout: 10 * time.Minute,
		Values: map[string]any{"b": []any{"p", "q"}, "a": map[string]any{"y": "z", "x": int64(1)}}}
	if helmrel.Digest(base) != helmrel.Digest(same) {
		t.Error("key order must not change the digest")
	}
	if len(helmrel.Digest(base)) != 64 {
		t.Errorf("digest %q is not a sha256 hex", helmrel.Digest(base))
	}
	for name, mutate := range map[string]func(s *helmrel.Spec){
		"repository": func(s *helmrel.Spec) { s.Repository = "https://other" },
		"chart":      func(s *helmrel.Spec) { s.Chart = "d" },
		"version":    func(s *helmrel.Spec) { s.Version = "1.0.1" },
		"values":     func(s *helmrel.Spec) { s.Values = map[string]any{"a": "different"} },
		"timeout":    func(s *helmrel.Spec) { s.Timeout = time.Hour },
		"skipCRDs":   func(s *helmrel.Spec) { s.SkipCRDs = true },
	} {
		changed := base
		mutate(&changed)
		if helmrel.Digest(changed) == helmrel.Digest(base) {
			t.Errorf("changing %s must change the digest", name)
		}
	}
}

func TestDigestNilAndEmptyValuesAgree(t *testing.T) {
	a := helmrel.Spec{Repository: "r", Chart: "c", Version: "1"}
	b := a
	b.Values = map[string]any{}
	if helmrel.Digest(a) != helmrel.Digest(b) {
		t.Error("no values and empty values are the same release")
	}
}
