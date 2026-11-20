//go:build windows

package gateway

// Windows has no ps listing, so no diagnostic starts
func spawnAsyncDiagnostic(string, string, float64) (int, bool) { return 0, false }
