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
