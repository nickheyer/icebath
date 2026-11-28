package config

import (
	"time"

	"github.com/nickheyer/icebath/internal/config"
)

// Registers the settings of skills, skills the skills of the profile to tick from and apps the apps a skill can be turned off in alone
func Register(cfg *config.Config, skills, apps func() []config.Option) {
	cfg.Register(config.Sections{{
		Key:   Key,
		Title: "Skills",
		About: "Instructions the agent loads for certain kinds of work",
		Group: config.GroupAbilities,
		Order: 60,
		Settings: []config.Setting{
			{Key: Disabled, Title: "Skills turned off", About: "Skills that the agent is never offered.", Default: []string{}, Options: skills,
				Slash: "skills", Custom: true},
			{Key: ProjectDiscovery, Title: "From projects", About: "Load the skills of a trusted project that the session works in.",
				Help: "Project skills are read from the .icebath/skills and .agents/skills folders of the project.", Default: true},
			{Key: WriteApproval, Title: "Ask before changes", About: "Ask before the agent creates, changes or deletes a skill.", Default: false},
			{Key: Tier1Advisory, Title: "Check on install", About: "Show a quick safety scan of a skill when you install it.",
				Help: "The scan uses the skillevaluator command when it is installed. The scan never blocks an install.", Default: true},
			{Key: AutoLoad, Title: "Always loaded", About: "Skills loaded into every session from its start.", Default: []string{}, Options: skills,
				Apply: config.NewSessions},
			{Key: ExternalDirs, Title: "Extra folders", About: "Folders of skills shared with other tools, read after the profile's own.",
				Help:    "A skill in the profile's own folder wins over one of the same name here, and one in a higher folder over a lower one.",
				Default: []string{}, Path: &config.Path{Kind: config.PathFolder}, Entry: &config.Entry{Noun: "folder"}, Ordered: true},
			{Key: TrustedProjectDirs, Title: "Trusted projects", About: "Project folders whose own skills load.", Default: []string{}, Slash: "skills",
				Path: &config.Path{Kind: config.PathFolder}, Entry: &config.Entry{Noun: "folder"}, Custom: true},
			{Key: PlatformDisabled, Title: "Skills off in one app", About: "Skills turned off in one app only, such as this app.",
				Default: map[string]any{}, Slash: "skills", Custom: true, Entry: &config.Entry{Noun: "app", Names: apps, Fields: []config.Setting{
					{Title: "Skills turned off", About: "The skills turned off in this app.", Default: []string{}, Options: skills},
				}}},
			{Key: GuardAgentCreated, Title: "Scan the agent's skills", About: "Scan each skill the agent writes and undo a write that the scan blocks.",
				Default: false, Advanced: true},
			{Key: Ledger, Title: "Change history", About: "Record each change to a skill with the files before and after, so you can undo the change.",
				Default: true, Advanced: true},
			{Key: TemplateVars, Title: "Fill in variables", About: "Fill in the folder of a skill and the session ID where the skill names them.",
				Help: "The skill names them as ${ICEBATH_SKILL_DIR} and ${ICEBATH_SESSION_ID}.", Default: true, Advanced: true},
			{Key: InlineShell, Title: "Run shell snippets", About: "Run the shell snippets of a skill as the skill loads, and put in their output.",
				Help: "Snippets are written as !`command`. Never for skills from the community hub.", Default: false, Advanced: true},
			{Key: InlineShellTimeout, Title: "Snippet time limit", About: "How long one shell snippet of a skill may run.", Default: 10 * time.Second,
				Zero: "10 seconds", Advanced: true},
			{Key: LedgerMaxBytes, Title: "History size", About: "How large the change history may grow before its oldest entries go.",
				Default: 5 * 1024 * 1024, Zero: "No limit", Counts: config.Bytes, Least: config.Num(0), Advanced: true},
			{Key: CreateDir, Title: "New skill folder", About: "The folder that new skills are written to.", Default: "", Zero: "Profile's skills folder",
				Advanced: true, Path: &config.Path{Kind: config.PathFolder}},
			{Key: BundlesDir, Title: "Bundles folder", About: "The folder of skill bundles.", Default: "", Zero: "Profile's bundles folder",
				Env: []string{"ICEBATH_BUNDLES_DIR"}, Advanced: true, Path: &config.Path{Kind: config.PathFolder}},
		},
	}})
}
