package terminal

import (
	"fmt"

	"github.com/spf13/cast"

	"github.com/nickheyer/icebath/internal/config"
	terminalconfig "github.com/nickheyer/icebath/internal/terminal/config"
)

// What a container or cloud sandbox is given, as the profile's settings say
type Resources struct {
	// CPU cores
	CPU float64
	// Megabytes of memory and of disk
	Memory, Disk int
	// The sandbox's files are kept between sessions
	Persistent bool
}

// Resources of the profile's container and cloud sandboxes, or an error naming the setting that holds no number
func SandboxResources(cfg *config.Config) (Resources, error) {
	cpu, err := cast.ToFloat64E(cfg.Get(terminalconfig.ContainerCPU))
	if err != nil {
		return Resources{}, fmt.Errorf("%s holds %q, which is not a number; fix it in /settings Terminal", terminalconfig.ContainerCPU, cfg.GetString(terminalconfig.ContainerCPU))
	}
	resources := Resources{CPU: cpu, Persistent: cfg.GetBool(terminalconfig.ContainerPersistent)}
	for _, whole := range []struct {
		key  string
		into *int
	}{{terminalconfig.ContainerMemory, &resources.Memory}, {terminalconfig.ContainerDisk, &resources.Disk}} {
		if *whole.into, err = cast.ToIntE(cfg.Get(whole.key)); err != nil {
			return Resources{}, fmt.Errorf("%s holds %q, which is not a whole number; fix it in /settings Terminal", whole.key, cfg.GetString(whole.key))
		}
	}
	return resources, nil
}
