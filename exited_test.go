package fleetingd

import (
	"testing"

	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

// TestTakeExited: a VM that stopped on its own is reported creating, so
// that fleeting counts it even if it never saw it, then deleted, then not
// at all.
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
