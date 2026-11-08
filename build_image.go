package fleetingd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/hashicorp/go-hclog"
)

// BuildImage runs the prebuild the plugin_config in settings describes,
// without a runner, and writes the image to out for vm_image_url:
// <name>-<arch>.qcow2 (compressed), <name>-<arch>.vmlinuz and a SHA256SUMS
// listing both. It needs what the plugin needs: cloud-hypervisor, nft,
// qemu-img, /dev/kvm and the right to make tap devices.
func BuildImage(settings []byte, out, name string, logger hclog.Logger) error {
	var i InstanceGroup
	dec := json.NewDecoder(bytes.NewReader(settings))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&i); err != nil {
		return fmt.Errorf("plugin_config: %w", err)
	}
	if i.VMImageURL != "" {
		return fmt.Errorf("plugin_config: vm_image_url uses a published image; building one needs the commands instead")
	}
	if err := i.validate(); err != nil {
		return err
	}
	// The console is where a failed command is reported.
	i.VMEnableVirtioConsole = true
	i.strictPrebuild = true
	if err := checkTools(); err != nil {
		return err
	}
	i.logger = logger.Named(i.Name)
	i.inventory = NewInventory()
	if err := os.MkdirAll(i.VMDiskDir, 0o700); err != nil {
		return err
	}
	if err := i.lockDiskDir(); err != nil {
		return err
	}
	defer i.lockFile.Close()
	if err := i.prepareWorkdir(); err != nil {
		return err
	}
	if err := i.ensureImage(); err != nil {
		return err
	}
	defer i.imageHold.Close()

	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	base := fmt.Sprintf("%s-%s", name, runtime.GOARCH)
	image, kernel := base+".qcow2", base+".vmlinuz"
	i.logger.Info("compressing the image", "out", filepath.Join(out, image))
	if b, err := exec.Command("qemu-img", "convert", "-c", "-f", "qcow2", "-O", "qcow2", i.imagePath, filepath.Join(out, image)).CombinedOutput(); err != nil {
		return fmt.Errorf("compressing the image: %w: %s", err, b)
	}
	if b, err := exec.Command("cp", "-f", i.kernelPath, filepath.Join(out, kernel)).CombinedOutput(); err != nil {
		return fmt.Errorf("copying the kernel: %w: %s", err, b)
	}
	var sums bytes.Buffer
	for _, f := range []string{image, kernel} {
		sum, err := computeFileSHA256(filepath.Join(out, f))
		if err != nil {
			return err
		}
		fmt.Fprintf(&sums, "%s *%s\n", sum, f)
	}
	return os.WriteFile(filepath.Join(out, "SHA256SUMS"), sums.Bytes(), 0o644)
}
