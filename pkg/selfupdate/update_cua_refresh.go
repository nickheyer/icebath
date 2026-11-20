//go:build !icebath_core && !icebath_noextras

package selfupdate

import (
	"github.com/nickheyer/icebath/pkg/codeexec"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Package seams of the cua-driver refresh the tests swap
var (
	cuaRefreshInstalled = pm.Installed
	installCuaDriver    = codeexec.InstallCuaDriver
)
