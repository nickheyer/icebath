//go:build darwin && !cgo

package hostinfo

// IOKit is reachable only through cgo, so a cgo free build reports the registry as unreadable
func displayVendorIDs() ([]string, bool) { return nil, false }
