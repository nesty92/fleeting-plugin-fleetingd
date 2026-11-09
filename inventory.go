package fleetingd

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"text/template"
	"time"

	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
	"golang.org/x/crypto/ssh"
)

type InstanceInfo struct {
	// ID is the instance's ID for fleeting, unique to this plugin run: a
	// restarted plugin reuses slots and Names while the runner may still
	// hold the old run's instances.
	ID string
	// Name is the VM's slot name, also its tap device's.
	Name                      string
	InstanceContextCancelFunc context.CancelFunc

	HostTapIP             string
	InstanceTapIP         string
	InstanceTapMacAddress string

	SSHPublicKey  ed25519.PublicKey
	SSHPrivateKey ed25519.PrivateKey

	// template is a snapshot's template VM, not one the runner asked for.
	template bool
	// pending is a restored VM still taking its own identity.
	pending bool
}

type Inventory struct {
	lock     *sync.RWMutex
	nftLock  sync.Mutex
	prebuild *sync.Once
	// prebuildErr is the prebuild's result, for every caller of
	// EnsurePrebuild and not only the one that ran it.
	prebuildErr error

	// Stop accepting requests when this is true
	shuttingDown bool

	// exited are the instances whose VM stopped on its own, by id, with how
	// many times Update has reported them since: see TakeExited.
	exited map[string]int

	// IPAM "tickets" / subnet tracking
	ipamSlots map[string]struct{}
	// Inventory, by ID
	instances map[string]*InstanceInfo
	// run and seq make instance IDs unique across plugin runs.
	run string
	seq int
}

// newID names an instance for fleeting. Called with the lock held.
func (i *Inventory) newID(name string) string {
	i.seq++
	return fmt.Sprintf("%s-%s%d", name, i.run, i.seq)
}

func NewInventory() *Inventory {
	run := make([]byte, 3)
	_, _ = rand.Read(run)
	return &Inventory{
		run:      hex.EncodeToString(run),
		lock:     &sync.RWMutex{},
		prebuild: &sync.Once{},

		ipamSlots: make(map[string]struct{}),
		instances: make(map[string]*InstanceInfo),
		exited:    make(map[string]int),
	}
}

func (i *Inventory) RunPrebuild(instanceGroup *InstanceGroup) error {
	//
	// Disk image preparation
	//

	instanceGroup.logger.Info("First VM requested. Preparing environment...")

	// Clear old instance images
	err := instanceGroup.prepareWorkdir()
	if err != nil {
		return err
	}

	// The shared image: built once per host and input, reused by every group
	if err := instanceGroup.ensureImage(); err != nil {
		return err
	}
	if instanceGroup.VMSnapshot {
		if err := instanceGroup.ensureSnapshot(); err != nil {
			// Booting still works, only slower.
			instanceGroup.logger.Error("no snapshot, VMs boot instead", "err", err)
		}
	}
	return nil
}

// EnsurePrebuild builds the image once and returns that build's result.
// Callers that arrive while it runs wait for it.
func (i *Inventory) EnsurePrebuild(instanceGroup *InstanceGroup) error {
	i.prebuild.Do(func() {
		i.prebuildErr = i.RunPrebuild(instanceGroup)
		if i.prebuildErr != nil {
			instanceGroup.logger.Error("Prebuild failed", "err", i.prebuildErr)
		}
	})
	return i.prebuildErr
}

