package fleetingd

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"text/template"
)

func renderPrebuild(t *testing.T, skipGitlabRunner bool, cmds ...string) string {
	t.Helper()
	tpl, err := template.ParseFS(userDataTemplates, "templates/*.tpl")
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	err = tpl.ExecuteTemplate(&b, "user-data-prebuild.tpl", map[string]any{
		"InstanceName": "fd0", "ExtraCommands": cmds,
		"SkipGitlabRunner": skipGitlabRunner, "FailedMarker": prebuildFailedMarker,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestPrebuildGitlabRunner(t *testing.T) {
	if out := renderPrebuild(t, false); !strings.Contains(out, "apt install -y gitlab-runner") {
		t.Errorf("gitlab-runner missing:\n%s", out)
	}
	if out := renderPrebuild(t, true); strings.Contains(out, "gitlab-runner") {
		t.Errorf("gitlab-runner left in:\n%s", out)
	}
}

// The status lines the template adds report a failed command, and only it,
// when run the way cloud-init runs runcmd: one sh script, no -e.
func TestPrebuildReportsFailedCommands(t *testing.T) {
	cmds := []string{"true", "false", "echo fine", "sh -c 'exit 7' | cat; exit_is_cat=$?", "sh -c 'exit 3'"}
	out := renderPrebuild(t, true, cmds...)
	var script []string
	in := false
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.Contains(line, "CUSTOM COMMANDS START"):
			in = true
		case strings.Contains(line, "CUSTOM COMMANDS END"):
			in = false
		case in && strings.HasPrefix(line, "  - "):
			l := strings.TrimPrefix(line, "  - ")
			if strings.HasPrefix(l, "'") { // YAML single-quoted
				l = strings.ReplaceAll(strings.Trim(l, "'"), "''", "'")
			}
			script = append(script, strings.ReplaceAll(l, "/dev/hvc0", "/dev/stdout"))
		}
	}
	if len(script) != 2*len(cmds) {
		t.Fatalf("%d lines for %d commands:\n%s", len(script), len(cmds), out)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "runcmd")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+strings.Join(script, "\n")+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	console, err := exec.Command("sh", path).Output()
	if err != nil {
		t.Fatalf("%v: %s", err, console)
	}
	want := []string{"#2 (exit 1): false", "#5 (exit 3): sh -c 'exit 3'"}
	if got := prebuildFailures(console, cmds); !reflect.DeepEqual(got, want) {
		t.Errorf("failures %q, want %q; console:\n%s", got, want, console)
	}
}

func TestPrebuildFailuresIgnoresNoise(t *testing.T) {
	console := []byte("[  1.0] cloud-init: something\r\n" + prebuildFailedMarker + " 9 1\n" +
		prebuildFailedMarker + " x 1\n  " + prebuildFailedMarker + " 0 2\r\n")
	want := []string{"#1 (exit 2): a"}
	if got := prebuildFailures(console, []string{"a"}); !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}
