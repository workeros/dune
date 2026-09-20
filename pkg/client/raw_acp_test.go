package client

import (
	"context"
	"errors"
	"testing"

	"github.com/aiomni/dune/pkg/api"
)

func TestRawMutationsRetainCallerKeysOnCancellation(t *testing.T) {
	key := api.SubmissionKey{SubmissionID: "saved-raw", Target: api.SubmissionTarget{OwnerID: "owner", RunnerID: "runner", FabricID: "fabric", MachineID: "machine", BindingRevision: 1, RuntimeID: "runtime", RuntimeIncarnation: "host", RuntimeGeneration: 1}}
	c := &Client{Binding: api.Binding{Target: "machine", Capabilities: []string{"submission.raw"}}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, invoke := range []func(api.SubmissionKey) (api.SubmissionReceipt, error){
		func(k api.SubmissionKey) (api.SubmissionReceipt, error) {
			return c.TakeRawACP(ctx, k, api.RawACPTake{StreamID: "stream", OwnerID: "caller"})
		},
		func(k api.SubmissionKey) (api.SubmissionReceipt, error) {
			return c.WriteRawACP(ctx, k, api.RawACPWrite{StreamID: "stream", InputEpoch: 1, OwnerID: "caller"})
		},
	} {
		receipt, err := invoke(key)
		var failure *api.SubmissionError
		if !errors.Is(err, context.Canceled) || !errors.As(err, &failure) || failure.Key != key || receipt.SubmissionKey != key || receipt.Admission != api.SubmissionUnknown {
			t.Fatal(receipt, err)
		}
		missing := key
		missing.SubmissionID = ""
		receipt, err = invoke(missing)
		var invalid *api.Error
		if !errors.As(err, &invalid) || invalid.Code != "INVALID_ARGUMENT" || receipt.SubmissionKey != missing {
			t.Fatal(receipt, err)
		}
	}
}
