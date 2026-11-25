package cloudformation

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The server serves each role's template (the tenant roles' and the
// management account's) read-only at
// /cloudformation/<role>.yaml, lists them at /cloudformation/, and serves
// nothing else.
func TestHandlerServesTheTemplates(t *testing.T) {
	tests := []struct {
		path, method string
		status       int
		body         string
	}{
		{"/cloudformation/provisioner.yaml", http.MethodGet, http.StatusOK, "AWS::IAM::Role"},
		{"/cloudformation/updater.yaml", http.MethodGet, http.StatusOK, "AWS::IAM::Role"},
		{"/cloudformation/reader.yaml", http.MethodHead, http.StatusOK, ""},
		{"/cloudformation/management.yaml", http.MethodGet, http.StatusOK, "ManagementRoleArn"},
		{"/cloudformation/server.yaml", http.MethodGet, http.StatusOK, "ServerRoleArn"},
		{"/cloudformation/", http.MethodGet, http.StatusOK, "/cloudformation/updater.yaml"},
		{"/cloudformation/", http.MethodGet, http.StatusOK, "/cloudformation/management.yaml\n/cloudformation/server.yaml"},
		{"/cloudformation/admin.yaml", http.MethodGet, http.StatusNotFound, ""},
		{"/cloudformation/provisioner", http.MethodGet, http.StatusNotFound, ""},
		{"/cloudformation/cluster-vending-provisioner-role.template.yaml", http.MethodGet, http.StatusNotFound, ""},
		{"/cloudformation/provisioner.yaml", http.MethodPost, http.StatusMethodNotAllowed, ""},
		// ServeMux cleans the path and redirects out of Path: nothing here
		// serves it.
		{"/cloudformation/../cloudformation.go", http.MethodGet, http.StatusTemporaryRedirect, ""},
	}
	mux := http.NewServeMux()
	Mount(mux)
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			rec := httptest.NewRecorder()

			mux.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, nil))

			body, _ := io.ReadAll(rec.Body)
			if rec.Code != tt.status || !strings.Contains(string(body), tt.body) {
				t.Fatalf("%d %.120q, want %d with %q", rec.Code, body, tt.status, tt.body)
			}
			if tt.status == http.StatusOK && tt.path != Path &&
				!strings.HasPrefix(rec.Header().Get("Content-Type"), "application/yaml") {
				t.Errorf("Content-Type = %q", rec.Header().Get("Content-Type"))
			}
		})
	}
}

func TestTemplatePath(t *testing.T) {
	if got := TemplatePath("reader"); got != "/cloudformation/reader.yaml" {
		t.Errorf("TemplatePath(reader) = %q", got)
	}
}
