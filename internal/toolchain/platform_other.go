//go:build !darwin

package toolchain

import "context"

// Leaves a staged tree as unpacked, quarantine attributes and code signatures being macOS matters
func finishPlatform(ctx context.Context, _ string, _ []string) error { return ctx.Err() }
