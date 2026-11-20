package provider

import "github.com/nickheyer/icebath/pkg/config"

// Hands pkg/config the multiplex gate it reads for local time but cannot import from this package
func init() { config.IsMultiplexActive = IsMultiplexActive }
