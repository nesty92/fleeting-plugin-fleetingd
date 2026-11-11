package fleetingd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstanceSocket(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	i := &InstanceGroup{Name: "ci", VMDiskDir: t.TempDir()}
	if err := i.prepareSocketDir(); err != nil {
		t.Fatal(err)
	}
	sock, _ := i.instanceAPISocket("ci4")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := i.instanceSocket("ci4-ab12cd7"); err != nil || got != sock {
		t.Errorf("instanceSocket(ci4-ab12cd7) = %q, %v; want %q", got, err, sock)
	}
	for _, bad := range []string{"ci8-ab12cd1", "ci-ab12cd1", "other4-ab12cd1", "ci4x-ab12cd1", "../ci4-x", ""} {
		if _, err := i.instanceSocket(bad); err == nil {
			t.Errorf("instanceSocket(%q) found a socket", bad)
		}
	}
}

func TestCacheLayer(t *testing.T) {
	root := t.TempDir()
	cache := filepath.Join(root, "cache")
	if err := os.MkdirAll(filepath.Join(cache, "gl", "1"), 0o700); err != nil {
		t.Fatal(err)
	}
	layer := filepath.Join(cache, "gl", "1", "layer-3.qcow2")
	outside := filepath.Join(root, "outside.qcow2")
	for _, f := range []string{layer, outside} {
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(cache, "link.qcow2")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	i := &InstanceGroup{VMCacheDir: cache}
	if got, err := i.cacheLayer(layer); err != nil || got != layer {
		t.Errorf("cacheLayer(%s) = %q, %v", layer, got, err)
	}
	for _, bad := range []string{outside, link, cache, filepath.Join(cache, "gl"), filepath.Join(cache, "..", "outside.qcow2"), "layer.qcow2", filepath.Join(cache, "missing.qcow2")} {
		if _, err := i.cacheLayer(bad); err == nil {
			t.Errorf("cacheLayer(%s) took it", bad)
		}
	}
}

func TestDiskNeedsCacheDirectory(t *testing.T) {
	err := Disk([]byte(`{"vm_disk_directory": "/vms"}`), []string{"detach", "fleetingd1-a1"})
	if err == nil || !strings.Contains(err.Error(), "vm_cache_directory") {
		t.Errorf("Disk without vm_cache_directory: %v", err)
	}
	err = Disk([]byte(`{"vm_disk_directory": "/vms", "vm_cache_directory": "cache"}`), []string{"detach", "fleetingd1-a1"})
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Errorf("Disk with a relative vm_cache_directory: %v", err)
	}
}
