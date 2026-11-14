package local

import "github.com/nickheyer/icebath/internal/dl"

func libraryFileName(name string) string { return name + ".dll" }

func addLibraryDir(dir string) (func(), error) {
	remove, err := dl.AddDLLDirectory(dir)
	if err != nil {
		return nil, err
	}
	return func() { _ = remove() }, nil
}
