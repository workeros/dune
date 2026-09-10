package webapp

import (
	"errors"
	"net/http"
	"time"

	"github.com/aiomni/dune/internal/metadata"
	"github.com/aiomni/dune/pkg/fabric"
	"github.com/aiomni/dune/pkg/managed"
	"github.com/aiomni/dune/pkg/runner"
)

type operationView struct {
	ID                   string                       `json:"id"`
	RunnerID             string                       `json:"runner_id"`
	FabricID             string                       `json:"fabric_id"`
	BindingRevision      int64                        `json:"binding_revision"`
	Action               string                       `json:"action"`
	CreatedAt            time.Time                    `json:"created_at"`
	Finished             bool                         `json:"finished"`
	Outcome              string                       `json:"outcome,omitempty"`
	Stage                string                       `json:"stage,omitempty"`
	ProviderOutcome      string                       `json:"provider_outcome,omitempty"`
	ResourceRef          string                       `json:"resource_ref,omitempty"`
	ExpiresAt            *time.Time                   `json:"expires_at,omitempty"`
	RenewalPolicyVersion string                       `json:"renewal_policy_version,omitempty"`
	RenewalReason        string                       `json:"renewal_reason,omitempty"`
	RenewalObservedAt    *time.Time                   `json:"renewal_observed_at,omitempty"`
	RenewalNextCheckAt   *time.Time                   `json:"renewal_next_check_at,omitempty"`
	RenewalUntil         *time.Time                   `json:"renewal_until,omitempty"`
	AccessClosed         bool                         `json:"access_closed"`
	AccessSuspended      bool                         `json:"access_suspended"`
	ResourceState        string                       `json:"resource_state,omitempty"`
	Capabilities         *fabric.ResourceCapabilities `json:"capabilities,omitempty"`
	AccessCloseOutcome   string                       `json:"access_close_outcome,omitempty"`
	AccessCloseDeadline  *time.Time                   `json:"access_close_deadline,omitempty"`
}

type reviewView struct {
	ID                  string     `json:"id"`
	OperationID         string     `json:"operation_id"`
	Mode                string     `json:"mode"`
	Candidate           string     `json:"candidate_resource_ref,omitempty"`
	Reason              string     `json:"reason"`
	CreatedAt           time.Time  `json:"created_at"`
	CompletedAt         *time.Time `json:"completed_at,omitempty"`
	Outcome             string     `json:"outcome,omitempty"`
	VerifiedResourceRef string     `json:"verified_resource_ref,omitempty"`
}

func reviewResponse(review managed.Review) reviewView {
	view := reviewView{ID: review.ID, OperationID: review.OperationID, Mode: review.Mode, Candidate: review.Candidate, Reason: review.Reason, CreatedAt: review.CreatedAt, Outcome: review.Outcome, VerifiedResourceRef: review.VerifiedResourceRef}
	if !review.CompletedAt.IsZero() {
		completed := review.CompletedAt
		view.CompletedAt = &completed
	}
	return view
}

func statusResponse(status managed.Status) operationView {
	view := operationResponse(status.Operation)
	view.Stage, view.ProviderOutcome, view.ResourceRef = status.Stage, status.ProviderOutcome, status.ResourceRef
	view.AccessClosed = status.AccessClosed
	view.AccessSuspended, view.ResourceState, view.Capabilities = status.AccessSuspended, status.ResourceState, status.Capabilities
	if !status.ExpiresAt.IsZero() {
		expires := status.ExpiresAt
		view.ExpiresAt = &expires
	}
	view.RenewalPolicyVersion, view.RenewalReason = status.RenewalPolicyVersion, status.RenewalReason
	if !status.RenewalObservedAt.IsZero() {
		observed := status.RenewalObservedAt
		view.RenewalObservedAt = &observed
	}
	if !status.RenewalNextCheckAt.IsZero() {
		next := status.RenewalNextCheckAt
		view.RenewalNextCheckAt = &next
	}
	if !status.RenewalUntil.IsZero() {
		until := status.RenewalUntil
		view.RenewalUntil = &until
	}
	view.AccessCloseOutcome = status.AccessCloseOutcome
	if !status.AccessCloseDeadline.IsZero() {
		deadline := status.AccessCloseDeadline
		view.AccessCloseDeadline = &deadline
	}
	return view
}

func operationResponse(operation managed.Operation) operationView {
	return operationView{
		ID: operation.ID, RunnerID: operation.RunnerID,
		FabricID: operation.FabricID, BindingRevision: operation.BindingRevision,
		Action: operation.Action, CreatedAt: operation.CreatedAt, Finished: operation.Finished, Outcome: operation.Outcome,
	}
}

func (s *Server) managedTemplates(w http.ResponseWriter, r *http.Request) {
	user, _, ok := s.user(w, r)
	if !ok {
		return
	}
	templates, err := s.options.Managed.Templates(r.Context(), user)
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": templates})
}

func (s *Server) managedTemplate(w http.ResponseWriter, r *http.Request) {
	user, _, ok := s.user(w, r)
	if !ok {
		return
	}
	template, err := s.options.Managed.Template(r.Context(), user, r.PathValue("fabric"), r.PathValue("template"), r.PathValue("version"))
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, template)
}

