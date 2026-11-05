package provider

import "github.com/nickheyer/icebath/pkg/profiles"

// Sticky active profile of the process root, default when unset
func activeStickyProfile() string { return profiles.GetActiveProfile("") }
