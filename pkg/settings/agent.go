package settings

// Tone and style preset of the agent
var Personality = Register(&String{Meta: Meta{Key: "display.personality", Label: "Personality",
	Short: "Tone and style preset the agent speaks with.",
	Help:  "Applies to every session of this profile. None uses no preset.",
	Group: GroupAgent, Order: 10, Apply: NextTurn, Slash: "personality"}})

// Tool call cap of one request
var MaxTurns = Register(&Int{Meta: Meta{Key: "agent.max_turns", Label: "Max turns",
	Short: "Stop after this many tool calls for one request.",
	Group: GroupAgent, Order: 20, Apply: NewSession},
	Null: "no limit", Min: at(1), Unit: "calls", DefaultBelow: true,
	Unset: []string{"none", "unlimited", "infinite", "infinity", "inf", "\u221e", "-1", "0"}})

// Extra instructions for coding work
var CodingInstructions = Register(&List{Meta: Meta{Key: "agent.coding_instructions", Label: "Coding instructions",
	Short: "Your own rules the agent follows when it works on code.",
	Group: GroupAgent, Order: 30, Apply: NewSession}, Whole: true})

// Summaries that shorten long chats
var AutoCompress = Register(&Bool{Meta: Meta{Key: "compression.enabled", Label: "Shorten long chats",
	Short: "Summarize older messages when a chat nears the model's limit.",
	Group: GroupAgent, Order: 40, Apply: NewSession},
	Default: true})

// Fill level that starts a summary
var CompressAt = Register(&Float{Meta: Meta{Key: "compression.threshold", Label: "Shorten at",
	Short: "How full the chat gets before older messages are summarized.",
	Help:  "A share of the model's context window: 0.5 is half full.",
	Group: GroupAgent, Order: 41, Apply: NewSession},
	Default: 0.5, Unit: "of context"})

// Text /prompt saves and shows
var CustomPrompt = Register(&String{Meta: Meta{Key: "custom_prompt", Label: "Saved prompt",
	Short: "A note /prompt keeps for you, which the agent never reads.",
	Group: GroupAgent, Order: 100, Advanced: true, Slash: "prompt"}})
