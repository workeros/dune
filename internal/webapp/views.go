package webapp

import (
	"net/http"

	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/pkg/workbench"
)

func (s *Server) viewRoutes(prefix string) {
	s.mux.HandleFunc("GET "+prefix+"/workbench/views/{view}", s.getView)
	s.mux.HandleFunc("PUT "+prefix+"/workbench/views/{view}", s.saveView)
	s.mux.HandleFunc("POST "+prefix+"/workbench/read-markers/query", s.readMarkers)
	s.mux.HandleFunc("PUT "+prefix+"/workbench/read-markers", s.markRead)
}

func (s *Server) viewOwner(w http.ResponseWriter, r *http.Request, operation string) (metadata.ViewOwner, bool) {
	user, owner, ok := s.workbenchOwner(w, r, operation)
	return metadata.ViewOwner{OwnerID: owner, Namespace: user.Namespace, UserID: user.ID}, ok
}

func (s *Server) getView(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.viewOwner(w, r, "workspace.read")
	if !ok {
		return
	}
	view, err := s.store.View(r.Context(), owner, r.PathValue("view"))
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) saveView(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.viewOwner(w, r, "workspace.write")
	if !ok {
		return
	}
	var body struct {
		Revision int64 `json:"revision"`
		workbench.ViewSpec
	}
	if !readJSON(w, r, &body) {
		return
	}
	view, err := s.store.SaveView(r.Context(), owner, workbench.View{ID: r.PathValue("view"), Revision: body.Revision, ViewSpec: body.ViewSpec})
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) readMarkers(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.viewOwner(w, r, "workspace.read")
	if !ok {
		return
	}
	var body struct {
		Items []workbench.ReadMarker `json:"items"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	markers, err := s.store.ReadMarkers(r.Context(), owner, body.Items)
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": markers})
}

func (s *Server) markRead(w http.ResponseWriter, r *http.Request) {
	owner, ok := s.viewOwner(w, r, "workspace.write")
	if !ok {
		return
	}
	var marker workbench.ReadMarker
	if !readJSON(w, r, &marker) {
		return
	}
	marker, err := s.store.MarkRead(r.Context(), owner, marker)
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, marker)
}
