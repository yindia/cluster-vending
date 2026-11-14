package cloudformation

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The server serves each role's template read-only by its file name, and
// nothing else.
func TestHandlerServesTheTemplates(t *testing.T) {
	tests := []struct {
		path, method string
		status       int
		body         string
	}{
		{"/cloudformation/cluster-vending-provisioner-role.template.yaml", http.MethodGet, http.StatusOK, "AWS::IAM::Role"},
		{"/cloudformation/cluster-vending-reader-role.template.yaml", http.MethodHead, http.StatusOK, ""},
		{"/cloudformation/", http.MethodGet, http.StatusOK, "cluster-vending-updater-role.template.yaml"},
		{"/cloudformation/admin-role.template.yaml", http.MethodGet, http.StatusNotFound, ""},
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
			if tt.status == http.StatusOK && tt.path != "/cloudformation/" &&
				!strings.HasPrefix(rec.Header().Get("Content-Type"), "application/yaml") {
				t.Errorf("Content-Type = %q", rec.Header().Get("Content-Type"))
			}
		})
	}
}