func (s *Server) createManagedRunner(w http.ResponseWriter, r *http.Request) {
	user, _, ok := s.user(w, r)
	if !ok {
		return
	}
	var request struct {
		RequestKey string               `json:"request_key"`
		Request    fabric.CreateRequest `json:"request"`
	}
	if !readJSON(w, r, &request) {
		return
	}
	created, err := s.options.Managed.Create(r.Context(), user, request.RequestKey, request.Request)
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	logical, err := s.store.Runner(r.Context(), user.ID, created.Runner.ID)
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	if logical.Kind != "managed" || created.Operation.RunnerID != logical.ID {
		writeMetadataError(w, metadata.ErrInvalidArgument)
		return
	}
	writeJSON(w, http.StatusAccepted, struct {
		Runner    runner.Runner `json:"runner"`
		Operation operationView `json:"operation"`
	}{Runner: logical, Operation: operationResponse(created.Operation)})
}

func (s *Server) destroyManagedRunner(w http.ResponseWriter, r *http.Request) {
	user, _, ok := s.user(w, r)
	if !ok {
		return
	}
	var request struct {
		RequestKey string `json:"request_key"`
	}
	if !readJSON(w, r, &request) {
		return
	}
	destroyed, err := s.options.Managed.Destroy(r.Context(), user, request.RequestKey, r.PathValue("runner"))
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	machineID, err := s.store.RevokeManaged(r.Context(), user.ID, r.PathValue("runner"))
	if err != nil && !errors.Is(err, metadata.ErrNotFound) {
		writeMetadataError(w, err)
		return
	}
	if machineID != "" {
		s.gateway.Disconnect(machineID)
	}
	writeJSON(w, http.StatusAccepted, struct {
		Operation         operationView `json:"operation"`
		AccessClosedAt    time.Time     `json:"access_closed_at"`
		CloseDeadline     time.Time     `json:"close_deadline"`
		AccessCloseResult string        `json:"access_close_outcome"`
	}{Operation: operationResponse(destroyed.Operation), AccessClosedAt: destroyed.AccessClosedAt, CloseDeadline: destroyed.AccessCloseDeadline, AccessCloseResult: destroyed.AccessCloseOutcome})
}

func (s *Server) pauseManagedRunner(w http.ResponseWriter, r *http.Request) {
	s.pauseResumeManagedRunner(w, r, "pause")
}

func (s *Server) resumeManagedRunner(w http.ResponseWriter, r *http.Request) {
	s.pauseResumeManagedRunner(w, r, "resume")
}

func (s *Server) pauseResumeManagedRunner(w http.ResponseWriter, r *http.Request, action string) {
	user, _, ok := s.user(w, r)
	if !ok {
		return
	}
	var request struct {
		RequestKey string `json:"request_key"`
	}
	if !readJSON(w, r, &request) {
		return
	}
	var mutation managed.Mutation
	var err error
	if action == "pause" {
		mutation, err = s.options.Managed.Pause(r.Context(), user, request.RequestKey, r.PathValue("runner"))
	} else {
		mutation, err = s.options.Managed.Resume(r.Context(), user, request.RequestKey, r.PathValue("runner"))
	}
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	machineID, err := s.store.SetManagedSuspended(r.Context(), user.ID, r.PathValue("runner"), action == "pause")
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	if action == "pause" && machineID != "" {
		s.gateway.Drop(machineID)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"operation": operationResponse(mutation.Operation)})
}

func (s *Server) managedOperation(w http.ResponseWriter, r *http.Request) {
	user, _, ok := s.user(w, r)
	if !ok {
		return
	}
	status, err := s.options.Managed.Status(r.Context(), user, r.PathValue("operation"))
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, statusResponse(status))
}

func (s *Server) managedRunnerStatus(w http.ResponseWriter, r *http.Request) {
	user, _, ok := s.user(w, r)
	if !ok {
		return
	}
	status, err := s.options.Managed.RunnerStatus(r.Context(), user, r.PathValue("runner"))
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, statusResponse(status))
}

func (s *Server) createManagedReview(w http.ResponseWriter, r *http.Request) {
	user, _, ok := s.user(w, r)
	if !ok {
		return
	}
	var request struct {
		RequestKey string `json:"request_key"`
		Mode       string `json:"mode"`
		Candidate  string `json:"candidate_resource_ref"`
		Reason     string `json:"reason"`
	}
	if !readJSON(w, r, &request) {
		return
	}
	review, err := s.options.Managed.Review(r.Context(), user, request.RequestKey, managed.ReviewRequest{OperationID: r.PathValue("operation"), Mode: request.Mode, Candidate: request.Candidate, Reason: request.Reason})
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, reviewResponse(review))
}

func (s *Server) managedReview(w http.ResponseWriter, r *http.Request) {
	user, _, ok := s.user(w, r)
	if !ok {
		return
	}
	review, err := s.options.Managed.ReviewStatus(r.Context(), user, r.PathValue("review"))
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, reviewResponse(review))
}

func (s *Server) managedOperationReview(w http.ResponseWriter, r *http.Request) {
	user, _, ok := s.user(w, r)
	if !ok {
		return
	}
	review, err := s.options.Managed.OperationReview(r.Context(), user, r.PathValue("operation"))
	if err != nil {
		writeMetadataError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, reviewResponse(review))
}
