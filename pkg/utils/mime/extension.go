package mimeutil

import "mime"

// Preferred file extension with its leading dot for a media type, matched without regard to case, parameters or surrounding space: the built-in table's for every type it holds, else the alphabetically first the host knows through the standard mime package, "" when neither knows the type
func ExtensionByType(typ string) string {
	key := essence(typ)
	if ext, ok := lookup().byType[key]; ok {
		return ext
	}
	if key == "" {
		return ""
	}
	if exts, err := mime.ExtensionsByType(key); err == nil && len(exts) > 0 {
		return exts[0]
	}
	return ""
}
