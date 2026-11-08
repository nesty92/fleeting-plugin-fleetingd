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
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/unix"
)

const agentPort = 52

// snapshotKeyVersion changes when the way snapshots are made changes.
const snapshotKeyVersion = "3"
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

// snapshotSlot names what a group asks of its VMs: size, network and
// console. Groups with the same slot share a snapshot; a new key in the
// same slot replaces an older one.
func (i *InstanceGroup) snapshotSlot() string {
	h := sha256.New()
	fmt.Fprintf(h, "%d\n%d\n%d\n%t\n%t\n%s", i.VMNumCPUCores, i.VMMemoryMegabytes, i.VMDiskSizeGB,
		i.packageUpgrade(), i.VMEnableVirtioConsole, i.VMSubnet)
	return hex.EncodeToString(h.Sum(nil))[:8]
}

// snapshotKey names how a slot's snapshot is made: the hypervisor, the
// template's user data and this code's version.
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
	fmt.Fprintf(h, "%s\n%s\n%s", strings.TrimSpace(string(out)), tpl, snapshotKeyVersion)
	return hex.EncodeToString(h.Sum(nil))[:8], nil
}

// snapshotPrefix is the start of every snapshot name of this group's
// image and slot: snapshot-<image>-<slot>-<key>.
func (i *InstanceGroup) snapshotPrefix() string {
	image := strings.TrimPrefix(strings.TrimSuffix(filepath.Base(i.imagePath), ".qcow2"), "prebuilt-")
	return filepath.Join(i.imageDir(), "snapshot-"+image+"-"+i.snapshotSlot()+"-")
}

func (i *InstanceGroup) ensureSnapshot() error {
	key, err := i.snapshotKey()
	if err != nil {
		return err
	}
	dir := i.snapshotPrefix() + key

	lock, err := os.OpenFile(dir+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)

	s, err := loadSnapshot(dir, i.imagePath)
	switch {
	case err == nil:
		i.logger.Info("reusing the snapshot", "snapshot", filepath.Base(dir))
	case errors.Is(err, os.ErrNotExist):
		// None yet.
	default:
		i.logger.Warn("rebuilding the snapshot", "snapshot", filepath.Base(dir), "why", err)
	}
	if err != nil {
		if s, err = i.buildSnapshot(dir); err != nil {
			return err
		}
	}
	// Held for the group's lifetime, as the image is: a snapshot no group
	// holds may be removed.
	hold, err := os.Open(filepath.Join(dir, "meta.json"))
	if err != nil {
		return err
	}
	if err := unix.Flock(int(hold.Fd()), unix.LOCK_SH); err != nil {
		hold.Close()
		return err
	}
	if i.snapshotHold != nil {
		i.snapshotHold.Close()
	}
	i.snapshotHold, i.snapshot = hold, s
	i.removeUnusedSnapshots(dir)
	return nil
}

// removeUnusedSnapshots deletes older snapshots of this group's image and
// slot that no group holds or is building, such as those of an older
// plugin or cloud-hypervisor. Other slots' snapshots belong to other
// groups, which may not have started yet; snapshots of other images go
// with their image.
func (i *InstanceGroup) removeUnusedSnapshots(keep string) {
	dirs, _ := filepath.Glob(i.snapshotPrefix() + "*")
	for _, dir := range dirs {
		if dir == keep || strings.HasSuffix(dir, ".lock") || strings.HasSuffix(dir, ".tmp") {
			continue
		}
		if removeSnapshotIfUnused(dir) {
			i.logger.Info("removed an unused snapshot", "snapshot", filepath.Base(dir))
		}
	}
}

