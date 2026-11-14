package idl_test

import (
	"strings"
	"testing"

	"buf.build/go/protovalidate"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// A maintenance window opens on distinct, defined days at HH:MM UTC, for
// 1h to 24h; no window at all is valid too.
func TestMaintenanceWindowValidation(t *testing.T) {
	type spec = cloudv1.ClusterSpec
	type req = cloudv1.CreateClusterRequest
	sat, sun := cloudv1.Weekday_WEEKDAY_SATURDAY, cloudv1.Weekday_WEEKDAY_SUNDAY
	for name, tt := range map[string]struct {
		window  *cloudv1.MaintenanceWindow
		wantErr string // part of the violation; "" for valid
	}{
		"none":            {},
		"weekend":         {window: &cloudv1.MaintenanceWindow{Days: []cloudv1.Weekday{sat, sun}, Start: "02:00", Duration: "4h"}},
		"every day":       {window: &cloudv1.MaintenanceWindow{Start: "23:30", Duration: "90m"}},
		"hours and mins":  {window: &cloudv1.MaintenanceWindow{Start: "00:00", Duration: "1h30m"}},
		"whole day":       {window: &cloudv1.MaintenanceWindow{Start: "00:00", Duration: "24h"}},
		"too short":       {window: &cloudv1.MaintenanceWindow{Start: "02:00", Duration: "30m"}, wantErr: "1h to 24h"},
		"too long":        {window: &cloudv1.MaintenanceWindow{Start: "02:00", Duration: "25h"}, wantErr: "1h to 24h"},
		"no duration":     {window: &cloudv1.MaintenanceWindow{Start: "02:00"}, wantErr: "1h to 24h"},
		"seconds":         {window: &cloudv1.MaintenanceWindow{Start: "02:00", Duration: "3600s"}, wantErr: "1h to 24h"},
		"hour 24":         {window: &cloudv1.MaintenanceWindow{Start: "24:00", Duration: "4h"}, wantErr: "start"},
		"no start":        {window: &cloudv1.MaintenanceWindow{Duration: "4h"}, wantErr: "start"},
		"one digit hour":  {window: &cloudv1.MaintenanceWindow{Start: "2:00", Duration: "4h"}, wantErr: "start"},
		"repeated day":    {window: &cloudv1.MaintenanceWindow{Days: []cloudv1.Weekday{sat, sat}, Start: "02:00", Duration: "4h"}, wantErr: "days"},
		"unspecified day": {window: &cloudv1.MaintenanceWindow{Days: []cloudv1.Weekday{0}, Start: "02:00", Duration: "4h"}, wantErr: "days"},
		"undefined day":   {window: &cloudv1.MaintenanceWindow{Days: []cloudv1.Weekday{9}, Start: "02:00", Duration: "4h"}, wantErr: "days"},
	} {
		err := protovalidate.Validate(mutate(func(s *spec, _ *req) { s.MaintenanceWindow = tt.window }))
		switch {
		case tt.wantErr == "" && err != nil:
			t.Errorf("%s: Validate = %v, want valid", name, err)
		case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
			t.Errorf("%s: Validate = %v, want a violation about %q", name, err, tt.wantErr)
		}
	}
}
