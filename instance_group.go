package fleetingd

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/go-hclog"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
	"golang.org/x/sys/unix"
)

// Currently the number of VM slots is limited by the number of /30s, this is fairly arbitrary but should be plenty for now
const VMPrefix = "172.16.120."
const MaxIPAMSlots = 255 / 4

type InstanceGroup struct {
	// Name tells groups on one host apart: it is the group's ID and prefixes
	// its VMs, tap devices and nftables tables. Default "fleetingd". Each
	// group needs its own name, vm_subnet and vm_disk_directory.
	Name            string `json:"name"`
	EgressInterface string `json:"egress_interface"`
	VMDiskDir       string `json:"vm_disk_directory"`
	// VMImageDir holds the downloads and prebuilt images, shared by every
	// group on the host. Default vm_disk_directory.
	VMImageDir                   string   `json:"vm_image_directory"`
	VMSubnet                     string   `json:"vm_subnet"`
	VMNumCPUCores                uint64   `json:"vm_num_cpu_cores"`
	VMMemoryMegabytes            uint64   `json:"vm_memory_mb"`
	VMDiskSizeGB                 uint64   `json:"vm_disk_size_gb"`
	VMPrebuildCloudinitExtraCmds []string `json:"vm_prebuild_cloudinit_extra_cmds"`
	VMEnableVirtioConsole        bool     `json:"vm_enable_virtio_console"`
	// VMFullDiskCopy copies the whole prebuilt image for each VM instead of
	// giving it a qcow2 overlay. Only for cloud-hypervisor without
	// backing_files support.
	VMFullDiskCopy bool `json:"vm_full_disk_copy"`
	// VMPackageUpgrade runs apt update and upgrade in every VM on boot.
	// Default true. false saves 3 s per VM, or more when Ubuntu has
	// published updates since the prebuild; those then wait for the next
	// prebuild (a runner restart).
	VMPackageUpgrade *bool `json:"vm_package_upgrade"`
	// VMPrebuildOnStart builds the image when the plugin starts instead of
	// on the first job, after prebuildGrace so that short-lived starts
	// (fleetci check, config probes) end before it begins.
	VMPrebuildOnStart bool `json:"vm_prebuild_on_start"`
	// VMSnapshot restores VMs from a snapshot of a booted one instead of
	// booting them: ready in under a second instead of 5-7. The snapshot is
	// taken once per image and VM size, after the prebuild. See snapshot.go.
	VMSnapshot bool `json:"vm_snapshot"`
	// VMImageURL is a published prebuilt image to use instead of building
	// one: the URL of a SHA256SUMS listing its .qcow2 and .vmlinuz. {arch}
	// in it stands for the host's (amd64, arm64). See published_image.go.
	VMImageURL string `json:"vm_image_url"`

	logger    hclog.Logger
	inventory *Inventory
	lockFile  *os.File
	// The prebuilt image in use, its kernel, and the shared lock that keeps
	// other builds from deleting it.
	imagePath  string
	kernelPath string
	imageHold  *os.File
	// snapshot is the snapshot VMs are restored from, with vm_snapshot.
	snapshot     *snapshot
	snapshotHold *os.File
}

// prebuildGrace delays vm_prebuild_on_start past processes that only Init
// a plugin and exit, such as fleetci check.
var prebuildGrace = 10 * time.Second

// Tap device names are at most 15 bytes: the name plus a two-digit index.
var validName = regexp.MustCompile(`^[a-z][a-z0-9]{0,9}$`)

// validate fills in defaults and checks the group's own settings.
func (i *InstanceGroup) validate() error {
	if i.Name == "" {
		i.Name = "fleetingd"
	}
	if !validName.MatchString(i.Name) {
		return fmt.Errorf("name %q: want 1-10 lowercase letters and digits, starting with a letter", i.Name)
	}
	if i.VMDiskSizeGB == 0 {
		i.VMDiskSizeGB = 30
	}
	if i.VMDiskSizeGB < prebuildDiskGB {
		return fmt.Errorf("vm_disk_size_gb %d: at least %d, the prebuilt image's size", i.VMDiskSizeGB, prebuildDiskGB)
	}
	if i.VMImageURL != "" {
		u, err := url.Parse(i.VMImageURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return fmt.Errorf("vm_image_url %q: want an http(s) URL of a SHA256SUMS file", i.VMImageURL)
		}
		if len(i.VMPrebuildCloudinitExtraCmds) > 0 {
			return fmt.Errorf("vm_image_url and vm_prebuild_cloudinit_extra_cmds: a published image is used as it is, so set only one")
		}
	}
	if i.VMSubnet == "" {
		i.VMSubnet = VMPrefix
	}
	if ip := net.ParseIP(i.VMSubnet + "0").To4(); ip == nil || !strings.HasSuffix(i.VMSubnet, ".") || strings.Count(i.VMSubnet, ".") != 3 {
		return fmt.Errorf("vm_subnet %q: want the first three octets of a /24 and a dot, like %q", i.VMSubnet, VMPrefix)
	}
	return nil
}

