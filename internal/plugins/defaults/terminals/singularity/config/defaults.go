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
		About:  "A rootless Apptainer or Singularity container",
		Order:  70,
		Settings: []config.Setting{
			{Key: Image, Title: "Apptainer image", About: "The image that the sandbox starts from.",
				Help: "A registry image, a .sif file or any other source that Apptainer takes.", Default: "", Zero: "The icebath image"},
			{Key: ScratchDir, Title: "Scratch folder", About: "The folder for sandbox overlays and the image cache.",
				Help: "When not set, a writable /scratch is used, else the sandbox folder.", Default: "", Zero: "Automatic",
				Env: []string{"TERMINAL_SCRATCH_DIR"}, Advanced: true},
			{Key: CacheDir, Title: "Image cache", About: "The folder that keeps the .sif images built from registry images.", Default: "",
				Zero: "In scratch folder", Env: []string{"APPTAINER_CACHEDIR"}, Advanced: true},
			{Key: DiskWarning, Title: "Disk warning", About: "How much disk the sandboxes may take in the scratch folder before a warning.", Default: 500.0,
				Counts: "GB", Env: []string{"TERMINAL_DISK_WARNING_GB"}, Advanced: true},
		},
	}})
}
