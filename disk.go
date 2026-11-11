package fleetingd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// disk: a disk added to a running VM, and taken out again, for a cache that
// outlives the VM. fleetci keeps its cache disks as qcow2 layers in
// vm_cache_directory; when it is given a VM for a job, it runs
//
//	fleeting-plugin-fleetingd disk -config <plugin_config> attach <instance> <layer>
//
// and the guest sees a new virtio disk, /dev/disk/by-id/virtio-<id>. After
// the job it unmounts it in the guest and runs "disk ... detach <instance>";
// only then is the layer whole. The VM is deleted as it always is.
//
// A VM restored from a snapshot takes a disk the same way: the snapshot is
// of a VM without one. Every VM's landlock rules let cloud-hypervisor open
// the files in vm_cache_directory, and nothing else it wasn't given at
// start: a layer and its backing files must all be in it.

// diskID names the disk in cloud-hypervisor and, as its serial, in the
// guest: /dev/disk/by-id/virtio-cache.
const diskID = "cache"

// instanceAPISocket is the API socket of a VM fleetci may add a disk to.
func (i *InstanceGroup) instanceAPISocket(name string) (string, error) {
	return i.socketPath(name + ".api")
}

// instanceSocket finds the API socket of a fleeting instance ID, which is
// its VM's name, a dash and a suffix (newID).
func (i *InstanceGroup) instanceSocket(instance string) (string, error) {
	name, _, _ := strings.Cut(instance, "-")
	index, ok := strings.CutPrefix(name, i.Name)
	if !ok || index == "" || strings.Trim(index, "0123456789") != "" {
		return "", fmt.Errorf("instance %q is not one of group %s's", instance, i.Name)
	}
	p, err := i.instanceAPISocket(name)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("instance %s has no API socket; is it running, and was vm_cache_directory set when it started? %w", instance, err)
	}
	return p, nil
}

// cacheLayer checks that layer is a file in vm_cache_directory, as landlock
// will, but with a clearer error.
func (i *InstanceGroup) cacheLayer(layer string) (string, error) {
	if !filepath.IsAbs(layer) {
		return "", fmt.Errorf("layer %q: want an absolute path", layer)
	}
	dir, err := filepath.EvalSymlinks(i.VMCacheDir)
	if err != nil {
		return "", fmt.Errorf("vm_cache_directory: %w", err)
	}
	p, err := filepath.EvalSymlinks(layer)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(dir, p)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("layer %s is not in vm_cache_directory %s", layer, i.VMCacheDir)
	}
	if st, err := os.Stat(p); err != nil {
		return "", err
	} else if !st.Mode().IsRegular() {
		return "", fmt.Errorf("layer %s is not a file", layer)
	}
	return p, nil
}

// AttachDisk adds the qcow2 file layer to a running instance.
func (i *InstanceGroup) AttachDisk(ctx context.Context, instance, layer string) error {
	sock, err := i.instanceSocket(instance)
	if err != nil {
		return err
	}
	p, err := i.cacheLayer(layer)
	if err != nil {
		return err
	}
	return chAPI(ctx, sock, "vm.add-disk", map[string]any{
		"path":          p,
		"id":            diskID,
		"serial":        diskID,
		"image_type":    "Qcow2",
		"backing_files": true,
	})
}

// DetachDisk takes the disk out of a running instance. The guest must have
// unmounted it: what it hasn't written back is lost.
func (i *InstanceGroup) DetachDisk(ctx context.Context, instance string) error {
	sock, err := i.instanceSocket(instance)
	if err != nil {
		return err
	}
	return chAPI(ctx, sock, "vm.remove-device", map[string]string{"id": diskID})
}

// Disk runs the disk subcommand: attach <instance> <layer>, or detach
// <instance>, for the group the plugin_config in settings describes.
func Disk(settings []byte, args []string) error {
	var i InstanceGroup
	dec := json.NewDecoder(bytes.NewReader(settings))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&i); err != nil {
		return fmt.Errorf("plugin_config: %w", err)
	}
	if err := i.validate(); err != nil {
		return err
	}
	if i.VMCacheDir == "" {
		return errors.New("plugin_config has no vm_cache_directory, so its VMs take no disks")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	switch {
	case len(args) == 3 && args[0] == "attach":
		return i.AttachDisk(ctx, args[1], args[2])
	case len(args) == 2 && args[0] == "detach":
		return i.DetachDisk(ctx, args[1])
	}
	return errors.New("want attach <instance> <layer>, or detach <instance>")
}
