package plugins

// Directory read in place of the embedded in-tree catalog, empty for the embedded one
var catalogDirOverride string

// Points the in-tree catalog at a directory, the returned func putting the previous source back
func SwapCatalogDir(dir string) func() {
	prev := catalogDirOverride
	catalogDirOverride = dir
	return func() { catalogDirOverride = prev }
}
