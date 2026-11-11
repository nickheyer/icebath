package app

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Slash handler of one canonical command, false when the REPL should exit
type slashHandler func(c *CLI, cmd string) bool

// Wraps a handler that takes the original command and never asks to exit
func withArg(fn func(c *CLI, cmd string)) slashHandler {
	return func(c *CLI, cmd string) bool { fn(c, cmd); return true }
}

// Wraps a handler that takes no argument and never asks to exit
func noArg(fn func(c *CLI)) slashHandler {
	return func(c *CLI, _ string) bool { fn(c); return true }
}

// Canonical command to handler, the _SLASH_DISPATCH table plus every _handle_name_command of the CLI
var slashDispatch = map[string]slashHandler{
	"exit": (*CLI).cmdExit, "quit": (*CLI).cmdExit, "help": withArg((*CLI).cmdHelp),
	"palette": noArg((*CLI).openCommandPalette), "whoami": noArg((*CLI).handleWhoamiCommand),
	"profile": noArg((*CLI).handleProfileCommand), "toolsets": noArg((*CLI).showToolsets),
	"config": noArg((*CLI).showConfig), "redraw": withArg((*CLI).cmdRedraw), "clear": withArg((*CLI).cmdClear),
	"history": noArg((*CLI).showHistory), "title": withArg((*CLI).cmdTitle), "new": withArg((*CLI).cmdNew),
	"model": withArg((*CLI).handleModelSwitch), "codex-runtime": withArg((*CLI).handleCodexRuntime),
	"retry": withArg((*CLI).cmdRetry), "prompt": withArg((*CLI).handlePromptComposeCommand),
	"undo": withArg((*CLI).cmdUndo), "save": withArg((*CLI).saveConversation), "skills": withArg((*CLI).cmdSkills),
	"platforms": noArg((*CLI).showGatewayStatus), "status": noArg((*CLI).showSessionStatus),
	"context": withArg((*CLI).showContextBreakdown), "egress": withArg((*CLI).cmdEgress),
	"statusbar": withArg((*CLI).cmdStatusbar), "verbose": noArg((*CLI).toggleVerbose), "yolo": noArg((*CLI).toggleYolo),
	"compress": withArg((*CLI).manualCompress), "subscription": noArg((*CLI).showSubscription),
	"topup": withArg((*CLI).showBilling), "insights": withArg((*CLI).showInsights), "update": (*CLI).cmdUpdate,
	"version": withArg((*CLI).cmdVersion), "paste": noArg((*CLI).handlePasteCommand), "reload": withArg((*CLI).cmdReload),
	"reload-mcp": withArg((*CLI).confirmAndReloadMCP), "reload-skills": withArg((*CLI).cmdReloadSkills),
	"plugins": withArg((*CLI).cmdPlugins), "stop": noArg((*CLI).handleStopCommand),
	"agents": noArg((*CLI).handleAgentsCommand), "bg": withArg((*CLI).handleBackgroundCommand),
	"queue": withArg((*CLI).cmdQueue), "steer": withArg((*CLI).cmdSteer), "moa": withArg((*CLI).cmdMoa),
	"approvals": withArg((*CLI).handleApprovalsCommand), "autoprune": withArg((*CLI).handleAutopruneCommand),
	"background": withArg((*CLI).handleBackgroundCommand), "battery": withArg((*CLI).handleBatteryCommand),
	"blueprint": withArg((*CLI).handleBlueprintCommand), "branch": withArg((*CLI).handleBranchCommand),
	"browser": withArg((*CLI).handleBrowserCommand), "btw": withArg((*CLI).handleBtwCommand),
	"bundles": withArg((*CLI).handleBundlesCommand), "busy": withArg((*CLI).handleBusyCommand),
	"copy": withArg((*CLI).handleCopyCommand), "cron": withArg((*CLI).handleCronCommand),
	"curator": withArg((*CLI).handleCuratorCommand), "debug": withArg((*CLI).handleDebugCommand),
	"diff": withArg((*CLI).handleDiffCommand), "discard": withArg((*CLI).handleDiscardCommand),
	"export": withArg((*CLI).handleExportCommand), "fast": withArg((*CLI).handleFastCommand),
	"focus": withArg((*CLI).handleFocusCommand), "footer": withArg((*CLI).handleFooterCommand),
	"goal": withArg((*CLI).handleGoalCommand), "handoff": (*CLI).handleHandoffCommand,
	"hatch": withArg((*CLI).handleHatchCommand), "heartbeat": withArg((*CLI).handleHeartbeatCommand),
	"image": withArg((*CLI).handleImageCommand), "import": withArg((*CLI).handleImportCommand),
	"indicator": withArg((*CLI).handleIndicatorCommand), "init": withArg((*CLI).handleInitCommand),
	"journey": withArg((*CLI).handleJourneyCommand), "kanban": withArg((*CLI).handleKanbanCommand),
	"learn": withArg((*CLI).handleLearnCommand), "login": withArg((*CLI).handleLoginCommand),
	"loop": withArg((*CLI).handleLoopCommand), "memory": withArg((*CLI).handleMemoryCommand),
	"personality": withArg((*CLI).handlePersonalityCommand), "pet": withArg((*CLI).handlePetCommand),
	"plan": withArg((*CLI).handlePlanCommand), "prompt-compose": withArg((*CLI).handlePromptComposeCommand), "prompt_compose": withArg((*CLI).handlePromptComposeCommand),
	"reasoning": withArg((*CLI).handleReasoningCommand), "refine": withArg((*CLI).handleRefineCommand),
	"resume": withArg((*CLI).handleResumeCommand), "review": withArg((*CLI).handleReviewCommand),
	"rollback": withArg((*CLI).handleRollbackCommand), "sessions": withArg((*CLI).handleSessionsCommand),
	"skin": withArg((*CLI).handleSkinCommand), "snapshot": withArg((*CLI).handleSnapshotCommand),
	"subgoal": withArg((*CLI).handleSubgoalCommand), "suggestions": withArg((*CLI).handleSuggestionsCommand),
	"timestamps": withArg((*CLI).handleTimestampsCommand), "tools": withArg((*CLI).handleToolsCommand),
	"usage": withArg((*CLI).handleUsageCommand), "voice": withArg((*CLI).handleVoiceCommand),
	"wake": withArg((*CLI).handleWakeCommand),
	"worktree": func(c *CLI, cmd string) bool {
		if err := c.handleWorktreeCommand(cmd); err != nil {
			panic(err)
		}
		return true
	},
}

// Dispatches a slash command, false to exit the REPL, an internal re entry counted once
func (c *CLI) processCommand(command string) bool {
	redispatch := c.slashRedispatch
	c.slashRedispatch = false
	cmdLower := strings.ToLower(strings.TrimSpace(command))
	cmdOriginal := strings.TrimSpace(command)
	words := strings.Fields(cmdLower)
	if len(words) == 0 {
		panic("IndexError: list index out of range")
	}
	baseWord := strings.TrimLeft(words[0], "/")
	def := commands.Resolve(baseWord)
	canonical := baseWord
	if def != nil {
		canonical = def.Name
	}
	if !redispatch {
		observability.RecordSlashCommand(c.ctx, canonical, "cli")
	}
	if canonical != "resume" && canonical != "sessions" {
		c.pendingResumeSessions = nil
	}
	handler, ok := slashDispatch[canonical]
	if def != nil {
		sessionKey, platform := c.sessionID, "cli"
		plugins.FirePreCommandHook(c.ctx, "cli", canonical, baseWord, slashArgs(cmdOriginal), &sessionKey, &platform)
	}
	if !ok {
		return c.processUnregisteredSlash(cmdOriginal, cmdLower)
	}
	return handler(c, cmdOriginal)
}
