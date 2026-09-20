package sessionregistry

import (
	"testing"

	"github.com/aiomni/dune/pkg/api"
)

func TestRawAdmissionOrderSurvivesReopenAndProgress(t *testing.T) {
	dir := privateDirectory(t)
	r := openRegistry(t, dir, 4)
	key := testKey()
	digest := Digest("acp.raw.write", []byte("original exact bytes"))
	claim, _, err := r.ClaimKey(t.Context(), key, digest, "host-one")
	if err != nil {
		t.Fatal(err)
	}
	input := api.RawACPInputReceipt{StreamID: "original-stream", InputEpoch: 7, Sequence: 32, Length: 100}
	accepted, err := r.AcceptRaw(t.Context(), claim, "original-write", input)
	if err != nil || accepted.RawInput == nil || *accepted.RawInput != input {
		t.Fatal(accepted, err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r = openRegistry(t, dir, 4)
	for _, stage := range []string{"writing", "written"} {
		receipt, err := r.Progress(t.Context(), key, "original-write", stage, "", nil)
		if err != nil || receipt.RawInput == nil || *receipt.RawInput != input {
			t.Fatal(receipt, err)
		}
	}
	duplicate, receipt, err := r.ClaimKey(t.Context(), key, digest, "host-one")
	if err != nil || duplicate.Acquired() || receipt.Stage != "written" || receipt.RawInput == nil || *receipt.RawInput != input {
		t.Fatal(receipt, err)
	}
	_, _, err = r.ClaimKey(t.Context(), key, Digest("acp.raw.take", nil), "host-one")
	requireCode(t, err, "SUBMISSION_CONFLICT")
	input.Sequence++
	_, err = r.AcceptRaw(t.Context(), claim, "original-write", input)
	requireCode(t, err, "SUBMISSION_CONFLICT")
}
