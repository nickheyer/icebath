package tuigateway

type GroupsPromoteResult struct {
	RoomID             string `json:"room_id"`
	AuthorityGatewayID string `json:"authority_gateway_id"`
	AuthorityEpoch     int64  `json:"authority_epoch"`
	PreviousGatewayID  string `json:"previous_gateway_id"`
	PreviousEpoch      int64  `json:"previous_epoch"`
	ClaimSeq           int64  `json:"claim_seq"`
	LatestSeq          int64  `json:"latest_seq"`
}

type GroupsPromoteParams struct {
	Profile *string `json:"profile,omitzero"`
	RoomID  string  `json:"room_id"`
	Confirm *bool   `json:"confirm,omitzero"`
	Reason  *string `json:"reason,omitzero"`
}

type GroupsReplicaStateResult struct {
	RoomID      string            `json:"room_id"`
	Name        string            `json:"name"`
	Members     []RoomMember      `json:"members"`
	Authority   RoomAuthority     `json:"authority"`
	LastSeq     int64             `json:"last_seq"`
	LatestSeq   int64             `json:"latest_seq"`
	EventBytes  int64             `json:"event_bytes"`
	CreatedAt   float64           `json:"created_at"`
	UpdatedAt   float64           `json:"updated_at"`
	Home        *string           `json:"home,omitzero"`
	SyncedAt    *float64          `json:"synced_at,omitzero"`
	DisbandedAt *float64          `json:"disbanded_at,omitzero"`
	Pending     *int64            `json:"pending,omitzero"`
	Errors      []RoomQueuedError `json:"errors,omitzero"`
}

type RoomQueuedError struct {
	OutboxID int64  `json:"outbox_id"`
	Op       string `json:"op"`
	Error    string `json:"error"`
}

type RoomAuthority struct {
	GatewayID string `json:"gateway_id"`
	Epoch     int64  `json:"epoch"`
}

// One seat of a room roster, older rooms keeping keys of rows written before the roster was normalised
type RoomMember struct {
	MemberID    string         `json:"member_id,omitzero"`
	Profile     string         `json:"profile,omitzero"`
	Handle      string         `json:"handle,omitzero"`
	DisplayName string         `json:"display_name,omitzero"`
	Target      *RoomTarget    `json:"target,omitzero"`
	Extra       map[string]any `json:"-" wire:"extra"`
}

// Where a seat runs, a peer gateway and the profile it serves for a seat on a peer
type RoomTarget struct {
	Kind    string         `json:"kind,omitzero"`
	Peer    string         `json:"peer,omitzero"`
	Profile string         `json:"profile,omitzero"`
	Extra   map[string]any `json:"-" wire:"extra"`
}

type GroupsReplicaStateParams struct {
	Profile *string `json:"profile,omitzero"`
	RoomID  string  `json:"room_id"`
}

type GroupsRetryResult struct {
	Retried *bool            `json:"retried,omitzero"`
	Task    *RoomTaskReceipt `json:"task,omitzero"`
	Queued  *bool            `json:"queued,omitzero"`
}

type RoomTaskReceipt struct {
	RoomID              string `json:"room_id"`
	TaskID              string `json:"task_id"`
	ThreadID            string `json:"thread_id"`
	TurnID              string `json:"turn_id"`
	Status              string `json:"status"`
	ExecutionGeneration int64  `json:"execution_generation"`
	CancelGeneration    int64  `json:"cancel_generation"`
}

type GroupsRetryParams struct {
	Profile             *string `json:"profile,omitzero"`
	RoomID              string  `json:"room_id"`
	TaskID              string  `json:"task_id"`
	MemberID            *string `json:"member_id,omitzero"`
	ExecutionGeneration *int64  `json:"execution_generation,omitzero"`
}

type GroupsApproveResult struct {
	Approved *bool          `json:"approved,omitzero"`
	Result   map[string]any `json:"result"`
	Queued   *bool          `json:"queued,omitzero"`
}

type GroupsApproveParams struct {
	Profile             *string        `json:"profile,omitzero"`
	RoomID              string         `json:"room_id"`
	MemberID            string         `json:"member_id"`
	TaskID              string         `json:"task_id"`
	ExecutionGeneration int64          `json:"execution_generation"`
	Choice              ApprovalChoice `json:"choice"`
	RequestID           string         `json:"request_id"`
}

