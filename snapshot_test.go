package fleetingd

import (
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"
	"time"
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
	s, err := identityScript("fleetingd3", "172.16.120.14", "172.16.120.13", pub, time.Unix(1793840000, 5))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ip addr add 172.16.120.14/30 dev veth0", "via 172.16.120.13", "ssh-ed25519 ", "hostnamectl set-hostname fleetingd3", "/etc/machine-id", "date -s @1793840000.000000005", "echo clock_was=$(date +%s.%N)"} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q", want)
		}
	}
}

func TestIsLeftover(t *testing.T) {
	cmd := func(args ...string) []byte { return []byte(strings.Join(args, "\x00") + "\x00") }
	work := []byte("/vms/.instance_data/")
	for _, c := range []struct {
		cmdline []byte
		prefix  []byte
		want    bool
	}{
		{cmd("cloud-hypervisor", "--disk", "path=/vms/.instance_data/fd40.img,image_type=qcow2"), work, true},
		{cmd("/usr/bin/cloud-hypervisor", "--restore", "source_url=file:///vms/.instance_data/fd40_snap,resume=true"), work, true},
		{cmd("cloud-hypervisor", "--api-socket", "path=/vms/.instance_data2/x.sock"), work, false},
		{cmd("qemu-img", "create", "/vms/.instance_data/fd40.img"), work, false},
		{cmd("cloud-hypervisor", "--disk", "path=/img/prebuilt-a.qcow2.tmp,image_type=qcow2"), []byte("/img/prebuilt-a.qcow2.tmp/"), true},
		{cmd("cloud-hypervisor", "--vsock", "cid=3,socket=/vms/.instance_data/fd40.vsock"), work, true},
	} {
		if got := isLeftover(c.cmdline, c.prefix); got != c.want {
			t.Errorf("isLeftover(%q, %q) = %v, want %v", c.cmdline, c.prefix, got, c.want)
		}
	}
}

func TestClockDrift(t *testing.T) {
	at := time.Unix(1793843600, 0)
	d, ok := clockDrift("OK 1\nclock_was=1793840000.250000000\n", at)
	if !ok || d != time.Hour-250*time.Millisecond {
		t.Fatalf("drift %v %v, want 59m59.75s", d, ok)
	}
	if _, ok := clockDrift("nothing here", at); ok {
		t.Fatal("drift from output without it")
	}
}
