package fleetingd

// vm_snapshot: VMs are restored from a snapshot of a booted VM instead of
// booting.
//
// Once the shared image exists, each VM size gets one snapshot, next to the
// image: a template VM boots from it with a guest agent, waits until Docker
// answers, and is paused and saved. A new VM is then a qcow2 overlay on the
// template's disk plus a copy of the snapshot's config with its own tap,
// host address, vsock socket and console, restored with its memory mapped
// copy-on-write from the snapshot. It keeps fleetingd's network model: its
// own tap and /30, the same nftables rules.
//
// Before anything else talks to it, the host gives the restored VM its
// address, its SSH key and a new identity through the agent: systemd socket
// activation on vsock port 52, one root shell per connection. Only the host
// can reach it, through cloud-hypervisor's vsock socket.

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

const agentPort = 52
const agentDone = "__fleetingd_agent_ok__"

// snapshot is a built snapshot in use.
type snapshot struct {
	Dir  string `json:"-"`
	MAC  string `json:"mac"`
	Disk string `json:"-"`
	// BuiltIn is where it was built; paths in its config start there.
	BuiltIn string `json:"built_in"`
}

// bootOptions changes what BootInstance starts.
type bootOptions struct {
	// Template boots the VM a snapshot is taken of, with its disk, seed and
	// sockets in this directory, which outlive it.
	Template string
	// Info receives the started instance.
	Info *InstanceInfo
}

func (o *bootOptions) template() bool { return o != nil && o.Template != "" }

// snapshotKey names the snapshot for this image, VM size and hypervisor.
func (i *InstanceGroup) snapshotKey() (string, error) {
	out, err := exec.Command("cloud-hypervisor", "--version").Output()
	if err != nil {
		return "", fmt.Errorf("cloud-hypervisor --version: %w", err)
	}
	tpl, err := userDataTemplates.ReadFile("templates/user-data-snapshot.tpl")
	if err != nil {
		return "", err
	}
	h := sha256.New()
	fmt.Fprintf(h, "%s\n%s\n%d\n%d\n%d\n%t\n%t\n%s\n%s", filepath.Base(i.imagePath), strings.TrimSpace(string(out)),
		i.VMNumCPUCores, i.VMMemoryMegabytes, i.VMDiskSizeGB, i.packageUpgrade(), i.VMEnableVirtioConsole, i.VMSubnet, tpl)
	return hex.EncodeToString(h.Sum(nil))[:12], nil
}

// ensureSnapshot builds this group's snapshot unless it exists, and uses it.
func (i *InstanceGroup) ensureSnapshot() error {
	key, err := i.snapshotKey()
	if err != nil {
		return err
	}
	image := strings.TrimSuffix(filepath.Base(i.imagePath), ".qcow2")
	dir := filepath.Join(i.imageDir(), fmt.Sprintf("snapshot-%s-%s", strings.TrimPrefix(image, "prebuilt-"), key))

	lock, err := os.OpenFile(dir+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)

	if s, err := loadSnapshot(dir); err == nil {
		i.logger.Info("reusing the snapshot", "snapshot", filepath.Base(dir))
		i.snapshot = s
		return nil
	}
	s, err := i.buildSnapshot(dir)
	if err != nil {
		return err
	}
	i.snapshot = s
	return nil
}

func loadSnapshot(dir string) (*snapshot, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return nil, err
	}
	var s snapshot
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	for _, f := range []string{"snap/config.json", "snap/state.json", "disk.qcow2"} {
		if ok, _ := checkFileExists(filepath.Join(dir, f)); !ok {
			return nil, fmt.Errorf("snapshot %s lacks %s", dir, f)
		}
	}
	s.Dir, s.Disk = dir, filepath.Join(dir, "disk.qcow2")
	return &s, nil
}

