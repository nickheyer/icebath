package provider

import "github.com/nickheyer/icebath/pkg/profiles"

// Parsed distribution.yaml of a profile, defined in pkg/profiles so the profile installer shares it
type DistributionManifest = profiles.DistributionManifest

// Manifest next to profileDir, nil and nil when the directory holds no distribution.yaml
func ReadManifest(profileDir string) (*DistributionManifest, error) {
	return profiles.ReadManifest(profileDir)
}
