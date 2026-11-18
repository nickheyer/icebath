package logging

// Logger named after this package, every record of the package carries it
func pkgLog() *Logger { return GetLogger("logging") }
