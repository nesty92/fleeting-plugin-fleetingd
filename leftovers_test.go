package fleetingd

import (
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestKillLeftovers(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	work := filepath.Join(dir, "vms", vmWorkdir)
	// A process that looks like a VM of a dead run, and one of another dir.
	vm := func(arg string) *exec.Cmd {
		c := exec.Command(sh, "-c", "sleep 600; :", arg)
		c.Args[0] = "cloud-hypervisor"
		return c
	}
	stale := vm("path=" + filepath.Join(work, "fd40.img") + ",image_type=qcow2")
	other := vm("path=" + filepath.Join(dir, "vms2", vmWorkdir, "fd40.img"))
	for _, c := range []*exec.Cmd{stale, other} {
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
	}
	defer other.Process.Kill()

	killed := killLeftovers(work)
	if len(killed) != 1 || killed[0] != stale.Process.Pid {
		t.Fatalf("killed %v, want only %d", killed, stale.Process.Pid)
	}
	done := make(chan error, 1)
	go func() { done <- stale.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("leftover still running")
	}
	if other.ProcessState != nil {
		t.Fatal("killed another directory's VM")
	}
}