type GroupsStopResult struct {
	Cancelled int64 `json:"cancelled"`
	Queued    *bool `json:"queued,omitzero"`
}

type GroupsStopParams struct {
	Profile  *string `json:"profile,omitzero"`
	RoomID   string  `json:"room_id"`
	CancelID *string `json:"cancel_id,omitzero"`
}

type GroupsContinueResult struct {
	Continued *bool      `json:"continued,omitzero"`
	Event     *RoomEvent `json:"event,omitzero"`
	Queued    *bool      `json:"queued,omitzero"`
}

type GroupsContinueParams struct {
	Profile           *string `json:"profile,omitzero"`
	RoomID            string  `json:"room_id"`
	DiscussionEventID string  `json:"discussion_event_id"`
}

type GroupsDisbandResult struct {
	Tombstone *RoomTombstone `json:"tombstone,omitzero"`
	Queued    *bool          `json:"queued,omitzero"`
}

type RoomTombstone struct {
	RoomID         string     `json:"room_id"`
	DisbandedAt    float64    `json:"disbanded_at"`
	Idempotent     bool       `json:"idempotent"`
	HistoryExpired *bool      `json:"history_expired,omitzero"`
	Event          *RoomEvent `json:"event,omitzero"`
}

type RoomEvent struct {
	RoomID         string         `json:"room_id"`
	Seq            int64          `json:"seq"`
	EventID        string         `json:"event_id"`
	Kind           string         `json:"kind"`
	Actor          RoomActor      `json:"actor"`
	AuthorityEpoch *int64         `json:"authority_epoch,omitzero"`
	Payload        map[string]any `json:"payload"`
	CreatedAt      float64        `json:"created_at"`
	Idempotent     bool           `json:"idempotent,omitzero"`
}

type RoomActor struct {
	Kind         string  `json:"kind"`
	ID           string  `json:"id"`
	Profile      *string `json:"profile,omitzero"`
	DisplayName  *string `json:"display_name,omitzero"`
	ConnectionID *string `json:"connection_id,omitzero"`
}

type GroupsDisbandParams struct {
	Profile  *string `json:"profile,omitzero"`
	RoomID   string  `json:"room_id"`
	CancelID *string `json:"cancel_id,omitzero"`
}

type GroupsLogResult struct {
	Events    []RoomEvent   `json:"events"`
	Cursor    int64         `json:"cursor"`
	LatestSeq int64         `json:"latest_seq"`
	HasMore   bool          `json:"has_more"`
	Authority RoomAuthority `json:"authority"`
}

type GroupsLogParams struct {
	Profile          *string `json:"profile,omitzero"`
	RoomID           string  `json:"room_id"`
	SinceSeq         *int64  `json:"since_seq,omitzero"`
	Limit            *int64  `json:"limit,omitzero"`
	IncludeDisbanded *bool   `json:"include_disbanded,omitzero"`
}

type GroupsRenameResult struct {
	Room   Room  `json:"room"`
	Queued *bool `json:"queued,omitzero"`
}

type Room struct {
	RoomID             string       `json:"room_id"`
	Name               string       `json:"name"`
	Members            []RoomMember `json:"members"`
	AuthorityGatewayID string       `json:"authority_gateway_id"`
	AuthorityEpoch     int64        `json:"authority_epoch"`
	Revision           int64        `json:"revision"`
	CreatedAt          float64      `json:"created_at"`
	UpdatedAt          float64      `json:"updated_at"`
	Idempotent         bool         `json:"idempotent,omitzero"`
	DisbandedAt        *float64     `json:"disbanded_at,omitzero"`
	LatestSeq          *int64       `json:"latest_seq,omitzero"`
	Adopted            *bool        `json:"adopted,omitzero"`
	ClaimEvent         *RoomEvent   `json:"claim_event,omitzero"`
	AuthorityClaim     *RoomEvent   `json:"authority_claim,omitzero"`
	Event              *RoomEvent   `json:"event,omitzero"`
	Replica            *bool        `json:"replica,omitzero"`
	Home               *string      `json:"home,omitzero"`
}

type GroupsRenameParams struct {
	Profile *string `json:"profile,omitzero"`
	RoomID  string  `json:"room_id"`
	EventID string  `json:"event_id"`
	Name    string  `json:"name"`
}

