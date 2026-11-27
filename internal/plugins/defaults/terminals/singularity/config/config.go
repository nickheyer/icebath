package config

import terminalconfig "github.com/nickheyer/icebath/internal/terminal/config"

const (
	Key = terminalconfig.Key + ".singularity"
	// Image the instance starts from, icebath's own sandbox image when empty
	Image = Key + ".image"
	// Folder of the overlays and the image cache
	ScratchDir = Key + ".scratch_dir"
	// Folder of the built .sif images
	CacheDir = Key + ".cache_dir"
	// Gigabytes the scratch folder's sandboxes may take before a warning
	DiskWarning = Key + ".disk_warning_gb"
)
