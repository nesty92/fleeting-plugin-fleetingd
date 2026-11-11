package main

import (
	_ "embed"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/hashicorp/go-hclog"

	fleetingd "github.com/helmholtzcloud/fleeting-plugin-fleetingd"
	"gitlab.com/gitlab-org/fleeting/fleeting/plugin"
)

//go:embed NOTICE
var licenseNotice string

//go:embed LICENSE
var license string

func main() {
	if len(os.Args) > 1 && os.Args[1] == "licenses" {
		fmt.Println(licenseNotice)
		fmt.Println("This software's license:")
		fmt.Println(license)
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "build-image" {
		if err := buildImage(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "fleetingd build-image:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "disk" {
		if err := disk(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "fleetingd disk:", err)
			os.Exit(1)
		}
		return
	}

	// The plugin dies with the runner that started it: an orphan would keep
	// its VMs and the disk directory's lock from the runner's next start.
	if err := fleetingd.DieWithParent(); err != nil {
		fmt.Fprintln(os.Stderr, "fleetingd:", err)
		os.Exit(1)
	}
	plugin.Main(&fleetingd.InstanceGroup{}, fleetingd.Version)
}

// disk adds a disk to a running VM or takes it out: see disk.go.
func disk(args []string) error {
	fs := flag.NewFlagSet("disk", flag.ExitOnError)
	config := fs.String("config", "", "plugin_config as JSON, or - to read it from stdin")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: fleeting-plugin-fleetingd disk -config <file> attach <instance> <layer>")
		fmt.Fprintln(fs.Output(), "       fleeting-plugin-fleetingd disk -config <file> detach <instance>")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	if *config == "" {
		return fmt.Errorf("-config is required")
	}
	var settings []byte
	var err error
	if *config == "-" {
		settings, err = io.ReadAll(os.Stdin)
	} else {
		settings, err = os.ReadFile(*config)
	}
	if err != nil {
		return err
	}
	return fleetingd.Disk(settings, fs.Args())
}

// buildImage builds an image for vm_image_url from a plugin_config.
func buildImage(args []string) error {
	fs := flag.NewFlagSet("build-image", flag.ExitOnError)
	config := fs.String("config", "", "plugin_config as JSON: vm_disk_directory, vm_prebuild_cloudinit_extra_cmds and the rest")
	out := fs.String("out", "", "directory to write the image, its kernel and SHA256SUMS to")
	name := fs.String("name", "image", "file name prefix: <name>-<arch>.qcow2 and .vmlinuz")
	_ = fs.Parse(args)
	if *config == "" || *out == "" {
		return fmt.Errorf("-config and -out are required")
	}
	settings, err := os.ReadFile(*config)
	if err != nil {
		return err
	}
	log := hclog.New(&hclog.LoggerOptions{Name: "fleetingd", Level: hclog.Info, Output: os.Stderr})
	return fleetingd.BuildImage(settings, *out, *name, log)
}
