package dashboard

import "github.com/nickheyer/icebath/pkg/internal/pymimetypes"

// Web standard content types of the dashboard assets, text/javascript per RFC 9239
var webAssetMIMETypes = []struct{ ext, mime string }{
	{".js", "text/javascript"},
	{".mjs", "text/javascript"},
	{".cjs", "text/javascript"},
	{".css", "text/css"},
	{".json", "application/json"},
	{".map", "application/json"},
	{".svg", "image/svg+xml"},
	{".wasm", "application/wasm"},
	{".webp", "image/webp"},
	{".webmanifest", "application/manifest+json"},
	{".woff", "font/woff"},
	{".woff2", "font/woff2"},
	{".ico", "image/x-icon"},
}

// Pins the asset content types over any host mapping, idempotent and run before the dashboard serves a file
func normalizeWebAssetMIMETypes() {
	db := pymimetypes.Default()
	for _, e := range webAssetMIMETypes {
		db.AddType(e.mime, e.ext, true)
	}
}
