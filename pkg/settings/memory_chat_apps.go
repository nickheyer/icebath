package settings

// Notes the agent keeps between sessions
var Memory = Register(&Bool{Meta: Meta{Key: "memory.memory_enabled", Label: "Memory",
	Short: "Let the agent keep notes about your work between sessions.",
	Group: GroupMemory, Order: 10, Apply: NewSession},
	Default: true})

// Profile of you the agent builds
var LearnAboutMe = Register(&Bool{Meta: Meta{Key: "memory.user_profile_enabled", Label: "Learn about me",
	Short: "Let the agent keep a short profile of you and your preferences.",
	Group: GroupMemory, Order: 20, Apply: NewSession},
	Default: true})

// Approval of each memory write
var AskBeforeMemory = Register(&Bool{Meta: Meta{Key: "memory.write_approval", Label: "Ask before saving",
	Short: "Ask you before the agent saves or changes a memory.",
	Group: GroupMemory, Order: 30},
	Default: false})

// Reopening the last session at start
var ResumeLast = Register(&Bool{Meta: Meta{Key: "display.tui_auto_resume_recent", Label: "Resume last session",
	Short: "Reopen your most recent session when the terminal UI starts.",
	Group: GroupMemory, Order: 40, Apply: Restart},
	Default: false})

// Deleting old sessions
var AutoPrune = Register(&Bool{Meta: Meta{Key: "sessions.auto_prune", Label: "Delete old sessions",
	Short: "Delete sessions older than the keep time when the service starts.",
	Group: GroupMemory, Order: 50, Apply: Restart},
	Default: false})

// Footer under chat app replies
var ReplyFooter = Register(&Bool{Meta: Meta{Key: "display.runtime_footer.enabled", Platforms: true, Label: "Reply footer",
	Short: "Add a line with model and context use under chat app replies.",
	Group: GroupChatApps, Order: 20, Slash: "footer"},
	Default: false})

// Pause before chat app replies
var HumanDelay = Register(&Enum{Meta: Meta{Key: "human_delay.mode", Label: "Human-like delay",
	Short: "Wait a moment before replying in chat apps, like a person typing.",
	Group: GroupChatApps, Order: 30, Apply: Restart},
	Default: "off",
	Values: []Choice{
		{"off", "Off", "Reply at once."},
		{"natural", "Natural", "A short random pause."},
		{"custom", "Custom", "A pause between your own minimum and maximum."},
	}})
