package config

import (
	"time"

	"github.com/nickheyer/icebath/internal/config"
)

func Register(cfg *config.Config, skills func() []config.Option) {
	cfg.Register(config.Sections{{
		Key:   Key,
		Title: "Skills",
		About: "Where skills come from and how the agent loads and changes them",
		Settings: []config.Setting{
			{Key: Disabled, Title: "Disabled skills", About: "Skills the agent is not offered on any surface", Default: []string{}, Options: skills},
			{Key: PlatformDisabled, Title: "Per-app disabled", About: "Skills turned off on one surface only: a list of skill names by surface, such as tui", Default: map[string]any{}},
			{Key: AutoLoad, Title: "Preloaded skills", About: "Skills loaded into the system prompt of every session", Default: []string{}, Options: skills},
			{Key: ExternalDirs, Title: "Extra skill folders", About: "Folders of skills shared with other tools, listed after the profile's own; a skill of the same name in the profile's folder wins", Default: []string{}},
			{Key: CreateDir, Title: "New skill folder", About: "Folder new skills are written to; the profile's skills folder when empty", Default: ""},
			{Key: ProjectDiscovery, Title: "Project skills", About: "Load the skills of a trusted project the session works in, from .icebath/skills and .agents/skills", Default: true},
			{Key: TrustedProjectDirs, Title: "Trusted projects", About: "Project folders whose own skills load", Default: []string{}},
			{Key: TemplateVars, Title: "Skill variables", About: "Put the skill's folder and the session id in place of ${ICEBATH_SKILL_DIR} and ${ICEBATH_SESSION_ID} when a skill loads", Default: true},
			{Key: InlineShell, Title: "Skill shell snippets", About: "Run the !`command` snippets of a skill when it loads and put their output in their place; never for community hub skills", Default: false},
			{Key: InlineShellTimeout, Title: "Snippet time limit", About: "Seconds a skill shell snippet may run", Default: 10 * time.Second},
			{Key: WriteApproval, Title: "Approve skill writes", About: "Ask before the agent creates, changes or deletes a skill", Default: false},
			{Key: GuardAgentCreated, Title: "Scan agent skills", About: "Scan each skill the agent writes and undo a write the scan blocks", Default: false},
			{Key: Ledger, Title: "Skill change log", About: "Record every change to a skill with the files before and after, so it can be rolled back", Default: true},
			{Key: LedgerMaxBytes, Title: "Change log size", About: "Bytes the change log may grow to before its oldest entries are dropped; 0 for no limit", Default: 5 * 1024 * 1024},
			{Key: BundlesDir, Title: "Bundles folder", About: "Folder of skill bundles; skill-bundles in the profile's folder when empty", Default: "", Env: []string{"ICEBATH_BUNDLES_DIR"}},
			{Key: Tier1Advisory, Title: "Install safety check", About: "Show a quick safety scan of a skill when you install it, from the skillevaluator command when it is installed; it never blocks an install", Default: true},
		},
	}})
}
