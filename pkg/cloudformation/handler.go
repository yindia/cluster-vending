package cloudformation

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// Path is where the API server serves the templates: Path + FileName(role).
const Path = "/cloudformation/"

// Mount serves the templates read-only on mux under Path, without
// authentication: they hold no secret (the management role is a
// parameter) and a tenant fetches them to create the stack. Path itself
// lists them.
func Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET "+Path+"{file}", serveTemplate)
	mux.HandleFunc("GET "+Path+"{$}", listTemplates)
}

func serveTemplate(w http.ResponseWriter, r *http.Request) {
	file := r.PathValue("file")
	for _, role := range roles {
		if FileName(role) != file {
			continue
		}
		body, err := Template(role)
		if err != nil {
			http.Error(w, "template not readable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", file))
		if r.Method != http.MethodHead {
			_, _ = w.Write(body)
		}
		return
	}
	http.NotFound(w, r)
}

func listTemplates(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	names := make([]string, 0, len(roles))
	for _, role := range slices.Clone(roles) {
		names = append(names, FileName(role))
	}
	_, _ = fmt.Fprintln(w, strings.Join(names, "\n"))
}
