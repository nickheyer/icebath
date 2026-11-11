package plugins

import (
	"context"
	"net/http"
)

// Directory read in place of the embedded in-tree catalog, empty for the embedded one
var catalogDirOverride string

// Points the in-tree catalog at a directory, the returned func putting the previous source back
func SwapCatalogDir(dir string) func() {
	prev := catalogDirOverride
	catalogDirOverride = dir
	return func() { catalogDirOverride = prev }
}

// Replaces the installer owned install metadata of a home
func WriteInstallMetadata(ctx context.Context, h string, metadata map[string]any) error {
	return writeInstallMetadata(ctx, h, metadata)
}

// Replaces the HTTP client of the live catalog fetch with the dead host memory cleared, the returned func restoring both
func SwapLiveCatalogClient(client *http.Client) func() {
	liveFetchMu.Lock()
	prevClient, prevUntil := liveCatalogClient, liveFetchFailedUntil
	liveCatalogClient, liveFetchFailedUntil = client, 0
	liveFetchMu.Unlock()
	return func() {
		liveFetchMu.Lock()
		liveCatalogClient, liveFetchFailedUntil = prevClient, prevUntil
		liveFetchMu.Unlock()
	}
}

// Forgets a remembered live catalog failure so the next fetch tries the network again
func ForgetLiveCatalogFailure() {
	liveFetchMu.Lock()
	liveFetchFailedUntil = 0
	liveFetchMu.Unlock()
}