func removeSnapshotIfUnused(dir string) bool {
	lock, err := os.OpenFile(dir+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false
	}
	defer lock.Close()
	if unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil {
		return false
	}
	if meta, err := os.Open(filepath.Join(dir, "meta.json")); err == nil {
		defer meta.Close()
		if unix.Flock(int(meta.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil {
			return false
		}
	}
	if os.RemoveAll(dir) != nil {
		return false
	}
	_ = os.Remove(dir + ".lock")
	return true
}

// loadSnapshot returns the snapshot in dir if it can be used with image.
// Its config and disk hold absolute paths, so one whose directory moved
// (an image directory moved or mounted elsewhere) or whose disk is no
// longer an overlay on image is refused, and rebuilt.
func loadSnapshot(dir, image string) (*snapshot, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return nil, err
	}
	var s snapshot
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("snapshot %s: %w", dir, err)
	}
	for _, f := range []string{"snap/config.json", "snap/state.json", "disk.qcow2"} {
		if ok, _ := checkFileExists(filepath.Join(dir, f)); !ok {
			return nil, fmt.Errorf("snapshot %s lacks %s", dir, f)
		}
	}
	if s.BuiltIn != dir+".tmp" {
		return nil, fmt.Errorf("snapshot %s was built in %s, elsewhere", dir, s.BuiltIn)
	}
	s.Dir, s.Disk = dir, filepath.Join(dir, "disk.qcow2")
	backing, err := qcow2Backing(s.Disk)
	if err != nil {
		return nil, err
	}
	if backing != image {
		return nil, fmt.Errorf("snapshot %s: its disk is an overlay on %q, not on %s", dir, backing, image)
	}
	return &s, nil
}

// loginPrep makes the template's SSH logins fast: its first one took
// 650 ms, 470 of them update-motd.d's scripts and the rest starting
// user@1000.service.
const loginPrep = `set -e
chmod -x /etc/update-motd.d/* 2>/dev/null || true
loginctl enable-linger ubuntu
uid=$(id -u ubuntu)
for n in $(seq 100); do
  systemctl is-active -q "user@$uid.service" && break
  sleep 0.1
done
systemctl is-active -q "user@$uid.service"`

func (i *InstanceGroup) buildSnapshot(dir string) (*snapshot, error) {
	tmp := dir + ".tmp"
	// A build that died with its plugin; the snapshot lock is ours.
	killLeftovers(tmp)
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
		i.inventory.waitGone(tpl.ID, 30*time.Second)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	api, vsock, err := i.templateSockets(tmp)
	if err != nil {
		stop()
		return nil, err
	}
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
	// What a VM's first SSH login would start, started once here: the
	// ubuntu user's systemd instance, which the snapshot then holds, and no
	// motd scripts. A published image may not have them off already.
	if _, err := runAgent(ctx, vsock, loginPrep); err != nil {
		i.logger.Warn("SSH logins to restored VMs may be slow", "err", err)
	}
	i.logger.Info("snapshot template ready", "after", time.Since(start).Round(time.Millisecond).String())

	// Less for the snapshot to hold: free page cache, and give the host
	// back what the guest freed (free page reporting) before pausing.
	if _, err := runAgent(ctx, vsock, "sync; echo 3 > /proc/sys/vm/drop_caches; sleep 2"); err != nil {
		i.logger.Warn("could not drop the template's caches", "err", err)
	}
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
	if used, err := punchZeros(filepath.Join(tmp, "snap", "memory-ranges")); err != nil {
		i.logger.Warn("could not make the snapshot's memory sparse", "err", err)
	} else {
		i.logger.Info("snapshot memory", "on_disk_mb", used>>20, "guest_mb", i.VMMemoryMegabytes)
	}
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
	// Cloud Hypervisor locks its API socket with a file beside it.
	for _, f := range []string{api, api + ".lock", vsock} {
		_ = os.Remove(f)
	}
	_ = os.RemoveAll(dir)
	if err := os.Rename(tmp, dir); err != nil {
		return nil, err
	}
	i.logger.Info("snapshot built", "snapshot", filepath.Base(dir), "took", time.Since(start).Round(time.Millisecond).String())
	return loadSnapshot(dir, i.imagePath)
}

