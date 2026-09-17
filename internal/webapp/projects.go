package webapp

import (
	"net/http"
	"strconv"

	"github.com/aiomni/dune/internal/authorization"
	"github.com/aiomni/dune/internal/metadata"
	publicidentity "github.com/aiomni/dune/pkg/identity"
	"github.com/aiomni/dune/pkg/runner"
	"github.com/aiomni/dune/pkg/workbench"
)

// workbenchOwner resolves scope from the authenticated user and checks Tenant
// membership before any saved data is read. Payloads cannot choose their owner.
func (s *Server) workbenchOwner(w http.ResponseWriter, r *http.Request, operation string) (publicidentity.User, string, bool) {
	user, _, ok := s.user(w, r)
	if !ok {
		return user, "", false
	}
	owner := user.ID
	if s.options.TenantScoped {
		owner = r.PathValue("tenant")
		if owner == "" {
			writeMetadataError(w, metadata.ErrInvalidArgument)
			return user, "", false
		}
	}
	if _, err := s.access.Check(r.Context(), user, authorization.Resource{OwnerID: owner}, operation, ""); err != nil {
		writeMetadataError(w, err)
		return user, "", false
	}
	return user, owner, true
}

func (s *Server) projectRoutes(prefix string) {
	s.mux.HandleFunc("GET "+prefix+"/projects", s.listProjects)
	s.mux.HandleFunc("POST "+prefix+"/projects", s.saveProject)
	s.mux.HandleFunc("GET "+prefix+"/projects/{project}", s.getProject)
	s.mux.HandleFunc("PUT "+prefix+"/projects/{project}", s.saveProject)
	s.mux.HandleFunc("DELETE "+prefix+"/projects/{project}", s.deleteProject)
}

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	_, owner, ok := s.workbenchOwner(w, r, "workspace.read")
	if !ok {
		return
	}
	query, ok := pageQuery(w, r)
	if !ok {
		return
	}
	if query.Limit == 0 {
		query.Limit = 32
	}
	items, err := s.store.Projects(r.Context(), owner, query.Cursor, query.Limit+1)
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	page := struct {
		Items      []workbench.Project `json:"items"`
		NextCursor string              `json:"next_cursor,omitempty"`
	}{Items: items}
	if len(items) > query.Limit {
		page.Items = items[:query.Limit]
		page.NextCursor = page.Items[len(page.Items)-1].ID
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) getProject(w http.ResponseWriter, r *http.Request) {
	_, owner, ok := s.workbenchOwner(w, r, "workspace.read")
	if !ok {
		return
	}
	project, err := s.store.Project(r.Context(), owner, r.PathValue("project"))
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, project)
}

func (s *Server) saveProject(w http.ResponseWriter, r *http.Request) {
	user, owner, ok := s.workbenchOwner(w, r, "workspace.write")
	if !ok {
		return
	}
	var body struct {
		Revision int64 `json:"revision"`
		workbench.ProjectSpec
	}
	if !readJSON(w, r, &body) {
		return
	}
	if err := body.ProjectSpec.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return
	}
	for _, directory := range body.Directories {
		resource, _, err := s.access.Resource(r.Context(), user, directory.Binding.RunnerID, false, "runner.get")
		if err == nil && resource.OwnerID != owner {
			err = metadata.ErrNotFound
		}
		if err == nil && (resource.Runner.Binding == nil || *resource.Runner.Binding != directory.Binding) {
			err = runner.ErrBindingChanged
		}
		if err != nil {
			writeMetadataError(w, err)
			return
		}
	}
	if body.DefaultProfile != nil {
		profile, err := s.store.Profiles().Get(r.Context(), owner, *body.DefaultProfile)
		if err == nil && profile.Profile.Kind != "agent" {
			err = metadata.ErrInvalidArgument
		}
		if err != nil {
			writeMetadataError(w, err)
			return
		}
	}
	project, err := s.store.SaveProject(r.Context(), owner, r.PathValue("project"), body.Revision, body.ProjectSpec)
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	status := http.StatusOK
	if r.Method == http.MethodPost {
		status = http.StatusCreated
	}
	writeJSON(w, status, project)
}

func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request) {
	_, owner, ok := s.workbenchOwner(w, r, "workspace.write")
	if !ok {
		return
	}
	revision, err := strconv.ParseInt(r.URL.Query().Get("revision"), 10, 64)
	if err != nil || revision < 1 {
		writeMetadataError(w, metadata.ErrInvalidArgument)
		return
	}
	if err := s.store.DeleteProject(r.Context(), owner, r.PathValue("project"), revision); err != nil {
		writeMetadataError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
