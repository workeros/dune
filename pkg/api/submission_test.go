package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestSubmissionIdentityRejectsNormalizationAndPartialTargets(t *testing.T) {
	for _, id := range []string{"", " padded", "padded ", "two words", "a/b", "a\n", "é", strings.Repeat("a", 129)} {
		if ValidateSubmissionID(id) == nil {
			t.Errorf("accepted invalid ID %q", id)
		}
	}
	key := SubmissionKey{SubmissionID: "caller-01_ABC", Target: SubmissionTarget{
		OwnerID: "tenant", RunnerID: "runner", FabricID: "fabric", MachineID: "machine", BindingRevision: 1,
	}}
	if err := key.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, target := range []SubmissionTarget{
		{OwnerID: "tenant", RunnerID: "runner", FabricID: "fabric", MachineID: "machine"},
		{OwnerID: "tenant", RunnerID: "runner", FabricID: "fabric", MachineID: "machine", BindingRevision: 1, RuntimeID: "runtime"},
		{OwnerID: "tenant", RunnerID: "runner", FabricID: "fabric", MachineID: "machine", BindingRevision: 1, RuntimeIncarnation: "inc", RuntimeGeneration: 1},
	} {
		if target.Validate() == nil {
			t.Fatalf("accepted partial target %+v", target)
		}
	}
	key.Target.RuntimeID, key.Target.RuntimeIncarnation, key.Target.RuntimeGeneration = "runtime", "inc", 1
	if err := key.Validate(); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(key)
	if err != nil {
		t.Fatal(err)
	}
	var restored SubmissionKey
	if err := json.Unmarshal(encoded, &restored); err != nil || restored != key {
		t.Fatalf("caller query key changed: %s, %v", encoded, err)
	}
}

func TestSubmissionLocalFailureRetainsKeyAndCause(t *testing.T) {
	key := SubmissionKey{SubmissionID: "before-first-send", Target: SubmissionTarget{OwnerID: "tenant"}}
	err := &SubmissionError{Key: key, Cause: context.Canceled}
	var failure *SubmissionError
	if !errors.Is(err, context.Canceled) || !errors.As(err, &failure) || failure.Key != key {
		t.Fatal("local failure lost the caller's key or cancellation cause")
	}
	remote := &Error{Code: "SESSION_UNAVAILABLE", Detail: "session host is unavailable"}
	var apiError *Error
	if !errors.As(&SubmissionError{Key: key, Cause: remote}, &apiError) || apiError != remote {
		t.Fatal("submission metadata obscured the structured server error")
	}
}
