package fleetingd

import (
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"
)

const snapConfig = `{
  "disks": [{"path": "/img/snapshot-a.tmp/disk.qcow2", "backing_files": true},
            {"path": "/img/snapshot-a.tmp/seed.img", "readonly": true}],
  "net": [{"tap": "fleetingd0", "ip": "172.16.120.1", "mask": "255.255.255.252", "mac": "de:51:00:00:00:01"}],
  "vsock": {"cid": 3, "socket": "/img/snapshot-a.tmp/vsock"},
  "console": {"mode": "Off", "file": null},
  "landlock_enable": true,
  "landlock_rules": [{"path": "/img/snapshot-a.tmp", "access": "rw"}]
}`

func TestEditRestoreConfig(t *testing.T) {
	raw := strings.ReplaceAll(snapConfig, "/img/snapshot-a.tmp", "/img/snapshot-a")
	var cfg map[string]any
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	err := editRestoreConfig(cfg, restoreSettings{
		TemplateDisk: "/img/snapshot-a/disk.qcow2",
		Disk:         "/vms/w/fleetingd3.img",
		Tap:          "fleetingd3",
		HostIP:       "172.16.120.13",
		Vsock:        "/vms/w/fleetingd3.vsock",
		Console:      "/vms/w/fleetingd3_console",
		Landlock:     []string{"/img"},
		Writable:     "/vms/w",
	})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(cfg)
	for _, want := range []string{
		`"path":"/vms/w/fleetingd3.img"`,
		`"path":"/img/snapshot-a/seed.img"`,
		`"tap":"fleetingd3"`,
		`"ip":"172.16.120.13"`,
		`"mac":"de:51:00:00:00:01"`,
		`"socket":"/vms/w/fleetingd3.vsock"`,
		`"file":"/vms/w/fleetingd3_console","mode":"File"`,
		`"landlock_rules":[{"access":"rw","path":"/vms/w"},{"access":"r","path":"/img"}]`,
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("config lacks %s:\n%s", want, out)
		}
	}
	if strings.Contains(string(out), ".tmp") {
		t.Errorf("config still names the build directory:\n%s", out)
	}
}

func TestEditRestoreConfigNeedsTheTemplateDisk(t *testing.T) {
	var cfg map[string]any
	_ = json.Unmarshal([]byte(snapConfig), &cfg)
	if err := editRestoreConfig(cfg, restoreSettings{TemplateDisk: "/elsewhere.qcow2"}); err == nil {
		t.Fatal("want an error for a config without the template's disk")
	}
}

func TestIdentityScript(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	s, err := identityScript("fleetingd3", "172.16.120.14", "172.16.120.13", pub)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ip addr add 172.16.120.14/30 dev veth0", "via 172.16.120.13", "ssh-ed25519 ", "hostnamectl set-hostname fleetingd3", "/etc/machine-id"} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q", want)
		}
	}
}
