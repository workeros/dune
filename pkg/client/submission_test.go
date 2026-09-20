package client

import (
	"context"
	"errors"
	"testing"

	"github.com/aiomni/dune/pkg/api"
)

func TestQuerySubmissionLocalErrorsKeepOriginalKey(t *testing.T) {
	key := api.SubmissionKey{SubmissionID: "caller-owned", Target: api.SubmissionTarget{
		OwnerID: "owner", RunnerID: "runner", FabricID: "fabric", MachineID: "machine", BindingRevision: 1,
	}}
	client := &Client{Binding: api.Binding{Target: "machine", Capabilities: []string{"submission.get"}}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	receipt, err := client.QuerySubmission(ctx, key)
	var failure *api.SubmissionError
	if !errors.Is(err, context.Canceled) || !errors.As(err, &failure) || failure.Key != key || receipt.SubmissionKey != key || receipt.Admission != api.SubmissionUnknown {
		t.Fatalf("local cancellation changed the query identity: %+v, %v", receipt, err)
	}
	client.Binding.Capabilities = nil
	_, err = client.QuerySubmission(t.Context(), key)
	var unsupported *api.Error
	if !errors.As(err, &unsupported) || unsupported.Code != "UNSUPPORTED" || !errors.As(err, &failure) || failure.Key != key {
		t.Fatal("unsupported query lost structured cause or original key", err)
	}
}

func TestStartRequiresCallerKeyAndPreservesLocalCancellation(t *testing.T) {
	key := api.SubmissionKey{SubmissionID: "saved-before-send", Target: api.SubmissionTarget{OwnerID: "owner", RunnerID: "runner", FabricID: "fabric", MachineID: "machine", BindingRevision: 1}}
	client := &Client{Binding: api.Binding{Target: "machine", Capabilities: []string{"profile.start"}}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	request := api.StartRequest{SubmissionKey: key, Profile: api.Profile{Kind: "agent"}}
	result, stream, err := client.Start(ctx, request)
	var failure *api.SubmissionError
	if !errors.Is(err, context.Canceled) || !errors.As(err, &failure) || failure.Key != key || result.SubmissionKey != key || result.Admission != api.SubmissionUnknown || stream != nil {
		t.Fatal("cancelled launch lost original identity or invented acceptance", result, err)
	}
	request.SubmissionID = ""
	_, _, err = client.Start(t.Context(), request)
	var invalid *api.Error
	if !errors.As(err, &invalid) || invalid.Code != "INVALID_ARGUMENT" || !errors.As(err, &failure) || failure.Key.SubmissionID != "" {
		t.Fatal("Start generated a replacement ID", err)
	}
}

func TestACPSubmitAndControlKeepCallerKeyWithoutAResponse(t *testing.T) {
	key := api.SubmissionKey{SubmissionID: "saved-before-submit", Target: api.SubmissionTarget{OwnerID: "owner", RunnerID: "runner", FabricID: "fabric", MachineID: "machine", BindingRevision: 1, RuntimeID: "runtime", RuntimeIncarnation: "host", RuntimeGeneration: 1}}
	client := &Client{Binding: api.Binding{Target: "machine", Capabilities: []string{"submission.acp"}}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	operation, err := client.ACPSubmit(ctx, key, api.ACPAction{Action: "prompt", Text: "not sent"})
	var submissionError *api.SubmissionError
	if !errors.Is(err, context.Canceled) || !errors.As(err, &submissionError) || submissionError.Key != key || operation.Submission == nil || operation.Submission.SubmissionKey != key || operation.Submission.Admission != api.SubmissionUnknown {
		t.Fatal("lost original prompt identity or cancellation", operation, err)
	}
	receipt, err := client.ACPControl(ctx, key, api.ACPAction{Action: "cancel", OperationRef: "exact-prompt"})
	if !errors.Is(err, context.Canceled) || !errors.As(err, &submissionError) || submissionError.Key != key || receipt.SubmissionKey != key || receipt.Admission != api.SubmissionUnknown {
		t.Fatal(receipt, err)
	}
	key.SubmissionID = ""
	operation, err = client.ACPSubmit(t.Context(), key, api.ACPAction{Action: "new"})
	var invalid *api.Error
	if !errors.As(err, &invalid) || invalid.Code != "INVALID_ARGUMENT" || operation.Submission.SubmissionID != "" {
		t.Fatal("missing ID was replaced", operation, err)
	}
}

func TestStopRequiresCallerKeyAndKeepsItAfterLocalCancellation(t *testing.T) {
	key := api.SubmissionKey{SubmissionID: "saved-stop", Target: api.SubmissionTarget{OwnerID: "owner", RunnerID: "runner", FabricID: "fabric", MachineID: "machine", BindingRevision: 1, RuntimeID: "runtime", RuntimeIncarnation: "host", RuntimeGeneration: 1}}
	client := &Client{Binding: api.Binding{Target: "machine", Capabilities: []string{"runtime.stop"}}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	receipt, err := client.Stop(ctx, key)
	var local *api.SubmissionError
	if !errors.Is(err, context.Canceled) || !errors.As(err, &local) || local.Key != key || receipt.SubmissionKey != key || receipt.Admission != api.SubmissionUnknown {
		t.Fatal("stop lost caller identity or fabricated acceptance", receipt, err)
	}
	key.SubmissionID = ""
	receipt, err = client.Stop(t.Context(), key)
	var invalid *api.Error
	if !errors.As(err, &invalid) || invalid.Code != "INVALID_ARGUMENT" || receipt.SubmissionID != "" {
		t.Fatal("stop supplied an implicit ID", receipt, err)
	}
}
