//go:build !windows

package local

func libraryFileName(name string) string { return "lib" + name + ".so" }

func addLibraryDir(string) (func(), error) { return func() {}, nil }
