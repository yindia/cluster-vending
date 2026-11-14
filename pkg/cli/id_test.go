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

// A verb written with an <id> (or optional [id]) argument takes --id
// instead: its usage drops the argument, a positional one is still taken.
func TestIDVerb(t *testing.T) {
	tests := []struct {
		use, wantUse    string
		takes, required bool
	}{
		{"get <id>", "get", true, true},
		{"update [id]", "update", true, false},
		{"watch <id>", "watch", true, true},
		{"list", "list", false, false},
		{"enable <name>", "enable <name>", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.use, func(t *testing.T) {
			use, takes, required := IDVerb(tt.use)

			if use != tt.wantUse || takes != tt.takes || required != tt.required {
				t.Errorf("IDVerb(%q) = %q, %v, %v", tt.use, use, takes, required)
			}
		})
	}
}

func TestRequireID(t *testing.T) {
	if err := RequireID("", "cluster"); err == nil || err.Error() != "--id is required: the cluster's ID" {
		t.Errorf("RequireID = %v", err)
	}
	if err := RequireID("01a1", "cluster"); err != nil {
		t.Errorf("RequireID = %v", err)
	}
}
