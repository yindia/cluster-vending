package reconcile

import (
	"testing"
	"time"

	"github.com/yindia/cluster-vending/pkg/database/model"
)

func TestDiscoveredRestore(t *testing.T) {
	same := func(a, b model.JSON) bool { return string(a) == string(b) }
	discovered := Discovered{Spec: []byte(`{"region":"us-east-1"}`), DisplayName: "Prod", Labels: map[string]string{"env": "prod"}}
	tests := []struct {
		name    string
		row     model.Resource
		changed bool
	}{
		{"as discovered", model.Resource{Spec: model.JSON(`{"region":"us-east-1"}`), DisplayName: "Prod", Labels: model.Labels{"env": "prod"}}, false},
		{"another spec", model.Resource{Spec: model.JSON(`{"region":"eu-west-1"}`), DisplayName: "Prod", Labels: model.Labels{"env": "prod"}}, true},
		{"another display name", model.Resource{Spec: model.JSON(`{"region":"us-east-1"}`), DisplayName: "x", Labels: model.Labels{"env": "prod"}}, true},
		{"a label added", model.Resource{Spec: model.JSON(`{"region":"us-east-1"}`), DisplayName: "Prod", Labels: model.Labels{"env": "prod", "a": "b"}}, true},
		{"labels removed", model.Resource{Spec: model.JSON(`{"region":"us-east-1"}`), DisplayName: "Prod"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row := tt.row

			changed := discovered.Restore(&row, same)

			if changed != tt.changed {
				t.Fatalf("changed = %v, want %v", changed, tt.changed)
			}
			if string(row.Spec) != string(discovered.Spec) || row.DisplayName != "Prod" || row.Labels["env"] != "prod" || len(row.Labels) != 1 {
				t.Errorf("row = %s %q %v, want what was discovered", row.Spec, row.DisplayName, row.Labels)
			}
		})
	}
}

// What an import kept next to the snapshot stays, and the snapshot reads
// back as it was written.
func TestDiscoveredIsKeptNextToOtherImportData(t *testing.T) {
	stored := model.JSON(`{"importObjects":[1,2]}`)
	d := Snapshot(&model.Resource{Spec: model.JSON(`{"a":1}`), DisplayName: "n", Labels: model.Labels{"k": "v"}}).
		Ignored(time.Date(2026, 11, 14, 1, 2, 3, 0, time.UTC))

	out, err := WithDiscovered(stored, d)
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := DiscoveredOf(out)

	if err != nil || !ok || string(got.Spec) != `{"a":1}` || got.DisplayName != "n" || got.Labels["k"] != "v" ||
		got.EditIgnoredAt != "2026-11-14T01:02:03Z" {
		t.Fatalf("read back %+v, %v, %v", got, ok, err)
	}
	if _, kept, _ := DiscoveredOf(stored); kept {
		t.Error("a row without a snapshot reads as having one")
	}
	if _, has, _ := DiscoveredOf(nil); has {
		t.Error("an empty value reads as having a snapshot")
	}
	if string(out) == string(stored) || !contains(out, `"importObjects":[1,2]`) {
		t.Errorf("the other import data was not kept: %s", out)
	}
	if _, _, err := DiscoveredOf(model.JSON(`not json`)); err == nil {
		t.Error("unreadable import data read without an error")
	}
}

func contains(raw model.JSON, part string) bool {
	for i := 0; i+len(part) <= len(raw); i++ {
		if string(raw[i:i+len(part)]) == part {
			return true
		}
	}
	return false
}
