package settings

// Who may start a direct chat with a chat app that checks access itself
var whatsappPolicyChoices = []Choice{{Value: "pairing", Label: "Pairing", Meaning: "Strangers get a code you approve."}, {Value: "allowlist", Label: "Allow list", Meaning: "Only the allowed people."}, {Value: "open", Label: "Open", Meaning: "Anyone, when Allow everyone is on too."}, {Value: "disabled", Label: "Off", Meaning: "Nobody."}}

// Who may message the bot directly or in a group
var chatPolicyChoices = []Choice{
	{Value: "pairing", Label: "Pairing", Meaning: "Unknown people ask, and you approve them."},
	{Value: "allowlist", Label: "Allowed list", Meaning: "Only the people or chats listed."},
	{Value: "open", Label: "Anyone", Meaning: "Everyone may use the bot."},
	{Value: "disabled", Label: "Nobody", Meaning: "These messages are ignored."},
}
