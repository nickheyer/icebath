package pm

import "context"

// Points the core project root at dir until restore runs, the way tests patch the repo root
func OverrideCoreRepo(root string) (restore func()) {
	previous := repoRoot
	repoRoot = func() string { return root }
	return func() { repoRoot = previous }
}

// Answers every toolchain lookup with the given uv and python binaries until restore runs, the way tests patch _toolchain
func OverrideUVToolchain(uv, python string) (restore func()) {
	previous := uvToolchain
	uvToolchain = func(context.Context, bool, bool) (string, string, bool, error) { return uv, python, true, nil }
	return func() { uvToolchain = previous }
}