func (i *InstanceGroup) buildSnapshot(dir string) (*snapshot, error) {
	tmp := dir + ".tmp"
	_ = os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	i.logger.Info("building the snapshot", "snapshot", filepath.Base(dir))
	start := time.Now()
	opts := &bootOptions{Template: tmp}
	if err := i.inventory.BootInstance(i, opts); err != nil {
		return nil, fmt.Errorf("booting the snapshot template: %w", err)
	}
	tpl := opts.Info
	stop := func() {
		tpl.InstanceContextCancelFunc()
		i.inventory.waitGone(tpl.Name, 30*time.Second)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	vsock := filepath.Join(tmp, "vsock")
	for {
		_, err := runAgent(ctx, vsock, `cloud-init status --wait >/dev/null 2>&1 || true
docker version --format '{{.Server.Version}}' >/dev/null`)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			stop()
			return nil, fmt.Errorf("snapshot template never got ready: %w", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	i.logger.Info("snapshot template ready", "after", time.Since(start).Round(time.Millisecond).String())

	api := filepath.Join(tmp, "api.sock")
	if err := chAPI(ctx, api, "vm.pause", nil); err != nil {
		stop()
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(tmp, "snap"), 0o700); err != nil {
		stop()
		return nil, err
	}
	if err := chAPI(ctx, api, "vm.snapshot", map[string]string{"destination_url": "file://" + filepath.Join(tmp, "snap")}); err != nil {
		stop()
		return nil, err
	}
	stop()
	// Every VM restored from it reads this disk through its own overlay.
	for _, f := range []string{"disk.qcow2", "seed.img"} {
		if err := os.Chmod(filepath.Join(tmp, f), 0o400); err != nil {
			return nil, err
		}
	}

	s := &snapshot{MAC: tpl.InstanceTapMacAddress, BuiltIn: tmp}
	meta, _ := json.Marshal(s)
	if err := os.WriteFile(filepath.Join(tmp, "meta.json"), meta, 0o600); err != nil {
		return nil, err
	}
	for _, f := range []string{"api.sock", "vsock"} {
		_ = os.Remove(filepath.Join(tmp, f))
	}
	_ = os.RemoveAll(dir)
	if err := os.Rename(tmp, dir); err != nil {
		return nil, err
	}
	i.logger.Info("snapshot built", "snapshot", filepath.Base(dir), "took", time.Since(start).Round(time.Millisecond).String())
	return loadSnapshot(dir)
}

// prepareRestore writes this VM's copy of the snapshot: its own disk, tap,
// host address, vsock socket and console. The memory file is shared.
func (i *InstanceGroup) prepareRestore(name, hostIP, disk string) (string, error) {
	s := i.snapshot
	work := filepath.Join(i.VMDiskDir, vmWorkdir)
	dir := filepath.Join(work, name+"_snap")
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	raw, err := os.ReadFile(filepath.Join(s.Dir, "snap", "config.json"))
	if err != nil {
		return "", err
	}
	// The snapshot was taken in its build directory, then moved.
	raw = []byte(strings.ReplaceAll(string(raw), s.BuiltIn, s.Dir))
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return "", err
	}
	if err := editRestoreConfig(cfg, restoreSettings{
		TemplateDisk: s.Disk,
		Disk:         disk,
		Tap:          name,
		HostIP:       hostIP,
		Vsock:        filepath.Join(work, name+".vsock"),
		Console:      consoleFile(i, name),
		Landlock:     []string{i.imageDir()},
		Writable:     work,
	}); err != nil {
		return "", err
	}
	out, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), out, 0o600); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(filepath.Join(s.Dir, "snap"))
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		src := filepath.Join(s.Dir, "snap", e.Name())
		switch e.Name() {
		case "config.json":
		case "state.json":
			b, err := os.ReadFile(src)
			if err != nil {
				return "", err
			}
			if err := os.WriteFile(filepath.Join(dir, e.Name()), b, 0o600); err != nil {
				return "", err
			}
		default:
			if err := os.Symlink(src, filepath.Join(dir, e.Name())); err != nil {
				return "", err
			}
		}
	}
	return dir, nil
}

func consoleFile(i *InstanceGroup, name string) string {
	if !i.VMEnableVirtioConsole {
		return ""
	}
	return filepath.Join(i.VMDiskDir, vmWorkdir, name+"_console")
}

type restoreSettings struct {
	TemplateDisk, Disk, Tap, HostIP, Vsock, Console string
	Landlock                                        []string // read-only paths
	Writable                                        string
}

