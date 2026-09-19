package fabricd

import (
	"testing"
)

func TestTerminalControlTakeoverFencesPendingInput(t *testing.T) {
	var control terminalControl
	first := &subscription{canInput: true, controlChanged: make(chan struct{}, 1)}
	second := &subscription{canInput: true, controlChanged: make(chan struct{}, 1)}
	observer := &subscription{controlChanged: make(chan struct{}, 1)}
	control.add(first)
	control.add(second)
	control.add(observer)
	_, originalEpoch := control.state(first)
	if state, epoch := control.state(second); state.Writable || epoch != 0 {
		t.Fatal("second viewer stole input")
	}
	if err := control.change(observer, "take"); err == nil {
		t.Fatal("observer escalated its access")
	}
	if err := control.change(second, "acquire"); err != nil {
		t.Fatal(err)
	}
	if state, _ := control.state(first); !state.Writable {
		t.Fatal("acquire preempted the owner")
	}
	if err := control.change(second, "take"); err != nil {
		t.Fatal(err)
	}
	if err := control.write(first, originalEpoch, func() error { t.Fatal("old queued input executed"); return nil }); err == nil {
		t.Fatal("old input was accepted")
	}
	if err := control.change(first, "take"); err != nil {
		t.Fatal(err)
	}
	if err := control.write(first, originalEpoch, func() error { t.Fatal("buffered input from a previous term executed"); return nil }); err == nil {
		t.Fatal("old epoch was accepted after taking back control")
	}
	_, currentEpoch := control.state(first)
	wrote := false
	if err := control.write(first, currentEpoch, func() error { wrote = true; return nil }); err != nil || !wrote {
		t.Fatal("new owner cannot write", err)
	}
	control.remove(second)
	if state, _ := control.state(first); !state.Writable {
		t.Fatal("late cleanup revoked the new owner")
	}
	control.remove(first)
	if state, _ := control.state(observer); state.Writable || !state.Available {
		t.Fatal("disconnect did not release input")
	}
}
