package acputil

import "github.com/nickheyer/icebath/pkg/logging"

// Logger every record of this package goes through
func pkgLog() *logging.Logger { return logging.GetLogger("utils/acp") }
