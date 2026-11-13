package main

// Links every package whose init registers a hook, builtin plugin or platform adapter into the binary
import (
	_ "github.com/nickheyer/icebath/pkg/builtin"
	_ "github.com/nickheyer/icebath/pkg/builtin/achievements"
	_ "github.com/nickheyer/icebath/pkg/builtin/delegationdefault"
	_ "github.com/nickheyer/icebath/pkg/builtin/diskcleanup"
	_ "github.com/nickheyer/icebath/pkg/builtin/securityguidance"
	_ "github.com/nickheyer/icebath/pkg/builtin/teamspipeline"
	_ "github.com/nickheyer/icebath/pkg/config/remote"
	_ "github.com/nickheyer/icebath/pkg/gateway/messagetools"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/bundled"
	_ "github.com/nickheyer/icebath/pkg/profiles/manage"
	_ "github.com/nickheyer/icebath/pkg/vxl"
)
