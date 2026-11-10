package kanban

import "github.com/nickheyer/icebath/pkg/tools"

// Board routing tools, hidden from dispatcher spawned task workers
var orchestratorTools = map[string]bool{"kanban_list": true, "kanban_unblock": true}

// One kanban tool in the order the schema lists it
type kanbanTool struct {
	name    string
	schema  map[string]any
	handler func(k *kanbanCall) (string, error)
	emoji   string
}

var kanbanTools = []kanbanTool{
	{"kanban_show", kanbanShowSchema, handleShow, "\U0001F4CB"},
	{"kanban_list", kanbanListSchema, handleList, "\U0001F4CB"},
	{"kanban_complete", kanbanCompleteSchema, handleComplete, "✔"},
	{"kanban_block", kanbanBlockSchema, handleBlock, "⏸"},
	{"kanban_schedule", kanbanScheduleSchema, handleSchedule, "⏰"},
	{"kanban_request_review", kanbanRequestReviewSchema, handleRequestReview, "\U0001F440"},
	{"kanban_request_changes", kanbanRequestChangesSchema, handleRequestChanges, "↩"},
	{"kanban_heartbeat", kanbanHeartbeatSchema, handleHeartbeat, "\U0001F493"},
	{"kanban_comment", kanbanCommentSchema, handleComment, "\U0001F4AC"},
	{"kanban_attach", kanbanAttachSchema, handleAttach, "\U0001F4CE"},
	{"kanban_attach_url", kanbanAttachURLSchema, handleAttachURL, "\U0001F4CE"},
	{"kanban_attachments", kanbanAttachmentsSchema, handleAttachments, "\U0001F4CE"},
	{"kanban_create", kanbanCreateSchema, handleCreate, "➕"},
	{"kanban_unblock", kanbanUnblockSchema, handleUnblock, "▶"},
	{"kanban_link", kanbanLinkSchema, handleLink, "\U0001F517"},
}

// Registers every kanban tool on the default registry under the kanban toolset
func registerKanbanTools() {
	for _, t := range kanbanTools {
		gate := checkKanbanMode
		if orchestratorTools[t.name] {
			gate = checkKanbanOrchestratorMode
		}
		tools.Register(tools.Def{Name: t.name, Toolset: "kanban", Schema: t.schema, Handler: kanbanHandler(t.name, t.handler),
			Emoji: t.emoji, Check: gate})
	}
}

func init() { registerKanbanTools() }
