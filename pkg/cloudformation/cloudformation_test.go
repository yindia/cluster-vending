package cloudformation

import (
	"net/url"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// Every role's template is embedded, parses, trusts the ManagementRoleArn
// parameter and names its role cluster-vending-<role>.
func TestTemplatesAreEmbedded(t *testing.T) {
	for _, role := range Roles() {
		t.Run(role, func(t *testing.T) {
			body, err := Template(role)
			if err != nil {
				t.Fatal(err)
			}
			var doc struct {
				Parameters map[string]any `json:"Parameters"`
				Resources  map[string]struct {
					Type       string         `json:"Type"`
					Properties map[string]any `json:"Properties"`
				} `json:"Resources"`
			}
			if err := yaml.Unmarshal(body, &doc); err != nil {
				t.Fatalf("not YAML: %v", err)
			}
			if _, ok := doc.Parameters[ParameterManagementRoleARN]; !ok || len(doc.Parameters) != 1 {
				t.Errorf("parameters = %v, want only %s", doc.Parameters, ParameterManagementRoleARN)
			}
			r, ok := doc.Resources["Role"]
			if !ok || r.Type != "AWS::IAM::Role" {
				t.Fatalf("no AWS::IAM::Role named Role: %v", doc.Resources)
			}
			if got := r.Properties["RoleName"]; got != RoleName(role) {
				t.Errorf("RoleName = %v, want %s", got, RoleName(role))
			}
			if strings.Contains(strings.ToLower(string(body)), "cops") {
				t.Error("the template still names Cops")
			}
		})
	}
}

func TestUnknownRole(t *testing.T) {
	if _, err := Template("admin"); err == nil || !strings.Contains(err.Error(), "reader, updater or provisioner") {
		t.Fatalf("err = %v", err)
	}
}

// The role the controller needs is provisioner, and it is the default.
func TestRoleOfName(t *testing.T) {
	tests := []struct{ name, want string }{
		{"cluster-vending-provisioner", "provisioner"},
		{"cluster-vending-reader", "reader"},
		{"acme-admin", ""},
	}
	for _, tt := range tests {
		if got := RoleOf(tt.name); got != tt.want {
			t.Errorf("RoleOf(%q) = %q, want %q", tt.name, got, tt.want)
		}
	}
	if DefaultRole != "provisioner" {
		t.Errorf("DefaultRole = %q", DefaultRole)
	}
}

func TestCreateStackCommand(t *testing.T) {
	got := CreateStackCommand(Stack{Role: "provisioner", Region: "us-west-1",
		ManagementRoleARN: "arn:aws:iam::111122223333:role/platform"}, "/tmp/role.yaml")

	want := "aws cloudformation create-stack --stack-name cluster-vending-provisioner" +
		" --template-body file:///tmp/role.yaml --capabilities CAPABILITY_NAMED_IAM" +
		" --parameters ParameterKey=ManagementRoleArn,ParameterValue=arn:aws:iam::111122223333:role/platform" +
		" --region us-west-1"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

// The console's quick-create link needs the template at an HTTPS URL.
func TestQuickCreateURL(t *testing.T) {
	stack := Stack{Role: "reader", Region: "eu-west-1", ManagementRoleARN: "arn:aws:iam::111122223333:role/platform"}

	got, err := QuickCreateURL("https://bucket.s3.amazonaws.com/cv/", stack)

	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "console.aws.amazon.com" || !strings.HasPrefix(u.Fragment, "/stacks/quickcreate?") {
		t.Fatalf("url = %s", got)
	}
	q, err := url.ParseQuery(strings.TrimPrefix(u.Fragment, "/stacks/quickcreate?"))
	if err != nil {
		t.Fatal(err)
	}
	if q.Get("templateURL") != "https://bucket.s3.amazonaws.com/cv/cluster-vending-reader-role.template.yaml" ||
		q.Get("stackName") != "cluster-vending-reader" ||
		q.Get("param_ManagementRoleArn") != stack.ManagementRoleARN || u.Query().Get("region") != "eu-west-1" {
		t.Errorf("query = %v (region %q)", q, u.Query().Get("region"))
	}
	if _, err := QuickCreateURL("", stack); err == nil {
		t.Error("no base URL accepted")
	}
}
