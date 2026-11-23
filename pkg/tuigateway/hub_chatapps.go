package tuigateway

// One messaging platform a profile set up, whether it is on and what the background service last said about it
type ChatAppPlatform struct {
	Platform string `json:"platform"`
	Enabled  bool   `json:"enabled"`
	// connected, retrying, failed or another word the background service wrote, empty when it never reported the platform
	State string `json:"state,omitzero"`
	Error string `json:"error,omitzero"`
	// Chat replies and scheduled jobs go to when no chat is named
	Home string `json:"home,omitzero"`
	// Chats the platform found, as send targets
	Targets []ChatAppTarget `json:"targets,omitzero"`
}

// One chat a message can be sent to, the target the send command takes and the name it shows
type ChatAppTarget struct {
	Target string `json:"target"`
	Label  string `json:"label"`
}

// Pairing request a person sent a bot, waiting for approval
type ChatAppPairing struct {
	Platform  string `json:"platform"`
	RequestID string `json:"request_id,omitzero"`
	UserID    string `json:"user_id"`
	UserName  string `json:"user_name,omitzero"`
	Minutes   int64  `json:"minutes"`
}

// Person a pairing approved, who may talk to the bot
type ChatAppUser struct {
	Platform string `json:"platform"`
	UserID   string `json:"user_id"`
	UserName string `json:"user_name,omitzero"`
}

// One webhook route that starts the agent or delivers a message when something posts to it
type ChatAppWebhook struct {
	Name        string `json:"name"`
	Description string `json:"description,omitzero"`
	Events      string `json:"events,omitzero"`
	Prompt      string `json:"prompt,omitzero"`
	Skills      string `json:"skills,omitzero"`
	Deliver     string `json:"deliver,omitzero"`
	DeliverChat string `json:"deliver_chat_id,omitzero"`
	DeliverOnly bool   `json:"deliver_only,omitzero"`
	URL         string `json:"url,omitzero"`
	Created     string `json:"created_at,omitzero"`
}

// Another icebath on another machine whose agents can be messaged
type ChatAppPeer struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Note   string `json:"note,omitzero"`
	KeySet bool   `json:"key_set,omitzero"`
	// Variable of the home's .env holding the peer's key
	KeyEnv string `json:"key_env"`
}

type ChatAppsListParams struct {
	Profile *string `json:"profile,omitzero"`
}

// Everything the chat apps screen shows for a profile
type ChatAppsListResult struct {
	Platforms []ChatAppPlatform `json:"platforms"`
	Pending   []ChatAppPairing  `json:"pending"`
	Approved  []ChatAppUser     `json:"approved"`
	// Platforms that refuse approvals for a while after too many wrong codes
	LockedOut       []string         `json:"locked_out,omitzero"`
	WebhooksEnabled bool             `json:"webhooks_enabled"`
	Webhooks        []ChatAppWebhook `json:"webhooks"`
	Peers           []ChatAppPeer    `json:"peers"`
	// Whether the background service is installed to start with the computer
	ServiceInstalled bool `json:"service_installed"`
}

// Pairing change: approve a request id or code, revoke an approved person, or clear the pending requests of a platform
type ChatAppsPairingParams struct {
	Profile  *string `json:"profile,omitzero"`
	Action   string  `json:"action"`
	Platform string  `json:"platform"`
	Code     string  `json:"code,omitzero"`
	UserID   string  `json:"user_id,omitzero"`
}

// Webhook change: add or replace a route, remove one, or post a test payload to it
type ChatAppsWebhookParams struct {
	Profile     *string `json:"profile,omitzero"`
	Action      string  `json:"action"`
	Name        string  `json:"name"`
	Prompt      string  `json:"prompt,omitzero"`
	Events      string  `json:"events,omitzero"`
	Description string  `json:"description,omitzero"`
	Skills      string  `json:"skills,omitzero"`
	Deliver     string  `json:"deliver,omitzero"`
	DeliverChat string  `json:"deliver_chat_id,omitzero"`
	DeliverOnly bool    `json:"deliver_only,omitzero"`
	Payload     string  `json:"payload,omitzero"`
}

// Peer change: add or update one, or remove one
type ChatAppsPeerParams struct {
	Profile *string `json:"profile,omitzero"`
	Action  string  `json:"action"`
	Name    string  `json:"name"`
	URL     string  `json:"url,omitzero"`
	Key     string  `json:"key,omitzero"`
	Note    string  `json:"note,omitzero"`
}

// Message to send to a chat of a configured platform
type ChatAppsSendParams struct {
	Profile *string `json:"profile,omitzero"`
	Target  string  `json:"target"`
	Message string  `json:"message"`
	Subject string  `json:"subject,omitzero"`
}

// What a chat apps change printed, the same lines its command prints in a terminal
type ChatAppsResult struct {
	Lines []string `json:"lines"`
}

// Platforms, pairing, webhooks and peers of a profile, with whether the background service starts with the computer
var ChatAppsList = NewMethod[ChatAppsListParams, ChatAppsListResult]("chatapps.list")

// Approves or revokes who may talk to a bot
var ChatAppsPairing = NewMethod[ChatAppsPairingParams, ChatAppsResult]("chatapps.pairing")

// Adds, removes or tests a webhook route
var ChatAppsWebhook = NewMethod[ChatAppsWebhookParams, ChatAppsResult]("chatapps.webhook")

// Adds or removes a peer icebath
var ChatAppsPeer = NewMethod[ChatAppsPeerParams, ChatAppsResult]("chatapps.peer")

// Sends a message to a chat of a configured platform
var ChatAppsSend = NewMethod[ChatAppsSendParams, ChatAppsResult]("chatapps.send")
