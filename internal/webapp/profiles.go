package webapp

import (
	"net/http"
	"strconv"

	"github.com/aiomni/dune/pkg/api"
	"github.com/aiomni/dune/pkg/profiles"
)

func (s *Server) listProfiles(w http.ResponseWriter, r *http.Request) {
	user, _, ok := s.user(w, r)
	if !ok {
		return
	}
	items, err := s.store.Profiles().List(r.Context(), user.ID)
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func profileSelection(r *http.Request, required bool) (profiles.Selection, error) {
	selection := profiles.Selection{ID: r.PathValue("profile")}
	if value := r.URL.Query().Get("revision"); value != "" {
		revision, err := strconv.ParseInt(value, 10, 64)
		if err != nil || revision < 1 {
			return selection, profiles.ErrInvalid
		}
		selection.Revision = revision
	}
	if required && selection.Revision == 0 {
		return selection, profiles.ErrInvalid
	}
	return selection, nil
}

func (s *Server) getProfile(w http.ResponseWriter, r *http.Request) {
	user, _, ok := s.user(w, r)
	if !ok {
		return
	}
	selection, err := profileSelection(r, false)
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	record, err := s.store.Profiles().Get(r.Context(), user.ID, selection)
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, record)
}

func (s *Server) saveProfile(w http.ResponseWriter, r *http.Request) {
	user, _, ok := s.user(w, r)
	if !ok {
		return
	}
	var body struct {
		Name        string      `json:"name"`
		Description string      `json:"description"`
		Revision    int64       `json:"revision"`
		Profile     api.Profile `json:"profile"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	creator := profiles.Actor{Type: user.Kind, Subject: user.Subject}
	if creator.Type == "" {
		creator.Type = "user"
	}
	if creator.Subject == "" {
		creator.Subject = user.ID
	}
	record := profiles.Record{ID: r.PathValue("profile"), OwnerID: user.ID, Name: body.Name, Description: body.Description, Revision: body.Revision, Profile: body.Profile, CreatedBy: creator}
	var err error
	status := http.StatusOK
	if r.Method == http.MethodPost {
		record, err = s.store.Profiles().Create(r.Context(), record)
		status = http.StatusCreated
	} else {
		record, err = s.store.Profiles().Update(r.Context(), record)
	}
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	writeJSON(w, status, record)
}

func (s *Server) deleteProfile(w http.ResponseWriter, r *http.Request) {
	user, _, ok := s.user(w, r)
	if !ok {
		return
	}
	selection, err := profileSelection(r, true)
	if err == nil {
		err = s.store.Profiles().Delete(r.Context(), user.ID, selection)
	}
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