type GroupsSendResult struct {
	Event         *RoomEvent `json:"event,omitzero"`
	ClientEventID *string    `json:"client_event_id,omitzero"`
	Accepted      *bool      `json:"accepted,omitzero"`
	DriverStarted *bool      `json:"driver_started,omitzero"`
	Queued        *bool      `json:"queued,omitzero"`
}

type GroupsSendParams struct {
	Profile *string            `json:"profile,omitzero"`
	RoomID  string             `json:"room_id"`
	EventID *string            `json:"event_id,omitzero"`
	Payload RoomMessagePayload `json:"payload"`
}

type GroupsStateResult struct {
	Room         Room              `json:"room"`
	DriverStatus *RoomDriverStatus `json:"driver_status,omitzero"`
}

type RoomDriverStatus struct {
	Running        bool                 `json:"running"`
	Working        bool                 `json:"working"`
	Blocked        bool                 `json:"blocked"`
	Counts         map[string]int64     `json:"counts"`
	PendingActions []RoomPendingAction  `json:"pending_actions"`
	MemberActivity []RoomMemberActivity `json:"member_activity,omitzero"`
	LastError      *string              `json:"last_error,omitzero"`
	MaxContinues   *int64               `json:"max_continues,omitzero"`
	PeerRoutes     []PeerRouteStatus    `json:"peer_routes"`
}

type PeerRouteStatus struct {
	RoomID   string `json:"room_id"`
	MemberID string `json:"member_id"`
	Status   string `json:"status"`
}

type RoomMemberActivity struct {
	MemberID string `json:"member_id"`
	Status   string `json:"status"`
}

type GroupsStateParams struct {
	Profile          *string `json:"profile,omitzero"`
	RoomID           string  `json:"room_id"`
	IncludeDisbanded *bool   `json:"include_disbanded,omitzero"`
}

type GroupsCreateResult struct {
	Room Room `json:"room"`
}

type GroupsCreateParams struct {
	Profile            *string           `json:"profile,omitzero"`
	RoomID             string            `json:"room_id"`
	Name               string            `json:"name"`
	Members            []RoomMemberInput `json:"members"`
	AuthorityGatewayID *string           `json:"authority_gateway_id,omitzero"`
}

// Seat a client proposes for a new room, the gateway's roster rules deciding what it accepts
type RoomMemberInput struct {
	MemberID    string         `json:"member_id,omitzero"`
	Profile     string         `json:"profile,omitzero"`
	Handle      string         `json:"handle,omitzero"`
	DisplayName string         `json:"display_name,omitzero"`
	Target      *RoomTarget    `json:"target,omitzero"`
	Extra       map[string]any `json:"-" wire:"extra"`
}

type GroupsListResult struct {
	Rooms      []Room `json:"rooms"`
	NextOffset *int64 `json:"next_offset,omitzero"`
}

type GroupsListParams struct {
	Profile          *string `json:"profile,omitzero"`
	IncludeDisbanded *bool   `json:"include_disbanded,omitzero"`
	Limit            *int64  `json:"limit,omitzero"`
	Offset           *int64  `json:"offset,omitzero"`
}

type GroupsCapabilitiesResult struct {
	ProtocolVersion    int64          `json:"protocol_version"`
	Driver             bool           `json:"driver"`
	PersistentProcess  bool           `json:"persistent_process"`
	AuthorityGatewayID string         `json:"authority_gateway_id"`
	RoomLink           RoomLinkStatus `json:"room_link"`
	Features           []string       `json:"features"`
	Methods            []string       `json:"methods"`
	MaxLogLimit        int64          `json:"max_log_limit"`
	SeatableProfiles   []string       `json:"seatable_profiles,omitzero"`
}

type RoomLinkStatus struct {
	Enabled  bool              `json:"enabled"`
	Profile  *string           `json:"profile,omitzero"`
	Catalog  *RoomLinkCatalog  `json:"catalog,omitzero"`
	Endpoint *RoomLinkEndpoint `json:"endpoint,omitzero"`
	Reason   *string           `json:"reason,omitzero"`
}

type RoomLinkEndpoint struct {
	Available         bool    `json:"available"`
	URL               *string `json:"url,omitzero"`
	TransportSecurity *string `json:"transport_security,omitzero"`
	Reason            *string `json:"reason,omitzero"`
}

