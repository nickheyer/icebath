//go:build icebath_core || icebath_noextras

package main

// The core and plugins builds still link plugin and extra packages through the upward edges in the tiers lane notes
func init() { cutRemainingUpwardTierEdges() }