// BootInstance starts a VM: booted from the image, or with vm_snapshot
// restored from the group's snapshot. opts may be nil.
func (i *Inventory) BootInstance(instanceGroup *InstanceGroup, opts *bootOptions) error {
	if !opts.template() {
		if err := i.EnsurePrebuild(instanceGroup); err != nil {
			return err
		}
	}
	snap := instanceGroup.snapshot
	if opts.template() {
		snap = nil
	}
	var err error

	i.lock.RLock()
	takenSlots := len(i.ipamSlots)
	i.lock.RUnlock()

	// Short-circuit function instead of walking address space
	if takenSlots >= MaxIPAMSlots {
		return errors.New("available VM address space exhausted")
	}

	i.lock.Lock()

	if i.shuttingDown {
		i.lock.Unlock()
		return errors.New("system is shutting down")
	}

	// Behold, the ultimate IPv4 subnet allocation algorithm
	subnetBase := 0
	stepSize := 4

	// Walk subnets until a free slot is found and allocate it
	for {
		if subnetBase >= 255-stepSize {
			i.lock.Unlock()
			return errors.New("available VM address space exhausted")
		}

		if _, ok := i.ipamSlots[instanceGroup.MakeAddress(subnetBase)+"/30"]; !ok {
			break
		}

		subnetBase += 4
	}

	slot := instanceGroup.MakeAddress(subnetBase) + "/30"
	i.ipamSlots[slot] = struct{}{}
	// fail gives back what this call took while still holding the lock.
	var files []string
	fail := func(err error) error {
		delete(i.ipamSlots, slot)
		i.lock.Unlock()
		for _, f := range files {
			_ = os.RemoveAll(f)
		}
		return err
	}

	// Generate SSH key
	pubKey, privKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		return fail(err)
	}

	instanceIndex := subnetBase / stepSize
	instanceName := instanceGroup.instanceName(instanceIndex)
	instanceID := i.newID(instanceName)
	workdir := filepath.Join(instanceGroup.VMDiskDir, vmWorkdir)

	// Generate random mac address
	randomBytes := make([]byte, 4)
	_, err = rand.Read(randomBytes)
	if err != nil {
		return fail(err)
	}
	randomPart := hex.EncodeToString(randomBytes)

	// slicing like this is okay since it is an ASCII string
	instanceMac := fmt.Sprintf(
		"de:51:%s:%s:%s:%s",
		randomPart[0:2],
		randomPart[2:4],
		randomPart[4:6],
		randomPart[6:])
	if snap != nil {
		// The restored guest keeps the template's network card.
		instanceMac = snap.MAC
	}

	hostTapIP := instanceGroup.MakeAddress(subnetBase + 1)
	instanceTapIP := instanceGroup.MakeAddress(subnetBase + 2)

	instanceContext, instanceCancelFunc := context.WithCancel(context.Background())
	var hypervisorCommand *exec.Cmd
	var vsockPath string

	if snap != nil {
		overlayPath, err := instanceGroup.makeDisk(filepath.Join(workdir, instanceName+".img"), snap.Disk)
		files = append(files, overlayPath)
		if err != nil {
			instanceCancelFunc()
			return fail(err)
		}
		vsockPath, err = instanceGroup.socketPath(instanceName + ".vsock")
		if err != nil {
			instanceCancelFunc()
			return fail(err)
		}
		files = append(files, vsockPath)
		snapDir, err := instanceGroup.prepareRestore(instanceName, hostTapIP, overlayPath, vsockPath)
		files = append(files, filepath.Join(workdir, instanceName+"_snap"))
		if err != nil {
			instanceCancelFunc()
			return fail(err)
		}
		hypervisorCommand = exec.CommandContext(instanceContext, "cloud-hypervisor",
			"--restore", fmt.Sprintf("source_url=file://%s,memory_restore_mode=copyonwrite,resume=true", snapDir))
	} else {
		var userdataPath, overlayPath string
		if opts.template() {
			userdataPath, err = instanceGroup.writeSeed(filepath.Join(opts.Template, "seed.img"), "user-data-snapshot.tpl",
				instanceName, instanceMac, instanceTapIP, hostTapIP, "/30", pubKey)
		} else {
			userdataPath, err = instanceGroup.createUserdata(instanceName, instanceMac, instanceTapIP, hostTapIP, "/30", pubKey)
		}
		if !opts.template() {
			files = append(files, userdataPath)
		}
		if err != nil {
			instanceCancelFunc()
			return fail(err)
		}

		if opts.template() {
			overlayPath, err = instanceGroup.makeDisk(filepath.Join(opts.Template, "disk.qcow2"), instanceGroup.imagePath)
		} else {
			overlayPath, err = instanceGroup.copyImage(instanceName)
			files = append(files, overlayPath)
		}
		if err != nil {
			instanceCancelFunc()
			return fail(err)
		}

		kernelFilePath, err := instanceGroup.bootKernel()
		if err != nil {
			instanceCancelFunc()
			return fail(err)
		}

		hypervisorCommand = exec.CommandContext(instanceContext, "cloud-hypervisor",
			"--kernel",
			kernelFilePath,
			"--disk",
			instanceGroup.diskArg(overlayPath),
			fmt.Sprintf("path=%s,readonly=on,image_type=raw", userdataPath),
			"--cpus",
			fmt.Sprintf("boot=%d", instanceGroup.VMNumCPUCores),
			"--memory",
			fmt.Sprintf("size=%dM", instanceGroup.VMMemoryMegabytes),
			"--net",
			fmt.Sprintf("tap=%s,mac=%s,ip=%s,mask=255.255.255.252", instanceName, instanceMac, hostTapIP),
			"--cmdline",
			"console=hvc0 root=/dev/vda1 rw",
			"--landlock",
		)
		if opts.template() {
			// Paused and saved over the API; the agent listens on vsock.
			var apiPath string
			apiPath, vsockPath, err = instanceGroup.templateSockets(opts.Template)
			if err != nil {
				instanceCancelFunc()
				return fail(err)
			}
			hypervisorCommand.Args = append(hypervisorCommand.Args,
				"--api-socket", "path="+apiPath,
				"--vsock", "cid=3,socket="+vsockPath,
				"--landlock-rules", fmt.Sprintf("path=%s,access=rw", opts.Template),
				"--landlock-rules", fmt.Sprintf("path=%s,access=rw", instanceGroup.socketDir()))
		}
		hypervisorCommand.Args = append(hypervisorCommand.Args, "--balloon", "size=0,free_page_reporting=on")

		if instanceGroup.VMEnableVirtioConsole {
			// Enable console
			consolePath := filepath.Join(workdir, fmt.Sprintf("%s_console", instanceName))

			hypervisorCommand.Args = append(hypervisorCommand.Args, "--console",
				fmt.Sprintf("file=%s", consolePath))
		} else if opts.template() {
			hypervisorCommand.Args = append(hypervisorCommand.Args, "--console", "off")
		}

		if !instanceGroup.VMFullDiskCopy {
			// landlock confines cloud-hypervisor to the paths it is given; the
			// overlay's backing image is read through the overlay.
			hypervisorCommand.Args = append(hypervisorCommand.Args,
				"--landlock-rules", fmt.Sprintf("path=%s,access=r", instanceGroup.imagePath))
		}
	}

	hypervisorCommand.SysProcAttr = dieWithPlugin()
	stderrPath := filepath.Join(workdir, instanceName+"_stderr")
	if f, ferr := os.Create(stderrPath); ferr == nil {
		hypervisorCommand.Stderr = f
		defer f.Close()
	}

	instanceGroup.logger.Info("starting instance VM", "instance", instanceName, "restore", snap != nil, "template", opts.template())
	started := time.Now()
	err = hypervisorCommand.Start()
	if err != nil {
		// Nothing started: give back the slot and the lock, drop the files.
		instanceCancelFunc()
		return fail(err)
	}

	go func() {
		//
		// VM cleanup - cancel VM context to trigger stopping the VM process and then calling this function
		//

		// Wait for VM to terminate (when context gets cancelled)
		werr := hypervisorCommand.Wait()
		onItsOwn := instanceContext.Err() == nil
		if werr != nil && onItsOwn {
			// Exited on its own, not stopped by us: say why.
			out, _ := os.ReadFile(stderrPath)
			instanceGroup.logger.Error("instance VM exited", "instance", instanceName, "err", werr, "stderr", lastLines(string(out), 5))
		}
		_ = os.Remove(stderrPath)

		instanceGroup.logger.Info("instance process finished. cleaning up.", "instance", instanceName)

		// Delete this VM's disk, seed and snapshot copy. A template's live in
		// the snapshot it was for.
		for _, f := range files {
			if err := os.RemoveAll(f); err != nil {
				instanceGroup.logger.Error("error deleting instance files after it stopped", "file", f, "err", err)
			}
		}

		i.lock.Lock()

		// Clear instance's IPAM lock
		delete(i.ipamSlots, slot)

		// Clear instance from inventory
		delete(i.instances, instanceID)
		if onItsOwn && !opts.template() {
			i.exited[instanceID] = 0
		}

		i.lock.Unlock()

		err := i.ApplyNftables(instanceGroup)
		if err != nil {
			instanceGroup.logger.Error("error applying nftables rules", err)
		}
	}()

	// Update inventory
	info := &InstanceInfo{
		ID:                        instanceID,
		Name:                      instanceName,
		InstanceContextCancelFunc: instanceCancelFunc,

		HostTapIP:     hostTapIP,
		InstanceTapIP: instanceTapIP,

		InstanceTapMacAddress: instanceMac,

		SSHPublicKey:  pubKey,
		SSHPrivateKey: privKey,

		template: opts.template(),
		pending:  snap != nil,
	}
	i.instances[instanceID] = info
	if opts != nil {
		opts.Info = info
	}

	// Release lock for nftables
	i.lock.Unlock()

	// Wait for the tap device: cloud-hypervisor makes it within tens of
	// milliseconds, so look often. Up to ten seconds, as before.
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if _, err := net.InterfaceByName(instanceName); err == nil {
			break
		}
	}

	// Render and apply nftables rules (wait for tap interface)
	if err := i.ApplyNftables(instanceGroup); err != nil {
		return err
	}
	if snap == nil {
		return nil
	}

	// A restored VM still has the template's address and identity: give it
	// its own before anything connects.
	_, hostKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		instanceCancelFunc()
		return fmt.Errorf("restoring %s: %w", instanceName, err)
	}
	var out string
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	for {
		// Made for each try: it carries the host's time.
		var script string
		sent := time.Now()
		script, err = identityScript(instanceName, instanceTapIP, hostTapIP, pubKey, hostKey, sent)
		if err != nil {
			break
		}
		out, err = runAgent(ctx, vsockPath, script)
		if err == nil || ctx.Err() != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	drift, _ := clockDrift(out, time.Now())
	if err != nil {
		instanceGroup.logger.Error("restored VM did not take its identity", "instance", instanceName, "err", err)
		instanceCancelFunc()
		return fmt.Errorf("restoring %s: %w", instanceName, err)
	}
	i.lock.Lock()
	info.pending = false
	i.lock.Unlock()
	instanceGroup.logger.Info("restored instance VM", "instance", instanceName, "took", time.Since(started).Round(time.Millisecond).String(), "clock_behind", drift.String())
	return nil
}

