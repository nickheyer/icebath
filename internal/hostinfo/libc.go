package hostinfo

import "context"

// C library family of a Linux userland
type LibCFlavor string

const (
	GLibC  LibCFlavor = "glibc"
	Musl   LibCFlavor = "musl"
	Bionic LibCFlavor = "bionic"
)

// C library of the Linux userland, Version empty when the library states none
type LibC struct {
	Flavor  LibCFlavor
	Version string
}

// C library the native userland links against, read once, ErrAbsent off Linux
func (h *Host) LibC(ctx context.Context) (LibC, error) { return h.libc.get(ctx, probeLibC) }