type RoomLinkCatalog struct {
	InstallationID    string              `json:"installation_id"`
	ProtocolVersions  []int64             `json:"protocol_versions"`
	LinkModes         []string            `json:"link_modes"`
	PersistentProcess bool                `json:"persistent_process"`
	Text              bool                `json:"text"`
	Attachments       bool                `json:"attachments"`
	ExecutionPolicy   RoomExecutionPolicy `json:"execution_policy"`
	CatalogDigest     string              `json:"catalog_digest"`
	Endpoint          *RoomLinkEndpoint   `json:"endpoint,omitzero"`
}

type RoomExecutionPolicy struct {
	Version         int64    `json:"version"`
	TargetProfile   string   `json:"target_profile"`
	EnabledToolsets []string `json:"enabled_toolsets"`
	ApprovalMode    string   `json:"approval_mode"`
	MaxIterations   int64    `json:"max_iterations"`
	PolicyDigest    string   `json:"policy_digest"`
}

type GroupsCapabilitiesParams struct {
	Profile *string `json:"profile,omitzero"`
}

type RoomApproveChoice string

func (RoomApproveChoice) Values() []string { return []string{"once", "deny"} }

type CanonicalRoomApproveParams struct {
	Profile             *string           `json:"profile,omitzero"`
	RoomID              string            `json:"room_id"`
	MemberID            string            `json:"member_id"`
	TaskID              string            `json:"task_id"`
	ExecutionGeneration int64             `json:"execution_generation"`
	Choice              RoomApproveChoice `json:"choice"`
	RequestID           string            `json:"request_id"`
}

type CanonicalRoomCreateParams struct {
	Profile *string           `json:"profile,omitzero"`
	RoomID  string            `json:"room_id"`
	Name    string            `json:"name"`
	Members []RoomMemberInput `json:"members"`
}

type CanonicalRoomAttemptParams struct {
	Profile             *string `json:"profile,omitzero"`
	RoomID              string  `json:"room_id"`
	MemberID            string  `json:"member_id"`
	TaskID              string  `json:"task_id"`
	ExecutionGeneration int64   `json:"execution_generation"`
}

type GroupsDiscardResult struct {
	Discarded *bool            `json:"discarded,omitzero"`
	Task      *RoomTaskReceipt `json:"task,omitzero"`
	Queued    *bool            `json:"queued,omitzero"`
}

type GroupsDiscardParams struct {
	Profile             *string `json:"profile,omitzero"`
	RoomID              string  `json:"room_id"`
	MemberID            string  `json:"member_id"`
	TaskID              string  `json:"task_id"`
	ExecutionGeneration int64   `json:"execution_generation"`
}

type GroupsShareResult struct {
	Share RoomShare `json:"share"`
}

type RoomShareState string

func (RoomShareState) Values() []string { return []string{"active", "removing"} }

type RoomShare struct {
	RoomID    string         `json:"room_id"`
	Peer      string         `json:"peer"`
	PushedSeq *int64         `json:"pushed_seq,omitzero"`
	State     RoomShareState `json:"state"`
	Error     *string        `json:"error,omitzero"`
	SyncedAt  *float64       `json:"synced_at,omitzero"`
}

type GroupsShareParams struct {
	Profile *string `json:"profile,omitzero"`
	RoomID  string  `json:"room_id"`
	Peer    string  `json:"peer"`
}

type GroupsPeersResult struct {
	Peers  []RoomPeer  `json:"peers"`
	Shares []RoomShare `json:"shares"`
}

type RoomPeer struct {
	Name string  `json:"name"`
	URL  string  `json:"url"`
	Key  bool    `json:"key"`
	Note *string `json:"note,omitzero"`
}

type GroupsPeersParams struct {
	Profile *string `json:"profile,omitzero"`
	RoomID  *string `json:"room_id,omitzero"`
}

type RoomAttachmentBytes struct {
	AttachmentID string         `json:"attachment_id"`
	Kind         string         `json:"kind"`
	Name         string         `json:"name"`
	Size         int64          `json:"size"`
	Mime         string         `json:"mime"`
	Sha256       string         `json:"sha256"`
	State        string         `json:"state"`
	CreatedAt    float64        `json:"created_at"`
	Idempotent   bool           `json:"idempotent"`
	EventID      *string        `json:"event_id,omitzero"`
	DataBase64   string         `json:"data_base64"`
	Extra        map[string]any `json:"-" wire:"extra"`
}

