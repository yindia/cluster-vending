package config_test

import (
	"strings"
	"testing"

	"github.com/yindia/cluster-vending/pkg/config"
)

const (
	sourceRole = "arn:aws:iam::111122223333:role/cluster-vending-server"
	serverRole = "arn:aws:iam::111122223333:role/cluster-vending-api"
)

// The server and the controller reach tenant accounts the same way, so they
// take the same three settings: the SDK's default region, an optional source
// role and an optional external ID. Keys are never a setting.
func TestAWSAccessSettingsOfServerAndController(t *testing.T) {
	flags := lookup(map[string]string{
		config.FlagAWSRegion: "eu-west-1", config.FlagAWSSourceRoleARN: sourceRole, config.FlagAWSExternalID: "ext-1234",
	})
	want := config.AWSAccess{Region: "eu-west-1", SourceRoleARN: sourceRole, ExternalID: "ext-1234"}

	serve, err := config.LoadServe(flags, lookup(nil), nil)
	if err != nil || serve.AWSAccess != want {
		t.Errorf("serve: %+v, %v; want %+v", serve.AWSAccess, err, want)
	}
	controller, err := config.LoadController(flags, lookup(awsEnv()), nil)
	if err != nil || controller.AWSAccess != want {
		t.Errorf("controller: %+v, %v; want %+v", controller.AWSAccess, err, want)
	}
}

func TestAWSAccessSettingsComeFromTheEnvironmentAndTheFile(t *testing.T) {
	env := lookup(map[string]string{config.EnvAWSRegion: "us-west-2"})
	file := lookup(map[string]string{config.FlagAWSSourceRoleARN: sourceRole})
	serve, err := config.LoadServe(nil, env, file)
	if err != nil || serve.AWSAccess.Region != "us-west-2" || serve.AWSAccess.SourceRoleARN != sourceRole {
		t.Errorf("serve = %+v, %v", serve.AWSAccess, err)
	}
	if (config.AWSAccess{}) != func() config.AWSAccess { s, _ := config.LoadServe(nil, lookup(nil), nil); return s.AWSAccess }() {
		t.Error("the settings are not optional")
	}
}

func TestAWSAccessSettingsAreValidated(t *testing.T) {
	for flag, value := range map[string]string{
		config.FlagAWSRegion:        "mars-1",
		config.FlagAWSSourceRoleARN: "role",
		config.FlagAWSExternalID:    "has space",
		config.FlagAWSServerRoleARN: "arn:aws:iam::123:role/x",
	} {
		_, err := config.LoadServe(lookup(map[string]string{flag: value}), lookup(nil), nil)
		requireErrorNaming(t, err, flag)
		if flag == config.FlagAWSServerRoleARN {
			continue // the controller does not take it
		}
		_, err = config.LoadController(lookup(map[string]string{flag: value}), lookup(awsEnv()), nil)
		requireErrorNaming(t, err, flag)
	}
	_, err := config.LoadServe(lookup(map[string]string{config.FlagAWSExternalID: strings.Repeat("e", 1225)}), lookup(nil), nil)
	requireErrorNaming(t, err, config.FlagAWSExternalID)
}

// The principal the tenant role trusts for the server: the role its
// credentials end as before the tenant role.
func TestServerPrincipalARN(t *testing.T) {
	for name, tt := range map[string]struct {
		flags map[string]string
		want  string
	}{
		"none":              {nil, ""},
		"the server's role": {map[string]string{config.FlagAWSServerRoleARN: serverRole}, serverRole},
		"the source role":   {map[string]string{config.FlagAWSSourceRoleARN: sourceRole}, sourceRole},
		"both: the source role is what assumes the tenant role": {map[string]string{
			config.FlagAWSServerRoleARN: serverRole, config.FlagAWSSourceRoleARN: sourceRole}, sourceRole},
	} {
		cfg, err := config.LoadServe(lookup(tt.flags), lookup(nil), nil)
		if err != nil || cfg.ServerPrincipalARN() != tt.want {
			t.Errorf("%s: %q, %v; want %q", name, cfg.ServerPrincipalARN(), err, tt.want)
		}
	}
}
