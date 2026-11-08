package fleetingd

// One prebuilt image per host and input, shared by every group.
//
// vm_image_directory holds Ubuntu's downloads and the prebuilt images, each
// named by a hash of what went into it: the cloud image, the kernel, the
// prebuild template and vm_prebuild_cloudinit_extra_cmds. The image's own
// copy of the kernel sits next to it, so a VM always boots the kernel whose
// modules are in its image.
//
// Builds take an exclusive lock on the directory: the first group builds,
// the others wait and reuse the result. A group holds a shared lock on the
// image it uses for as long as it runs, so a build that finds an older
// image without holders deletes it, and never one still in use.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// prebuildDiskGB is the prebuilt image's size. Each VM's overlay grows it
// to vm_disk_size_gb.
const prebuildDiskGB = 16

// imageKeyVersion changes when the way images are built changes.
const imageKeyVersion = "1"

func (i *InstanceGroup) imageDir() string {
	if i.VMImageDir != "" {
		return i.VMImageDir
	}
	return i.VMDiskDir
}

// bootKernel is the kernel that matches the image in use.
func (i *InstanceGroup) bootKernel() (string, error) {
	if i.kernelPath == "" {
		return "", errors.New("no prebuilt image")
	}
	return i.kernelPath, nil
}

// imageKey names the prebuilt image for these inputs.
func (i *InstanceGroup) imageKey(diskSum, kernelSum string) (string, error) {
	tpl, err := userDataTemplates.ReadFile("templates/user-data-prebuild.tpl")
	if err != nil {
		return "", err
	}
	h := sha256.New()
	for _, part := range []string{imageKeyVersion, diskSum, kernelSum, string(tpl), fmt.Sprint(prebuildDiskGB)} {
		fmt.Fprintf(h, "%d:%s\n", len(part), part)
	}
	for _, c := range i.VMPrebuildCloudinitExtraCmds {
		fmt.Fprintf(h, "cmd %d:%s\n", len(c), c)
	}
	if i.VMPrebuildSkipGitlabRunner {
		fmt.Fprintln(h, "no gitlab-runner")
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

// ensureImage makes sure the shared image for this group's inputs exists,
// building it if needed, and holds it for the group's lifetime.
func (i *InstanceGroup) ensureImage() error {
	if err := os.MkdirAll(i.imageDir(), 0o700); err != nil {
		return err
	}
	build, err := os.OpenFile(filepath.Join(i.imageDir(), ".build.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer build.Close()
	i.logger.Info("waiting for the image directory", "dir", i.imageDir())
	if err := unix.Flock(int(build.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	defer unix.Flock(int(build.Fd()), unix.LOCK_UN)

	if i.VMImageURL != "" {
		image, imageKernel, err := i.ensurePublishedImage()
		if err != nil {
			return err
		}
		return i.useImage(image, imageKernel)
	}
	if err := i.ensureImages(); err != nil {
		return err
	}
	diskName, err := getFilenameFromURL(diskImageURL)
	if err != nil {
		return err
	}
	disk := filepath.Join(i.imageDir(), diskName)
	kernel, err := i.getKernelFilePath()
	if err != nil {
		return err
	}
	diskSum, err := computeFileSHA256(disk)
	if err != nil {
		return err
	}
	kernelSum, err := computeFileSHA256(kernel)
	if err != nil {
		return err
	}
	key, err := i.imageKey(diskSum, kernelSum)
	if err != nil {
		return err
	}
	image := filepath.Join(i.imageDir(), "prebuilt-"+key+".qcow2")
	imageKernel := filepath.Join(i.imageDir(), "prebuilt-"+key+".vmlinuz")

	if ok, _ := checkFileExists(image); ok {
		i.logger.Info("reusing the prebuilt image", "image", filepath.Base(image))
	} else {
		if err := i.buildImage(disk, kernel, image, imageKernel); err != nil {
			return err
		}
	}
	return i.useImage(image, imageKernel)
}

// useImage holds image for the group's lifetime and removes the images no
// group holds. Called with the directory's build lock held.
func (i *InstanceGroup) useImage(image, imageKernel string) error {
	hold, err := os.Open(image)
	if err != nil {
		return err
	}
	if err := unix.Flock(int(hold.Fd()), unix.LOCK_SH); err != nil {
		hold.Close()
		return err
	}
	if i.imageHold != nil {
		i.imageHold.Close()
	}
	i.imageHold = hold
	i.imagePath, i.kernelPath = image, imageKernel

	i.removeUnusedImages(image)
	return nil
}

// buildImage runs the prebuild on a fresh copy of the cloud image and
// moves the result into place only once it succeeded.
func (i *InstanceGroup) buildImage(disk, kernel, image, imageKernel string) error {
	tmp, tmpKernel := image+".tmp", imageKernel+".tmp"
	// A build that died with its plugin; the build lock is ours.
	killLeftovers(tmp)
	defer os.Remove(tmp)
	defer os.Remove(tmpKernel)

	i.logger.Info("building the image", "image", filepath.Base(image))
	// cloud-hypervisor can't read compressed qcow2.
	if out, err := exec.Command("qemu-img", "convert", "-f", "qcow2", "-O", "qcow2", disk, tmp).CombinedOutput(); err != nil {
		return fmt.Errorf("decompressing the cloud image: %w: %s", err, out)
	}
	if out, err := exec.Command("qemu-img", "resize", "-q", tmp, fmt.Sprintf("%dG", prebuildDiskGB)).CombinedOutput(); err != nil {
		return fmt.Errorf("resizing the image: %w: %s", err, out)
	}
	if out, err := exec.Command("cp", "-f", kernel, tmpKernel).CombinedOutput(); err != nil {
		return fmt.Errorf("copying the kernel: %w: %s", err, out)
	}

	i.imagePath, i.kernelPath = tmp, tmpKernel
	if err := i.inventory.PrebuildInstance(i); err != nil {
		i.imagePath, i.kernelPath = "", ""
		return err
	}
	if err := os.Rename(tmpKernel, imageKernel); err != nil {
		return err
	}
	if err := os.Rename(tmp, image); err != nil {
		return err
	}
	i.logger.Info("Prebuild finished.", "image", filepath.Base(image))
	return nil
}

// removeUnusedImages deletes other prebuilt images no group holds.
// Called with the directory's build lock held.
func (i *InstanceGroup) removeUnusedImages(keep string) {
	old, _ := filepath.Glob(filepath.Join(i.imageDir(), "prebuilt-*.qcow2"))
	for _, image := range old {
		if image == keep {
			continue
		}
		f, err := os.Open(image)
		if err != nil {
			continue
		}
		if unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) == nil {
			_ = os.Remove(image)
			base := strings.TrimSuffix(image, ".qcow2")
			_ = os.Remove(base + ".vmlinuz")
			// Its snapshots: no VM uses them once no group holds the image.
			snaps, _ := filepath.Glob(filepath.Join(i.imageDir(), "snapshot-"+strings.TrimPrefix(filepath.Base(base), "prebuilt-")+"-*"))
			for _, s := range snaps {
				if !strings.HasSuffix(s, ".lock") {
					removeSnapshotIfUnused(s)
				}
				_ = os.Remove(s)
			}
			i.logger.Info("removed an unused image", "image", filepath.Base(image))
		}
		f.Close()
	}
}
