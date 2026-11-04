package fleetingd

import (
	"strings"
	"testing"
	"text/template"
)

func renderUserData(t *testing.T, upgrade bool) string {
	t.Helper()
	tpl, err := template.ParseFS(userDataTemplates, "templates/*.tpl")
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	err = tpl.ExecuteTemplate(&b, "user-data.tpl", map[string]any{
		"InstanceName":           "fleetingd0",
		"Gateway":                "172.16.120.1",
		"SSHAuthorizedPublicKey": "ssh-ed25519 AAAA",
		"PackageUpgrade":         upgrade,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestUserDataPackageUpgrade(t *testing.T) {
	if out := renderUserData(t, true); !strings.Contains(out, "package_upgrade: true") {
		t.Errorf("upgrade on, but:\n%s", out)
	}
	out := renderUserData(t, false)
	if strings.Contains(out, "package_") {
		t.Errorf("upgrade off, but:\n%s", out)
	}
	if !strings.HasPrefix(out, "#cloud-config\nhostname: fleetingd0\ndisable_root: true\n") {
		t.Errorf("header:\n%s", out)
	}
}

func TestPackageUpgradeDefault(t *testing.T) {
	no := false
	if !(&InstanceGroup{}).packageUpgrade() {
		t.Error("default should upgrade")
	}
	if (&InstanceGroup{VMPackageUpgrade: &no}).packageUpgrade() {
		t.Error("false should not upgrade")
	}
}

func TestNftablesPerGroup(t *testing.T) {
	tpl, err := template.ParseFS(userDataTemplates, "templates/*.tpl")
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	err = tpl.ExecuteTemplate(&b, "nftables-rules.tpl", map[string]any{
		"Name": "fd2", "Subnet": "172.16.121.", "EgressInterface": "eth0",
		"Instances": []map[string]string{{
			"Name": "fd20", "InstanceTapIP": "172.16.121.2",
			"InstanceTapMacAddress": "de:51:00:00:00:01", "InstanceGateway": "172.16.121.1",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"table ip fd2forwarding {", "table netdev fd2filter {", "table ip fd2snat {",
		"policy accept;", `iifname "fd20" counter drop;`, "ip daddr 172.16.121.0/24 counter drop;",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "fleetingd") || strings.Contains(out, "172.16.120.") {
		t.Errorf("default names left in:\n%s", out)
	}
}

func TestValidate(t *testing.T) {
	g := &InstanceGroup{}
	if err := g.validate(); err != nil || g.Name != "fleetingd" || g.VMSubnet != VMPrefix {
		t.Errorf("defaults: %v %q %q", err, g.Name, g.VMSubnet)
	}
	for _, bad := range []InstanceGroup{
		{Name: "Fd"}, {Name: "toolongname1"}, {Name: "fd-2"},
		{VMSubnet: "172.16.120"}, {VMSubnet: "172.16.300."}, {VMSubnet: "10.0."},
	} {
		if err := bad.validate(); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestDiskDirLock(t *testing.T) {
	dir := t.TempDir()
	a := &InstanceGroup{VMDiskDir: dir}
	if err := a.lockDiskDir(); err != nil {
		t.Fatal(err)
	}
	defer a.lockFile.Close()
	if err := (&InstanceGroup{VMDiskDir: dir}).lockDiskDir(); err == nil {
		t.Error("second group got the same directory")
	}
}
