package pathutil

// Absolute path with every symlink resolved as far as the path exists, the missing tail and any symlink loop kept as written
func Resolve(p string) (string, error) { return resolve(p, false) }

// Absolute path with every symlink resolved, failing when any part of it is missing, not a directory where one is needed or a symlink loop
func ResolveExisting(p string) (string, error) { return resolve(p, true) }

// Where a symlink in dir holding the text target leads, resolved as Resolve resolves a path, a relative target walked from dir the way the system follows it instead of being cleaned against dir first
func ResolveLink(dir, target string) (string, error) { return resolveLink(dir, target) }
