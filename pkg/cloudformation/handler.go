package cloudformation

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// Path is where the API server serves the templates: TemplatePath(role).
const Path = "/cloudformation/"

// TemplatePath is where the API server serves the role's template (a
// tenant or a management account role), relative to its base URL:
// /cloudformation/<role>.yaml.
func TemplatePath(role string) string { return Path + role + ".yaml" }

// Mount serves the templates read-only on mux under Path (the tenant
// roles' and the management account's), without
// authentication: they hold no secret (the management role and the
// external ID are parameters, given only by GetAccountOnboarding) and a
// tenant fetches them to create the role's stack. Path itself lists them.
func Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET "+Path+"{file}", serveTemplate)
	mux.HandleFunc("GET "+Path+"{$}", listTemplates)
}

func serveTemplate(w http.ResponseWriter, r *http.Request) {
	role, ok := strings.CutSuffix(r.PathValue("file"), ".yaml")
	if !ok || (!slices.Contains(roles, role) && !slices.Contains(managementRoles, role)) {
		http.NotFound(w, r)
		return
	}
	body, ok := anyTemplate(role)
	if !ok {
		http.Error(w, "template not readable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", RoleName(role)+".yaml"))
	if r.Method != http.MethodHead {
		_, _ = w.Write(body) //nolint:gosec // an embedded template of a fixed role list, served as YAML; a failed write: the client went away
	}
}

func listTemplates(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	paths := make([]string, 0, len(roles)+len(managementRoles))
	for _, role := range slices.Concat(roles, managementRoles) {
		paths = append(paths, TemplatePath(role))
	}
	_, _ = fmt.Fprintln(w, strings.Join(paths, "\n")) // the client went away: nothing to do
}
