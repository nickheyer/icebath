package tools

import (
	"github.com/nickheyer/icebath/internal/plugins"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/browser"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/camofox"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/checkpoints"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/clarify"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/codeexec"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/computeruse"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/files"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/generate"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/lsp"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/mcp"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/sessionsearch"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/terminal"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/todo"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/tts"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/verify"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/vision"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/web"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/worktrees"
	"github.com/nickheyer/icebath/internal/plugins/defaults/tools/xsearch"
)

func Register(registries *plugins.Registries) {
	terminal.Register(registries)
	lsp.Register(registries)
	files.Register(registries)
	clarify.Register(registries)
	vision.Register(registries)
	todo.Register(registries)
	web.Register(registries)
	xsearch.Register(registries)
	sessionsearch.Register(registries)
	mcp.Register(registries)
	checkpoints.Register(registries)
	worktrees.Register(registries)
	codeexec.Register(registries)
	computeruse.Register(registries)
	browser.Register(registries)
	camofox.Register(registries)
	verify.Register(registries)
	generate.Register(registries)
	tts.Register(registries)
}
