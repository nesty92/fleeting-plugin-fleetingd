package fleetingd

import (
	"testing"

	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

// TestTakeExited: a VM that stopped on its own before Update reported it
// is reported creating, so that fleeting counts it, then deleted, then not
// at all; one Update reported is deleted at once.
func TestTakeExited(t *testing.T) {
	i := NewInventory()
	i.exited["vm0"] = 0
	for n, want := range []provider.State{provider.StateCreating, provider.StateDeleted, ""} {
		got := i.TakeExited()["vm0"]
		if got != want {
			t.Fatalf("update %d: %q, want %q", n, got, want)
		}
	}
	if len(i.exited) != 0 {
		t.Fatalf("left: %v", i.exited)
	}
}

func TestTakeExitedReported(t *testing.T) {
	i := NewInventory()
	i.instances["vm1"] = &InstanceInfo{ID: "vm1"}
	i.MarkReported([]string{"vm1", "gone"})
	if !i.instances["vm1"].reported {
		t.Fatal("not marked")
	}
	i.exited["vm1"] = 1 // as the exit goroutine records a reported one
	for n, want := range []provider.State{provider.StateDeleted, ""} {
		if got := i.TakeExited()["vm1"]; got != want {
			t.Fatalf("update %d: %q, want %q", n, got, want)
		}
	}
}
