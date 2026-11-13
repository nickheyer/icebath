package skills

import "embed"

// Skills seeded into every home by default
//
//go:embed all:bundled
var Bundled embed.FS

// Heavier skills installed on request through the hub
//
//go:embed all:optional
var Optional embed.FS
