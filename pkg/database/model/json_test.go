package model_test

import (
	"testing"

	"github.com/yindia/cluster-vending/pkg/database/model"
)

func TestJSONValueAndScan(t *testing.T) {
	v, err := model.JSON(`{"a":1}`).Value()
	if err != nil || v != `{"a":1}` {
		t.Fatalf("Value = %v, %v", v, err)
	}
	if v, err := model.JSON(nil).Value(); err != nil || v != nil {
		t.Fatalf("empty Value = %v, %v; want NULL", v, err)
	}
	if _, err := model.JSON(`{not json`).Value(); err == nil {
		t.Fatal("Value accepted invalid JSON")
	}

	tests := []struct {
		src  any
		want string
	}{
		{[]byte(`{"b":2}`), `{"b":2}`},
		{`{"c":3}`, `{"c":3}`},
		{nil, ``},
	}
	for _, tt := range tests {
		var j model.JSON
		if err := j.Scan(tt.src); err != nil || string(j) != tt.want {
			t.Errorf("Scan(%v) = %q, %v; want %q", tt.src, j, err, tt.want)
		}
	}
	var j model.JSON
	if err := j.Scan(42); err == nil {
		t.Error("Scan accepted an int")
	}
}

func TestScanCopiesDriverBytes(t *testing.T) {
	src := []byte(`{"a":1}`)
	var j model.JSON
	if err := j.Scan(src); err != nil {
		t.Fatal(err)
	}
	src[2] = 'X' // drivers may reuse their buffer
	if string(j) != `{"a":1}` {
		t.Fatalf("Scan kept a reference to the driver buffer: %q", j)
	}
}

func TestLabelsValueAndScan(t *testing.T) {
	v, err := model.Labels{"env": "prod"}.Value()
	if err != nil || v != `{"env":"prod"}` {
		t.Fatalf("Value = %v, %v", v, err)
	}
	if v, _ := model.Labels(nil).Value(); v != "{}" {
		t.Fatalf("nil Value = %v, want {}", v)
	}

	for _, src := range []any{[]byte(`{"team":"x"}`), `{"team":"x"}`} {
		var l model.Labels
		if err := l.Scan(src); err != nil || l["team"] != "x" {
			t.Errorf("Scan(%v) = %v, %v", src, l, err)
		}
	}
	var l model.Labels
	if err := l.Scan(nil); err != nil || l != nil {
		t.Errorf("Scan(nil) = %v, %v", l, err)
	}
	if err := l.Scan(`[1,2]`); err == nil {
		t.Error("Scan accepted a JSON array")
	}
	if err := l.Scan(3.5); err == nil {
		t.Error("Scan accepted a float")
	}
}
