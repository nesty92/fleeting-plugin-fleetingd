package fleetingd

// A plugin that dies without Shutdown (killed, crashed, the runner gone)
// would leave its VMs running: their processes, taps and addresses, which
// the next start then trips over. cloud-hypervisor gets SIGKILL when the
// plugin dies, and a start kills whatever a previous run still left.

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// dieWithPlugin makes a VM process end when the plugin does.
func dieWithPlugin() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}

// killLeftovers kills cloud-hypervisor processes whose command line names
// dir or a path under it, and waits for them to go. Only for paths no live
// process of ours can be using: callers hold the lock that guards dir.
func killLeftovers(dir string) []int {
	var killed []int
	procs, _ := filepath.Glob("/proc/[0-9]*/cmdline")
	prefix := []byte(filepath.Clean(dir) + "/")
	self := os.Getpid()
	for _, p := range procs {
		pid, err := strconv.Atoi(filepath.Base(filepath.Dir(p)))
		if err != nil || pid == self {
			continue
		}
		cmd, err := os.ReadFile(p)
		if err != nil || !isLeftover(cmd, prefix) {
			continue
		}
		if syscall.Kill(pid, syscall.SIGKILL) == nil {
			killed = append(killed, pid)
		}
	}
	// Taps and sockets go once the processes are gone.
	end := time.Now().Add(5 * time.Second)
	for _, pid := range killed {
		for time.Now().Before(end) {
			if !alive(pid) {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	return killed
}

// isLeftover is whether a /proc cmdline is cloud-hypervisor using a path
// under prefix.
func isLeftover(cmdline, prefix []byte) bool {
	args := bytes.Split(bytes.TrimRight(cmdline, "\x00"), []byte{0})
	if len(args) == 0 || filepath.Base(string(args[0])) != "cloud-hypervisor" {
		return false
	}
	for _, a := range args[1:] {
		for _, f := range bytes.FieldsFunc(a, func(r rune) bool { return r == ',' || r == '=' }) {
			f = bytes.TrimPrefix(f, []byte("file://"))
			if bytes.HasPrefix(f, prefix) || bytes.Equal(f, prefix[:len(prefix)-1]) {
				return true
			}
		}
	}
	return false
}

// alive is whether a process still runs; a zombie has already let go of
// its taps and files.
func alive(pid int) bool {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	// pid (comm) state ...: comm may hold spaces and parentheses.
	end := bytes.LastIndexByte(stat, ')')
	return end < 0 || end+2 >= len(stat) || stat[end+2] != 'Z'
}