func (i *Inventory) PrebuildInstance(instanceGroup *InstanceGroup) error {
	i.lock.RLock()
	takenSlots := len(i.ipamSlots)
	i.lock.RUnlock()

	// Short-circuit function instead of walking adddress space
	if takenSlots >= MaxIPAMSlots {
		return errors.New("available VM address space exhausted")
	}

	i.lock.Lock()

	if i.shuttingDown {
		i.lock.Unlock()
		return errors.New("system is shutting down")
	}

	// Behold, the ultimate IPv4 subnet allocation algorithm
	subnetBase := 0
	stepSize := 4

	// Walk subnets until a free slot is found and allocate it
	for {
		if subnetBase >= 255-stepSize {
			i.lock.Unlock()
			return errors.New("available VM address space exhausted")
		}

		if _, ok := i.ipamSlots[instanceGroup.MakeAddress(subnetBase)+"/30"]; !ok {
			break
		}

		subnetBase += 4
	}

	i.ipamSlots[instanceGroup.MakeAddress(subnetBase)+"/30"] = struct{}{}

	instanceIndex := subnetBase / stepSize
	instanceName := instanceGroup.instanceName(instanceIndex)

	// Generate random mac address
	randomBytes := make([]byte, 4)
	_, err := rand.Read(randomBytes)
	if err != nil {
		delete(i.ipamSlots, instanceGroup.MakeAddress(subnetBase)+"/30")
		i.lock.Unlock()
		return err
	}
	randomPart := hex.EncodeToString(randomBytes)

	// slicing like this is okay since it is an ASCII string
	instanceMac := fmt.Sprintf(
		"de:51:%s:%s:%s:%s",
		randomPart[0:2],
		randomPart[2:4],
		randomPart[4:6],
		randomPart[6:])

	hostTapIP := instanceGroup.MakeAddress(subnetBase + 1)
	instanceTapIP := instanceGroup.MakeAddress(subnetBase + 2)

	// Generate userdata image
	userdataPath, err := instanceGroup.createUserdataPrebuild(instanceName,
		instanceMac,
		instanceTapIP,
		hostTapIP,
		"/30")
	if err != nil {
		delete(i.ipamSlots, instanceGroup.MakeAddress(subnetBase)+"/30")
		i.lock.Unlock()
		return err
	}

	decompressedPath := instanceGroup.imagePath

	kernelFilePath, err := instanceGroup.bootKernel()
	if err != nil {
		delete(i.ipamSlots, instanceGroup.MakeAddress(subnetBase)+"/30")
		i.lock.Unlock()
		return err
	}

	// Start instance
	instanceContext, instanceCancelFunc := context.WithCancel(context.Background())

	hypervisorCommand := exec.CommandContext(instanceContext, "cloud-hypervisor",
		"--kernel",
		kernelFilePath,
		"--disk",
		fmt.Sprintf("path=%s,image_type=qcow2", decompressedPath),
		fmt.Sprintf("path=%s,readonly=on,image_type=raw", userdataPath),
		"--cpus",
		fmt.Sprintf("boot=%d", instanceGroup.VMNumCPUCores),
		"--memory",
		fmt.Sprintf("size=%dM", instanceGroup.VMMemoryMegabytes),
		"--net",
		fmt.Sprintf("tap=%s,mac=%s,ip=%s,mask=255.255.255.252", instanceName, instanceMac, hostTapIP),
		"--balloon",
		"size=0,free_page_reporting=on",
		"--cmdline",
		"console=hvc0 root=/dev/vda1 rw",
		"--landlock")

	consolePath := ""
	if instanceGroup.VMEnableVirtioConsole {
		// Enable console
		consolePath = filepath.Join(instanceGroup.VMDiskDir, vmWorkdir, fmt.Sprintf("%s_console", instanceName))
		// An earlier build's report isn't this one's.
		_ = os.Remove(consolePath)

		hypervisorCommand.Args = append(hypervisorCommand.Args, "--console",
			fmt.Sprintf("file=%s", consolePath))
	}

	instanceGroup.logger.Info("starting instance VM", "instance", instanceName)
	hypervisorCommand.SysProcAttr = dieWithPlugin()
	err = hypervisorCommand.Start()
	if err != nil {
		// Nothing started: give back the slot and the lock, drop the seed.
		instanceCancelFunc()
		delete(i.ipamSlots, instanceGroup.MakeAddress(subnetBase)+"/30")
		i.lock.Unlock()
		_ = os.Remove(userdataPath)
		return err
	}

	prebuildDone := make(chan struct{})

	go func() {
		//
		// VM cleanup - cancel VM context to trigger stopping the VM process and then calling this function
		//

		// Wait for VM to terminate (when context gets cancelled)
		err := hypervisorCommand.Wait()
		if err != nil {
			instanceGroup.logger.Error("error running instace: %w", err)
		}

		instanceGroup.logger.Info("instance process finished. cleaning up.", "instance", instanceName)

		// Delete cloudinit data
		err = os.Remove(userdataPath)
		if err != nil {
			instanceGroup.logger.Error("error deleting userdata after instance has been stopped: %w", err)
		}

		i.lock.Lock()

		// Clear instance's IPAM lock
		delete(i.ipamSlots, instanceGroup.MakeAddress(subnetBase)+"/30")

		// Clear instance from inventory
		delete(i.instances, instanceName)

		i.lock.Unlock()

		err = i.ApplyNftables(instanceGroup)
		if err != nil {
			instanceGroup.logger.Error("error applying nftables rules: %w", err)
		}
		prebuildDone <- struct{}{}
	}()

	// Update inventory. Not an instance the runner asked for: hidden.
	i.instances[instanceName] = &InstanceInfo{
		ID:                        instanceName,
		Name:                      instanceName,
		InstanceContextCancelFunc: instanceCancelFunc,
		template:                  true,

		HostTapIP:     hostTapIP,
		InstanceTapIP: instanceTapIP,

		InstanceTapMacAddress: instanceMac,

		SSHPublicKey:  nil,
		SSHPrivateKey: nil,
	}

	// Release lock for nftables
	i.lock.Unlock()

	// Wait for tap device to become available
	checkCounter := 0
	tapReady := false

	for {
		interfaces, err := net.Interfaces()
		if err != nil {
			return err
		}
		for _, device := range interfaces {
			if device.Name == instanceName {
				tapReady = true
				break
			}
		}

		if tapReady || checkCounter > 100 {
			break
		}

		time.Sleep(100 * time.Millisecond)
		checkCounter++
	}

	// Render and apply nftables rules (wait for tap interface)
	err = i.ApplyNftables(instanceGroup)
	if err != nil {
		return err
	}

	// Wait for prebuild to finish / cleanup
	instanceGroup.logger.Info("waiting for prebuild to finish.")
	<-prebuildDone
	instanceGroup.logger.Info("prebuild finished.")

	if consolePath == "" {
		return nil
	}
	console, err := os.ReadFile(consolePath)
	if err != nil {
		if instanceGroup.strictPrebuild {
			return fmt.Errorf("reading the prebuild's console: %w", err)
		}
		instanceGroup.logger.Warn("can't tell whether the prebuild's commands succeeded", "err", err)
		return nil
	}
	failed := prebuildFailures(console, instanceGroup.VMPrebuildCloudinitExtraCmds)
	for _, f := range failed {
		instanceGroup.logger.Error("prebuild command failed", "command", f)
	}
	if len(failed) > 0 && instanceGroup.strictPrebuild {
		return fmt.Errorf("%d of vm_prebuild_cloudinit_extra_cmds failed, the first: %s", len(failed), failed[0])
	}
	return nil
}

