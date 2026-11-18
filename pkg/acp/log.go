package acp

import "github.com/nickheyer/icebath/pkg/logging"

// Logger named after this package, every record of the package carries it
func pkgLog() *logging.Logger { return logging.GetLogger("acp") }