// prepareRestore writes this VM's copy of the snapshot: its own disk, tap,
// host address, vsock socket and console. The memory file is shared.
func (i *InstanceGroup) prepareRestore(name, hostIP, disk, vsock string) (string, error) {
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
		Vsock:        vsock,
		Console:      consoleFile(i, name),
		Landlock:     []string{i.imageDir()},
		Writable:     []string{work, i.socketDir()},
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
	Writable                                        []string
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
	var rules []any
	for _, p := range r.Writable {
		rules = append(rules, map[string]any{"path": p, "access": "rw"})
	}
	for _, p := range r.Landlock {
		rules = append(rules, map[string]any{"path": p, "access": "r"})
	}
	cfg["landlock_enable"], cfg["landlock_rules"] = true, rules
	return nil
}

// identityScript gives a restored VM its own address, key and identity,
// and the host's time: a restored guest's clock still reads the moment the
// snapshot was taken, and tokens such as a GitHub runner's are refused
// from a clock that far off. It prints the guest's clock before the fix.
func identityScript(name, ip, gateway string, key ed25519.PublicKey, hostKey ed25519.PrivateKey, now time.Time) (string, error) {
	pub, err := ssh.NewPublicKey(key)
	if err != nil {
		return "", err
	}
	// The VM's SSH host key is made here rather than by ssh-keygen in the
	// VM, which costs some 80 ms a restore.
	hostPub, err := ssh.NewPublicKey(hostKey.Public())
	if err != nil {
		return "", err
	}
	block, err := ssh.MarshalPrivateKey(hostKey, "")
	if err != nil {
		return "", err
	}
	// Few processes: right after a restore each one is slow to start, its
	// memory read in from the snapshot as it is touched.
	return fmt.Sprintf(`set -e
echo clock_was=$(date +%%s.%%N)
date -s @%[5]d.%09[6]d >/dev/null
ip -batch - <<IP
addr flush dev veth0
addr add %[2]s/30 dev veth0
route replace default via %[3]s
IP
sed -i -E 's|- [0-9.]+/30|- %[2]s/30|; s|via: [0-9.]+|via: %[3]s|' /etc/netplan/50-cloud-init.yaml 2>/dev/null || true
install -d -m 700 -o ubuntu -g ubuntu /home/ubuntu/.ssh
install -m 600 -o ubuntu -g ubuntu /dev/null /home/ubuntu/.ssh/authorized_keys
echo '%[4]s' > /home/ubuntu/.ssh/authorized_keys
rm -f /etc/ssh/ssh_host_*
(umask 077; printf '%%s' '%[7]s' > /etc/ssh/ssh_host_ed25519_key)
echo '%[8]s root@%[1]s' > /etc/ssh/ssh_host_ed25519_key.pub
# A running sshd reloads its keys; one that socket activation hasn't
# started yet reads them when it starts.
if pgrep -x sshd >/dev/null; then systemctl reload ssh || systemctl restart ssh; fi
od -An -tx1 -N16 /dev/urandom | tr -d ' \n' > /etc/machine-id
head -c 512 /dev/urandom > /dev/urandom
chronyc -a makestep >/dev/null 2>&1 || true
echo %[1]s > /etc/hostname
hostname %[1]s
`, name, ip, gateway, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))), now.Unix(), now.Nanosecond(),
		string(pem.EncodeToMemory(block)), strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hostPub)))), nil
}

// clockDrift reads how far behind the host a guest's clock was from the
// identity script's output.
func clockDrift(out string, at time.Time) (time.Duration, bool) {
	for _, line := range strings.Split(out, "\n") {
		v, ok := strings.CutPrefix(strings.TrimSpace(line), "clock_was=")
		if !ok {
			continue
		}
		secs, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 0, false
		}
		was := time.Unix(0, int64(secs*float64(time.Second)))
		return at.Sub(was).Round(time.Millisecond), true
	}
	return 0, false
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
func (i *Inventory) waitGone(id string, timeout time.Duration) {
	end := time.Now().Add(timeout)
	for time.Now().Before(end) {
		i.lock.RLock()
		_, ok := i.instances[id]
		i.lock.RUnlock()
		if !ok {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}