// prebuildFailedMarker starts the console line the prebuild writes for a
// failed command: the marker, the command's index and its exit status.
const prebuildFailedMarker = "fleetingd-prebuild-failed:"

// prebuildFailures reads the commands that failed from a prebuild's
// console, each as "#<n> (exit <status>): <command>".
func prebuildFailures(console []byte, cmds []string) []string {
	var failed []string
	for _, line := range strings.Split(string(console), "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), prebuildFailedMarker+" ")
		if !ok {
			continue
		}
		var n, status int
		if _, err := fmt.Sscanf(rest, "%d %d", &n, &status); err != nil || n < 0 || n >= len(cmds) {
			continue
		}
		failed = append(failed, fmt.Sprintf("#%d (exit %d): %s", n+1, status, cmds[n]))
	}
	return failed
}

// DestroyInstance stops one of the runner's instances and waits up to 10 s
// for it to be gone.
func (i *Inventory) DestroyInstance(name string) error {
	return i.destroy(name, false)
}

// destroy stops an instance; hidden ones (prebuild, snapshot template)
// only when hidden is set.
func (i *Inventory) destroy(name string, hidden bool) error {
	i.lock.Lock()
	instance, ok := i.instances[name]
	if !ok || (instance.template && !hidden) {
		// Gone already, or never this run's: a restarted plugin is asked
		// to remove the instances of the run before it.
		i.lock.Unlock()
		return nil
	}
	instance.InstanceContextCancelFunc()
	i.lock.Unlock()

	waitCounter := 0
	for {
		i.lock.RLock()
		_, instanceStillExists := i.instances[name]
		i.lock.RUnlock()

		if !instanceStillExists {
			return nil
		}

		waitCounter++
		if waitCounter > 100 {
			return fmt.Errorf("timed out waiting for instance %s to be removed", name)
		}

		time.Sleep(time.Millisecond * 100)
	}
}

