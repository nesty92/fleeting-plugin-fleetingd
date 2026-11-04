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
