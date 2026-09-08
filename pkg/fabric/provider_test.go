package fabric

import "testing"

func TestAvailabilityValidation(t *testing.T) {
	valid := []Availability{
		{Available: true},
		{Reason: AvailabilityMaintenance},
		{Reason: AvailabilityCapacity},
		{Reason: AvailabilityConfiguration},
		{Reason: AvailabilityUnreachable},
		{Reason: AvailabilityUnknown},
	}
	for _, status := range valid {
		if !status.Valid() {
			t.Fatal("valid availability rejected", status)
		}
	}
	for _, status := range []Availability{{Available: true, Reason: AvailabilityCapacity}, {}, {Reason: "private-reason"}} {
		if status.Valid() {
			t.Fatal("invalid availability accepted", status)
		}
	}
}
