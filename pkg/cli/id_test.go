package cli

import "testing"

func TestResolveID(t *testing.T) {
	tests := []struct {
		name    string
		flag    string
		args    []string
		want    string
		wantErr bool
	}{
		{name: "the flag", flag: "a1", want: "a1"},
		{name: "the argument", args: []string{"a1"}, want: "a1"},
		{name: "both, the same", flag: "a1", args: []string{"a1"}, want: "a1"},
		{name: "both, different", flag: "a1", args: []string{"b2"}, wantErr: true},
		{name: "neither"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveID(tt.flag, tt.args)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Errorf("ResolveID = %q, %v; want %q (error %v)", got, err, tt.want, tt.wantErr)
			}
		})
	}
}
