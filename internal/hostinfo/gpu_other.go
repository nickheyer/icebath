//go:build !windows

package hostinfo

import "fmt"

// Facts kept for one platform, none outside Windows
type nativeCache struct{}

// DXGI exists only on Windows
func (h *Host) DXGIAdapters() ([]DXGIAdapter, error) {
	return nil, fmt.Errorf("dxgi adapters: %w", ErrAbsent)
}
