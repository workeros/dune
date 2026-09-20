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