type GroupsAttachmentDownloadParams struct {
	Profile      *string `json:"profile,omitzero"`
	RoomID       string  `json:"room_id"`
	EventID      string  `json:"event_id"`
	AttachmentID string  `json:"attachment_id"`
}

type RoomAttachment struct {
	AttachmentID string         `json:"attachment_id"`
	Kind         string         `json:"kind"`
	Name         string         `json:"name"`
	Size         int64          `json:"size"`
	Mime         string         `json:"mime"`
	Sha256       string         `json:"sha256"`
	State        string         `json:"state"`
	CreatedAt    float64        `json:"created_at"`
	Idempotent   bool           `json:"idempotent"`
	EventID      *string        `json:"event_id,omitzero"`
	Extra        map[string]any `json:"-" wire:"extra"`
}

type GroupsAttachmentUploadParams struct {
	Profile    *string `json:"profile,omitzero"`
	RoomID     string  `json:"room_id"`
	UploadID   string  `json:"upload_id"`
	Kind       string  `json:"kind"`
	Name       string  `json:"name"`
	Mime       string  `json:"mime"`
	DataBase64 string  `json:"data_base64"`
}

type RoomPendingActionKind string

func (RoomPendingActionKind) Values() []string { return []string{"retry", "discard", "approval"} }

// What waits in a room, an attempt to retry or discard fenced on its generation, or a member's approval
// An attempt of a member on a peer gateway always shows as retry, a deferred attempt carrying the reason it waits
// A stalled room preparation shows as retry of the task prepare:<room id>, member_id naming the members not woken
type RoomPendingAction struct {
	Kind                RoomPendingActionKind `json:"kind"`
	MemberID            string                `json:"member_id"`
	TaskID              string                `json:"task_id"`
	ExecutionGeneration int64                 `json:"execution_generation,omitzero"`
	Reason              string                `json:"reason,omitzero"`
	RequestID           string                `json:"request_id,omitzero"`
	Approval            *RoomApproval         `json:"approval,omitzero"`
	Extra               map[string]any        `json:"-" wire:"extra"`
}

// Approval a member's run waits on, the command or the description it shows
type RoomApproval struct {
	Command     string         `json:"command,omitzero"`
	Description string         `json:"description,omitzero"`
	Extra       map[string]any `json:"-" wire:"extra"`
}

// One file a room message carries, uploaded before the message
type RoomAttachmentItem struct {
	AttachmentID string         `json:"attachment_id"`
	Kind         string         `json:"kind,omitzero"`
	Mime         string         `json:"mime,omitzero"`
	Name         string         `json:"name,omitzero"`
	Size         int64          `json:"size,omitzero"`
	Extra        map[string]any `json:"-" wire:"extra"`
}

// Payload of a message.user or message.member event, its text, thread and files
type RoomMessagePayload struct {
	Text        string               `json:"text,omitzero"`
	ThreadID    string               `json:"thread_id,omitzero"`
	Attachments []RoomAttachmentItem `json:"attachments,omitzero"`
	Extra       map[string]any       `json:"-" wire:"extra"`
}

// Payload of a turn event, the member it ran for, the error of a failed turn and the reason of a deferred or cancelled one
type RoomTurnPayload struct {
	MemberID string `json:"member_id,omitzero"`
	Error    string `json:"error,omitzero"`
	Reason   string `json:"reason,omitzero"`
}

// Payload of a room.renamed event
type RoomRenamedPayload struct {
	Name string `json:"name,omitzero"`
}

// Payload of an authority.claimed or authority.lost event
type RoomAuthorityPayload struct {
	AuthorityGatewayID string `json:"authority_gateway_id,omitzero"`
}

// Payload of a room log event read into the type of its kind, false when it holds another shape
func RoomPayload[T any](event RoomEvent) (T, bool) {
	var out T
	return out, Loose(event.Payload, &out) == nil
}

// Room protocol this gateway implements
var GroupsCapabilities = NewMethod[GroupsCapabilitiesParams, GroupsCapabilitiesResult]("groups.capabilities")

