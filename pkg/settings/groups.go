package settings

// Settings screen group a setting belongs to
type GroupID string

const (
	GroupModel      GroupID = "model"
	GroupAgent      GroupID = "agent"
	GroupTools      GroupID = "tools"
	GroupTerminal   GroupID = "terminal"
	GroupSafety     GroupID = "safety"
	GroupVoice      GroupID = "voice"
	GroupMedia      GroupID = "media"
	GroupAppearance GroupID = "appearance"
	GroupWorking    GroupID = "working"
	GroupMemory     GroupID = "memory"
	GroupChatApps   GroupID = "chat_apps"
	GroupSystem     GroupID = "system"
)

// One settings screen group, its title, one line summary and place
type Group struct {
	ID    GroupID `json:"id"`
	Label string  `json:"label"`
	Short string  `json:"short"`
	Order int     `json:"order"`
	// Every row of the group is advanced
	AdvancedOnly bool `json:"advanced_only,omitempty"`
}

// Groups in screen order
var groups = []Group{
	{GroupModel, "Model", "The model, how hard it thinks, and its fallback.", 1, false},
	{GroupAgent, "Agent", "How long the agent works and when it shortens chats.", 2, false},
	{GroupTools, "Tools", "Which tools the agent may use.", 3, false},
	{GroupTerminal, "Terminal", "Where the agent's shell commands run.", 4, false},
	{GroupSafety, "Safety", "What the agent asks first and what it keeps private.", 5, false},
	{GroupVoice, "Voice", "Talking to the agent and hearing its replies.", 6, false},
	{GroupMedia, "Images & media", "Pictures and video the agent can make or look at.", 7, false},
	{GroupAppearance, "Appearance", "How the terminal UI looks.", 8, false},
	{GroupWorking, "While it works", "What you see and what Enter does while it works.", 9, false},
	{GroupMemory, "Memory & sessions", "What the agent remembers and how old sessions are kept.", 10, false},
	{GroupChatApps, "Chat apps", "Chat apps, scheduled jobs and their replies.", 11, false},
	{GroupSystem, "System", "Timeouts, logging, updates and limits.", 12, true},
}

// Every group in screen order
func Groups() []Group { return append([]Group(nil), groups...) }

// Group of an id, false when unknown
func GroupOf(id GroupID) (Group, bool) {
	for _, g := range groups {
		if g.ID == id {
			return g, true
		}
	}
	return Group{}, false
}
