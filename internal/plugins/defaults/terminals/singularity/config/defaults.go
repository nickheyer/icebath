package config

import (
	"github.com/nickheyer/icebath/internal/config"
	terminalconfig "github.com/nickheyer/icebath/internal/terminal/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: terminalconfig.Key,
		Title:  "Singularity/Apptainer",
		About:  "Commands run in a rootless Apptainer or Singularity instance, as on HPC machines",
		Settings: []config.Setting{
			{Key: Image, Title: "Apptainer image", About: "Image the Apptainer sandbox starts from: a registry image, a .sif file or any Apptainer source; empty uses icebath's own sandbox image", Default: ""},
			{Key: ScratchDir, Title: "Singularity scratch", About: "Folder for Singularity overlays and the image cache; empty uses a writable /scratch, else the sandboxes folder", Default: "", Env: []string{"TERMINAL_SCRATCH_DIR"}},
			{Key: CacheDir, Title: "Image cache", About: "Folder the .sif images built from registry images are kept in; empty uses .apptainer in the scratch folder", Default: "", Env: []string{"APPTAINER_CACHEDIR"}},
			{Key: DiskWarning, Title: "Disk warning", About: "Gigabytes Singularity sandboxes may take in the scratch folder before a warning", Default: 500.0, Env: []string{"TERMINAL_DISK_WARNING_GB"}},
		},
	}})
}
