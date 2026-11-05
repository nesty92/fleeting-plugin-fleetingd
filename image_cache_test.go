package fleetingd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hashicorp/go-hclog"
	"golang.org/x/sys/unix"
)

func TestImageKey(t *testing.T) {
	a := &InstanceGroup{VMPrebuildCloudinitExtraCmds: []string{"apt-get install -y docker.io"}}
	b := &InstanceGroup{VMPrebuildCloudinitExtraCmds: []string{"apt-get install -y docker.io"}, VMNumCPUCores: 8, VMDiskSizeGB: 100}
	c := &InstanceGroup{VMPrebuildCloudinitExtraCmds: []string{"apt-get install -y podman"}}
	ka, _ := a.imageKey("disk", "kernel")
	kb, _ := b.imageKey("disk", "kernel")
	kc, _ := c.imageKey("disk", "kernel")
	kd, _ := a.imageKey("disk2", "kernel")
	if ka != kb {
		t.Error("CPU, memory and disk size must not change the image")
	}
	if ka == kc || ka == kd {
		t.Error("commands and the cloud image must change the image")
	}
	// No ambiguity from joining: ["a b"] is not ["a", "b"].
	k1, _ := (&InstanceGroup{VMPrebuildCloudinitExtraCmds: []string{"a b"}}).imageKey("d", "k")
	k2, _ := (&InstanceGroup{VMPrebuildCloudinitExtraCmds: []string{"a", "b"}}).imageKey("d", "k")
	if k1 == k2 {
		t.Error("command lists collide")
	}
}

func TestRemoveUnusedImages(t *testing.T) {
	dir := t.TempDir()
	touch := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	keep := touch("prebuilt-new.qcow2")
	held := touch("prebuilt-held.qcow2")
	touch("prebuilt-held.vmlinuz")
	stale := touch("prebuilt-stale.qcow2")
	touch("prebuilt-stale.vmlinuz")

	// Another group still runs on "held".
	f, _ := os.Open(held)
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_SH); err != nil {
		t.Fatal(err)
	}

	g := &InstanceGroup{VMImageDir: dir, logger: hclog.NewNullLogger()}
	g.removeUnusedImages(keep)

	for p, want := range map[string]bool{keep: true, held: true, stale: false, filepath.Join(dir, "prebuilt-stale.vmlinuz"): false} {
		if ok, _ := checkFileExists(p); ok != want {
			t.Errorf("%s exists=%v, want %v", filepath.Base(p), ok, want)
		}
	}
}
