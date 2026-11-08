package fleetingd

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// maxSocketPath is the longest path a unix socket can have: sun_path holds
// 108 bytes, the NUL included.
const maxSocketPath = 107

// socketDir holds Cloud Hypervisor's API and vsock sockets. They don't live
// next to the disks: a socket's path is capped at 107 bytes, which a long
// vm_disk_directory or vm_image_directory goes over. One per disk
// directory, whose lock is ours.
func (i *InstanceGroup) socketDir() string {
	h := sha256.Sum256([]byte(i.VMDiskDir))
	return filepath.Join(os.TempDir(), "fleetingd-"+hex.EncodeToString(h[:])[:12])
}

// socketPath is a socket in socketDir, or an error when even that is too
// long (a long TMPDIR or group name).
func (i *InstanceGroup) socketPath(name string) (string, error) {
	p := filepath.Join(i.socketDir(), name)
	if len(p) > maxSocketPath {
		return "", fmt.Errorf("socket path %s is %d bytes, over the %d a unix socket allows; set a shorter TMPDIR or name", p, len(p), maxSocketPath)
	}
	return p, nil
}

// templateSockets are the API and vsock sockets of the VM a snapshot is
// built from in dir.
func (i *InstanceGroup) templateSockets(dir string) (api, vsock string, err error) {
	h := sha256.Sum256([]byte(dir))
	base := "template-" + hex.EncodeToString(h[:])[:8]
	if api, err = i.socketPath(base + ".api"); err != nil {
		return "", "", err
	}
	vsock, err = i.socketPath(base + ".vsock")
	return api, vsock, err
}

// prepareSocketDir makes socketDir afresh, ours alone. It is in a shared
// directory: Mkdir, not MkdirAll, so a directory or link someone else put
// there is an error rather than used.
func (i *InstanceGroup) prepareSocketDir() error {
	dir := i.socketDir()
	// The longest names it will hold.
	if _, _, err := i.templateSockets(dir); err != nil {
		return err
	}
	if _, err := i.socketPath(i.instanceName(9999) + ".vsock"); err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return fmt.Errorf("socket directory: %w", err)
	}
	return nil
}
