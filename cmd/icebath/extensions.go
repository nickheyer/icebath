package main

import (
	_ "github.com/nickheyer/icebath/pkg/ext/sprint"
	"github.com/nickheyer/icebath/pkg/ext/vsc"
)

func init() { vsc.Version = func() string { return version } }
