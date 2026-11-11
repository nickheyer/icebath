package selfupdate

import (
	"github.com/nickheyer/icebath/pkg/codeexec"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Package seams of the cua-driver refresh the tests swap as hermes monkeypatches pm and the setup module
var (
	cuaRefreshInstalled = pm.Installed
	installCuaDriver    = codeexec.InstallCuaDriver
)