func (i *Inventory) DestroyAllInstances() error {
	// Try to destroy all instances

	instanceNames := []string{}

	i.lock.Lock()
	// Block creation of new instances
	i.shuttingDown = true

	// Collect instance names to destroy
	for name := range i.instances {
		instanceNames = append(instanceNames, name)
	}

	i.lock.Unlock()

	for _, instanceToDestroy := range instanceNames {
		err := i.destroy(instanceToDestroy, true)
		if err != nil {
			return err
		}
	}

	return nil
}

func (i *Inventory) GetAllInstances() []string {
	// List all instance names

	instanceNames := []string{}

	i.lock.RLock()

	for name, instance := range i.instances {
		if !instance.template {
			instanceNames = append(instanceNames, name)
		}
	}

	i.lock.RUnlock()

	return instanceNames
}

// TakeExited is what Update says of the instances whose VM stopped on its
// own: first creating, then deleted. A VM that dies before fleeting has seen
// it would otherwise stay pending in fleeting forever, a VM it asked for and
// never got; and one it has seen would wait out five missed updates.
func (i *Inventory) TakeExited() map[string]provider.State {
	i.lock.Lock()
	defer i.lock.Unlock()
	states := map[string]provider.State{}
	for id, n := range i.exited {
		if n == 0 {
			states[id] = provider.StateCreating
			i.exited[id] = 1
			continue
		}
		states[id] = provider.StateDeleted
		delete(i.exited, id)
	}
	return states
}

