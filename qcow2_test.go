package fleetingd

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeQcow2 writes the start of a qcow2 header naming backing.
func writeQcow2(t *testing.T, path, backing string) {
	t.Helper()
	h := make([]byte, 512)
	copy(h, "QFI\xfb")
	binary.BigEndian.PutUint32(h[4:], 3)
	if backing != "" {
		binary.BigEndian.PutUint64(h[8:], 112)
		binary.BigEndian.PutUint32(h[16:], uint32(len(backing)))
		copy(h[112:], backing)
	}
	if err := os.WriteFile(path, h, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestQcow2Backing(t *testing.T) {
	dir := t.TempDir()
	for _, want := range []string{"/images/prebuilt-abc.qcow2", ""} {
		p := filepath.Join(dir, "d.qcow2")
		writeQcow2(t, p, want)
		if got, err := qcow2Backing(p); err != nil || got != want {
			t.Errorf("qcow2Backing = %q, %v; want %q", got, err, want)
		}
	}
	raw := filepath.Join(dir, "raw.img")
	_ = os.WriteFile(raw, make([]byte, 512), 0o600)
	if _, err := qcow2Backing(raw); err == nil {
		t.Error("a raw image passed as qcow2")
	}
}

// fakeSnapshot lays out a snapshot in dir as buildSnapshot leaves it.
func fakeSnapshot(t *testing.T, dir, builtIn, backing string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "snap"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"snap/config.json", "snap/state.json"} {
		_ = os.WriteFile(filepath.Join(dir, f), []byte("{}"), 0o600)
	}
	writeQcow2(t, filepath.Join(dir, "disk.qcow2"), backing)
	meta, _ := json.Marshal(snapshot{MAC: "aa:bb", BuiltIn: builtIn})
	_ = os.WriteFile(filepath.Join(dir, "meta.json"), meta, 0o600)
}

func TestLoadSnapshotRefusesStalePaths(t *testing.T) {
	images := t.TempDir()
	image := filepath.Join(images, "prebuilt-abc.qcow2")
	dir := filepath.Join(images, "snapshot-abc-11111111-22222222")
	for _, c := range []struct {
		name, builtIn, backing, why string
	}{
		{"usable", dir + ".tmp", image, ""},
		{"directory moved", "/old/images/snapshot-abc-11111111-22222222.tmp", "/old/images/prebuilt-abc.qcow2", "built in"},
		{"other image", dir + ".tmp", filepath.Join(images, "prebuilt-old.qcow2"), "overlay on"},
	} {
		_ = os.RemoveAll(dir)
		fakeSnapshot(t, dir, c.builtIn, c.backing)
		s, err := loadSnapshot(dir, image)
		switch {
		case c.why == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.why == "" && s.Disk != filepath.Join(dir, "disk.qcow2"):
			t.Errorf("%s: disk %s", c.name, s.Disk)
		case c.why != "" && (err == nil || !strings.Contains(err.Error(), c.why)):
			t.Errorf("%s: err %v, want one saying %q", c.name, err, c.why)
		}
	}
	if _, err := loadSnapshot(filepath.Join(images, "none"), image); !os.IsNotExist(err) {
		t.Errorf("missing snapshot: %v, want not-exist (built without a warning)", err)
	}
}
