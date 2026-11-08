package fleetingd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Sockets fit however long the disk and image directories are.
func TestSocketsFitUnderLongDirectories(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	long := filepath.Join(t.TempDir(), strings.Repeat("d", 120))
	g := &InstanceGroup{Name: "fleetingd", VMDiskDir: long, VMImageDir: long + "-images"}

	if err := g.prepareSocketDir(); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(g.socketDir()); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("socket directory: %v %v", fi, err)
	}
	api, vsock, err := g.templateSockets(filepath.Join(long+"-images", "snapshot-0123456789abcdef-01234567.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	vm, err := g.socketPath(g.instanceName(12) + ".vsock")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{api, vsock, vm} {
		if len(p) > maxSocketPath || filepath.Dir(p) != g.socketDir() {
			t.Errorf("socket %s (%d bytes)", p, len(p))
		}
	}
	if api == vsock {
		t.Error("template's API and vsock sockets are the same")
	}

	// Made afresh: what a previous run left goes.
	_ = os.WriteFile(filepath.Join(g.socketDir(), "old.vsock"), nil, 0o600)
	if err := g.prepareSocketDir(); err != nil {
		t.Fatal(err)
	}
	if left, _ := os.ReadDir(g.socketDir()); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}

	// Two disk directories, two socket directories.
	other := &InstanceGroup{Name: "fleetingd", VMDiskDir: long + "2"}
	if other.socketDir() == g.socketDir() {
		t.Error("two disk directories share a socket directory")
	}
}

// When even the socket directory is too long, a clear error before any VM.
func TestSocketPathTooLong(t *testing.T) {
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), strings.Repeat("t", 100)))
	g := &InstanceGroup{Name: "fleetingd", VMDiskDir: "/vms"}
	err := g.prepareSocketDir()
	if err == nil || !strings.Contains(err.Error(), "unix socket") {
		t.Fatalf("want a socket length error, got %v", err)
	}
}
