package sessionregistry

import (
	"os"
	"testing"

	"github.com/aiomni/dune/internal/statecontract"
	"github.com/aiomni/dune/pkg/api"
)

func TestSharedContractRefusesReopenWithoutChangingAcceptedEvidence(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	registry, err := Open(t.Context(), dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	key := api.SubmissionKey{SubmissionID: "retained", Target: api.SubmissionTarget{OwnerID: "owner", RunnerID: "runner", FabricID: "fabric", MachineID: "machine", BindingRevision: 1}}
	claim, _, err := registry.ClaimKey(t.Context(), key, Digest("launch", nil), "receiver")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Reject(t.Context(), claim, "UPGRADE_IN_PROGRESS"); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.db.ExecContext(t.Context(), `UPDATE registry_settings SET shared_contract='incompatible-writer'`); err != nil {
		t.Fatal(err)
	}
	if other, err := Open(t.Context(), dir, Options{}); err == nil {
		other.Close()
		t.Fatal("writer accepted another contract")
	}
	if other, err := OpenReadOnly(t.Context(), dir); err == nil {
		other.Close()
		t.Fatal("preview accepted another contract")
	}
	var contract string
	if err := registry.db.QueryRowContext(t.Context(), `SELECT shared_contract FROM registry_settings`).Scan(&contract); err != nil || contract != "incompatible-writer" {
		t.Fatal("open migrated state", contract, err)
	}
	if _, err := registry.db.ExecContext(t.Context(), `UPDATE registry_settings SET shared_contract=?`, statecontract.ID()); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(t.Context(), dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	receipt, err := reopened.Get(t.Context(), key)
	if err != nil || receipt.Admission != api.SubmissionNotAccepted || receipt.ErrorCode != "UPGRADE_IN_PROGRESS" {
		t.Fatal("contract check lost evidence", receipt, err)
	}
}
