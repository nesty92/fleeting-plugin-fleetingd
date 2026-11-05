package main

import (
	_ "embed"
	"fmt"
	"os"

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

	// The plugin dies with the runner that started it: an orphan would keep
	// its VMs and the disk directory's lock from the runner's next start.
	if err := fleetingd.DieWithParent(); err != nil {
		fmt.Fprintln(os.Stderr, "fleetingd:", err)
		os.Exit(1)
	}
	plugin.Main(&fleetingd.InstanceGroup{}, fleetingd.Version)
}
