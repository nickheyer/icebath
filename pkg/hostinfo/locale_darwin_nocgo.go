//go:build darwin && !cgo

package hostinfo

// CoreFoundation is reachable only through cgo, so a cgo free build reports no locale
func darwinPreferredLanguage() string { return "" }