// lockDiskDir keeps a second group off this vm_disk_directory: each group
// clears its working directory and rebuilds its image on start.
func (i *InstanceGroup) lockDiskDir() error {
	f, err := os.OpenFile(filepath.Join(i.VMDiskDir, ".fleetingd.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return fmt.Errorf("vm_disk_directory %s is in use by another fleetingd group; give each group its own", i.VMDiskDir)
	}
	i.lockFile = f
	return nil
}

func (i *InstanceGroup) instanceName(index int) string {
	return i.Name + strconv.Itoa(index)
}

func (i *InstanceGroup) Init(ctx context.Context, logger hclog.Logger, settings provider.Settings) (provider.ProviderInfo, error) {
	//
	// Preflight checks
	//

	if err := i.validate(); err != nil {
		return provider.ProviderInfo{}, err
	}
	i.logger = logger.Named(i.Name)

	i.inventory = NewInventory()

	if err := checkTools(); err != nil {
		return provider.ProviderInfo{}, err
	}

	// Check disk dir is writable
	err := unix.Access(i.VMDiskDir, unix.W_OK)
	if err != nil {
		return provider.ProviderInfo{}, fmt.Errorf("'%s' was specified as vm_disk_directory in the settings but is not writable: %w", i.VMDiskDir, err)
	}
	if err := i.lockDiskDir(); err != nil {
		return provider.ProviderInfo{}, err
	}

	if i.VMPrebuildOnStart {
		go func() {
			time.Sleep(prebuildGrace)
			i.logger.Info("building the image before the first VM is asked for")
			_ = i.inventory.EnsurePrebuild(i)
		}()
	}

	return provider.ProviderInfo{
		ID:        i.Name,
		MaxSize:   MaxIPAMSlots,
		Version:   Version.Version,
		BuildInfo: "TBD",
	}, nil
}

func (i *InstanceGroup) Update(ctx context.Context, updateFunc func(instance string, state provider.State)) error {
	// Query status from inventory
	instances := i.inventory.GetAllInstances()

	for _, instance := range instances {
		err := i.Heartbeat(ctx, instance)
		if err != nil {
			i.logger.Info("creating...", "instance", instance)
			updateFunc(instance, provider.StateCreating)
			continue
		}

		updateFunc(instance, provider.StateRunning)
	}

	return nil
}

func (i *InstanceGroup) Increase(ctx context.Context, n int) (succeeded int, err error) {
	// Try to boot more instances

	for counter := 0; counter < n; counter++ {
		err := i.inventory.BootInstance(i, nil)
		if err != nil {
			i.logger.Error("instance boot error", "error", err)
			return counter, err
		}
	}

	return n, nil
}

func (i *InstanceGroup) Decrease(ctx context.Context, instances []string) ([]string, error) {
	// Try to remove instances
	removedInstances := []string{}

	for _, instanceToRemove := range instances {
		i.logger.Info("stopping instance", "instance", instanceToRemove)

		err := i.inventory.DestroyInstance(instanceToRemove)
		if err != nil {
			i.logger.Error("error stopping instance: %w", err)
			continue
		}

		i.logger.Info("stopped instance", "instance", instanceToRemove)

		removedInstances = append(removedInstances, instanceToRemove)
	}

	return removedInstances, nil
}

func (i *InstanceGroup) ConnectInfo(ctx context.Context, instance string) (provider.ConnectInfo, error) {
	// Return connection information from the inventory

	info, err := i.inventory.GetConnectInfo(instance)
	if err != nil {
		return provider.ConnectInfo{}, err
	}

	return *info, err
}

func (i *InstanceGroup) Heartbeat(ctx context.Context, instance string) error {
	// Check SSH connection
	info, err := i.inventory.GetConnectInfo(instance)
	if err != nil {
		return err
	}

	// Check SSH port is reachable
	hostPort := net.JoinHostPort(info.InternalAddr, strconv.Itoa(info.ProtocolPort))
	connection, err := net.DialTimeout("tcp", hostPort, time.Second)
	if err != nil {
		return err
	}
	err = connection.Close()
	if err != nil {
		return err
	}

	return nil
}

func (i *InstanceGroup) Suspend(ctx context.Context, instances []string) (succeeded []string, err error) {
	// This is a no-op as we do not support suspend/resume
	i.logger.Warn("Runner attempted to suspend an instance. This is not supported on this plugin.")

	return []string{}, nil
}

func (i *InstanceGroup) Resume(ctx context.Context, instances []string) (succeeded []string, err error) {
	// This is a no-op as we do not support suspend/resume
	i.logger.Warn("Runner attempted to resume an instance. This is not supported on this plugin.")

	return []string{}, nil
}

func (i *InstanceGroup) Shutdown(ctx context.Context) error {
	// Destroy all instances
	return i.inventory.DestroyAllInstances()
}

func (i *InstanceGroup) MakeAddress(index int) string {
	return i.VMSubnet + strconv.Itoa(index)
}

// checkTools makes sure the tools the plugin runs are installed.
func checkTools() error {
	for _, binary := range []string{"cloud-hypervisor", "nft", "qemu-img"} {
		if _, err := exec.LookPath(binary); err != nil {
			return fmt.Errorf("could not find required binary %s on PATH, please check the PATH variable or install the missing tool: %w", binary, err)
		}
	}
	return nil
}