// editRestoreConfig points a copy of the snapshot's config at one VM.
func editRestoreConfig(cfg map[string]any, r restoreSettings) error {
	disks, _ := cfg["disks"].([]any)
	found := false
	for _, d := range disks {
		if m, ok := d.(map[string]any); ok && m["path"] == r.TemplateDisk {
			m["path"] = r.Disk
			found = true
		}
	}
	if !found {
		return fmt.Errorf("snapshot config has no disk %s", r.TemplateDisk)
	}
	nets, _ := cfg["net"].([]any)
	if len(nets) != 1 {
		return fmt.Errorf("snapshot config has %d network devices, want 1", len(nets))
	}
	n := nets[0].(map[string]any)
	n["tap"], n["ip"], n["mask"] = r.Tap, r.HostIP, "255.255.255.252"
	v, ok := cfg["vsock"].(map[string]any)
	if !ok {
		return errors.New("snapshot config has no vsock device")
	}
	v["socket"] = r.Vsock
	if c, ok := cfg["console"].(map[string]any); ok {
		if r.Console != "" {
			c["mode"], c["file"] = "File", r.Console
		} else {
			c["mode"], c["file"] = "Off", nil
		}
	}
	rules := []any{map[string]any{"path": r.Writable, "access": "rw"}}
	for _, p := range r.Landlock {
		rules = append(rules, map[string]any{"path": p, "access": "r"})
	}
	cfg["landlock_enable"], cfg["landlock_rules"] = true, rules
	return nil
}

// identityScript gives a restored VM its own address, key and identity.
func identityScript(name, ip, gateway string, key ed25519.PublicKey) (string, error) {
	pub, err := ssh.NewPublicKey(key)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`set -e
ip addr flush dev veth0
ip addr add %[2]s/30 dev veth0
ip route replace default via %[3]s
sed -i -E 's|- [0-9.]+/30|- %[2]s/30|; s|via: [0-9.]+|via: %[3]s|' /etc/netplan/50-cloud-init.yaml 2>/dev/null || true
install -d -m 700 -o ubuntu -g ubuntu /home/ubuntu/.ssh
echo '%[4]s' > /home/ubuntu/.ssh/authorized_keys
chown ubuntu:ubuntu /home/ubuntu/.ssh/authorized_keys
chmod 600 /home/ubuntu/.ssh/authorized_keys
rm -f /etc/ssh/ssh_host_*
ssh-keygen -A >/dev/null
systemctl reload ssh || systemctl restart ssh
od -An -tx1 -N16 /dev/urandom | tr -d ' \n' > /etc/machine-id
head -c 512 /dev/urandom > /dev/urandom
chronyc -a makestep >/dev/null 2>&1 || true
hostnamectl set-hostname %[1]s 2>/dev/null || hostname %[1]s
`, name, ip, gateway, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))), nil
}

// runAgent runs a script as root in the VM behind a cloud-hypervisor vsock
// socket and returns its output. The script fails the call unless it runs
// to the end.
func runAgent(ctx context.Context, socket, script string) (string, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > 2*time.Minute {
		deadline = time.Now().Add(2 * time.Minute)
	}
	_ = conn.SetDeadline(deadline)

	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", agentPort); err != nil {
		return "", err
	}
	r := bufio.NewReader(conn)
	line, err := r.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("vsock: %w", err)
	}
	if !strings.HasPrefix(line, "OK ") {
		return "", fmt.Errorf("vsock: %q", strings.TrimSpace(line))
	}
	if _, err := io.WriteString(conn, script+"\necho "+agentDone+"\n"); err != nil {
		return "", err
	}
	if uc, ok := conn.(*net.UnixConn); ok {
		_ = uc.CloseWrite()
	}
	out, err := io.ReadAll(r)
	if err != nil {
		return string(out), err
	}
	if !strings.Contains(string(out), agentDone) {
		return string(out), fmt.Errorf("agent script failed: %s", lastLines(string(out), 5))
	}
	return strings.Replace(string(out), agentDone+"\n", "", 1), nil
}

// chAPI calls cloud-hypervisor's HTTP API on its unix socket.
func chAPI(ctx context.Context, socket, action string, body any) error {
	c := http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://localhost/api/v1/"+action, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: %s: %s", action, resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

// waitGone waits for an instance's cleanup to finish.
func (i *Inventory) waitGone(name string, timeout time.Duration) {
	end := time.Now().Add(timeout)
	for time.Now().Before(end) {
		i.lock.RLock()
		_, ok := i.instances[name]
		i.lock.RUnlock()
		if !ok {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}