func (i *Inventory) GetConnectInfo(name string) (*provider.ConnectInfo, error) {
	// Get an instance's conneciton info

	i.lock.RLock()

	instance, ok := i.instances[name]
	if !ok || instance.template {
		i.lock.RUnlock()
		return nil, errors.New("instance not found")
	}
	if instance.pending {
		i.lock.RUnlock()
		return nil, errors.New("instance is still being restored")
	}

	marshalledKey, err := ssh.MarshalPrivateKey(instance.SSHPrivateKey, "fleetingd")
	if err != nil {
		i.lock.RUnlock()
		return nil, err
	}

	connectionInfo := provider.ConnectInfo{
		ID:           instance.ID,
		InternalAddr: instance.InstanceTapIP,

		ConnectorConfig: provider.ConnectorConfig{
			Username: "ubuntu",
			OS:       "linux",
			Arch:     runtime.GOARCH,

			Protocol:     provider.ProtocolSSH,
			ProtocolPort: 22,
			Key:          pem.EncodeToMemory(marshalledKey),
			Keepalive:    time.Second * 10,
			Timeout:      time.Second * 3,
		},
	}

	i.lock.RUnlock()

	return &connectionInfo, nil
}

func (i *Inventory) ApplyNftables(instanceGroup *InstanceGroup) error {
	// Render nftables template for setup and apply it. VMs boot in
	// parallel: one apply at a time, each from the inventory as it is then,
	// so the last one applied knows every VM.
	i.nftLock.Lock()
	defer i.nftLock.Unlock()

	type nftablesTemplateInstanceInfo struct {
		Name                  string
		InstanceTapIP         string
		InstanceTapMacAddress string
		InstanceGateway       string
	}

	type nftablesTemplateArgs struct {
		Name            string
		Subnet          string
		EgressInterface string
		Instances       []nftablesTemplateInstanceInfo
	}

	templates, err := template.ParseFS(userDataTemplates, "templates/*.tpl")
	if err != nil {
		return err
	}

	templateArgs := nftablesTemplateArgs{
		Name:            instanceGroup.Name,
		Subnet:          instanceGroup.VMSubnet,
		EgressInterface: instanceGroup.EgressInterface,
		Instances:       []nftablesTemplateInstanceInfo{},
	}

	i.lock.RLock()
	for _, instance := range i.instances {
		templateArgs.Instances = append(templateArgs.Instances, nftablesTemplateInstanceInfo{
			Name:                  instance.Name,
			InstanceTapIP:         instance.InstanceTapIP,
			InstanceTapMacAddress: instance.InstanceTapMacAddress,
			InstanceGateway:       instance.HostTapIP,
		})
	}
	i.lock.RUnlock()

	rulesetPath := filepath.Join(instanceGroup.VMDiskDir, "ruleset.nft")

	rulesetFile, err := os.Create(rulesetPath)
	if err != nil {
		return err
	}
	defer rulesetFile.Close()

	err = templates.ExecuteTemplate(rulesetFile, "nftables-rules.tpl", templateArgs)
	if err != nil {
		return err
	}

	err = rulesetFile.Close()
	if err != nil {
		return err
	}

	err = exec.Command("nft", "-f", rulesetPath).Run()
	if err != nil {
		return err
	}

	return nil
}

// lastLines is the end of a process's output, for a log line.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}