// Rooms this gateway hosts or holds a replica of, most recently changed first
var GroupsList = NewMethod[GroupsListParams, GroupsListResult]("groups.list")

// Creates a room idempotently, this gateway's install identity its authority
var GroupsCreate = NewMethod[GroupsCreateParams, GroupsCreateResult]("groups.create")

// Creates a room on the session authority
var CanonicalGroupsCreate = NewCanonicalMethod[CanonicalRoomCreateParams, GroupsCreateResult]("groups.create")

// One room with its replay cursor, fenced authority and live driver status
var GroupsState = NewMethod[GroupsStateParams, GroupsStateResult]("groups.state")

// Appends one user message idempotently, the actor set by the gateway
var GroupsSend = NewMethod[GroupsSendParams, GroupsSendResult]("groups.send")

// Renames a room together with its replay event
var GroupsRename = NewMethod[GroupsRenameParams, GroupsRenameResult]("groups.rename")

// Room log after since_seq, bounded by count and page bytes
var GroupsLog = NewMethod[GroupsLogParams, GroupsLogResult]("groups.log")

// Tombstones a room for good after stopping its work and revoking peer routes
var GroupsDisband = NewMethod[GroupsDisbandParams, GroupsDisbandResult]("groups.disband")

// Cancels the queued and running work of a room
var GroupsStop = NewMethod[GroupsStopParams, GroupsStopResult]("groups.stop")

// Grants a discussion paused at its cap one more budget of rounds and messages
var GroupsContinue = NewMethod[GroupsContinueParams, GroupsContinueResult]("groups.continue")

// Answers one pending approval of a local or peer member
var GroupsApprove = NewMethod[GroupsApproveParams, GroupsApproveResult]("groups.approve")

// Answers one pending approval on the session authority, once or deny
var CanonicalGroupsApprove = NewCanonicalMethod[CanonicalRoomApproveParams, GroupsApproveResult]("groups.approve")

// Retries one indeterminate or deferred room task after the user confirmed it
var GroupsRetry = NewMethod[GroupsRetryParams, GroupsRetryResult]("groups.retry")

// Retries one room task on the session authority, fenced on its generation
var CanonicalGroupsRetry = NewCanonicalMethod[CanonicalRoomAttemptParams, GroupsRetryResult]("groups.retry")

// Discards one indeterminate room task after the user confirmed it
var GroupsDiscard = NewMethod[GroupsDiscardParams, GroupsDiscardResult]("groups.discard")

// Discards one indeterminate room task on the session authority
var CanonicalGroupsDiscard = NewCanonicalMethod[GroupsDiscardParams, GroupsDiscardResult]("groups.discard")

// Coverage and authority lineage of the local replica of a room
var GroupsReplicaState = NewMethod[GroupsReplicaStateParams, GroupsReplicaStateResult]("groups.replica_state")

// Continues a replicated room on this gateway at the next epoch, confirm required
var GroupsPromote = NewMethod[GroupsPromoteParams, GroupsPromoteResult]("groups.promote")

// Stores one file for a later message, idempotent per upload id
var GroupsAttachmentUpload = NewMethod[GroupsAttachmentUploadParams, RoomAttachment]("groups.attachment.upload")

// Stores one file for a later message on the session authority
var CanonicalGroupsAttachmentUpload = NewCanonicalMethod[GroupsAttachmentUploadParams, RoomAttachment]("groups.attachment.upload")

// Reads one committed file of a room log
var GroupsAttachmentDownload = NewMethod[GroupsAttachmentDownloadParams, RoomAttachmentBytes]("groups.attachment.download")

// Reads one committed file of a room log on the session authority
var CanonicalGroupsAttachmentDownload = NewCanonicalMethod[GroupsAttachmentDownloadParams, RoomAttachmentBytes]("groups.attachment.download")

// Registered peer gateways and the peers one room is shared with
var CanonicalGroupsPeers = NewCanonicalMethod[GroupsPeersParams, GroupsPeersResult]("groups.peers")

// Shares a room this gateway hosts with a registered peer gateway
var CanonicalGroupsShare = NewCanonicalMethod[GroupsShareParams, GroupsShareResult]("groups.share")

// Stops sharing a room with a peer, which drops its replica on its next sync
var CanonicalGroupsUnshare = NewCanonicalMethod[GroupsShareParams, GroupsShareResult]("groups.unshare")
