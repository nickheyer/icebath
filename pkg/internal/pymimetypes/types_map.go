package pymimetypes

// Type the strict table of the module database maps an extension to, the mimetypes.types_map lookup
func TypesMapGet(ext string) (string, bool) {
	typ, ok := Default().types[1][ext]
	return typ, ok
}
