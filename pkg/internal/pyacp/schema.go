package pyacp

import (
	"reflect"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

type AuthMethod interface{ isAuthMethod() }

func (*EnvVarAuthMethod) isAuthMethod()   {}
func (*TerminalAuthMethod) isAuthMethod() {}
func (*AuthMethodAgent) isAuthMethod()    {}

type ContentBlock interface{ isContentBlock() }

func (*TextContentBlock) isContentBlock()             {}
func (*ImageContentBlock) isContentBlock()            {}
func (*AudioContentBlock) isContentBlock()            {}
func (*ResourceContentBlock) isContentBlock()         {}
func (*EmbeddedResourceContentBlock) isContentBlock() {}

type McpServer interface{ isMcpServer() }

func (*HttpMcpServer) isMcpServer()  {}
func (*SseMcpServer) isMcpServer()   {}
func (*McpServerStdio) isMcpServer() {}

type PermissionOutcome interface{ isPermissionOutcome() }

func (*DeniedOutcome) isPermissionOutcome()  {}
func (*AllowedOutcome) isPermissionOutcome() {}

type ResourceContents interface{ isResourceContents() }

func (*TextResourceContents) isResourceContents() {}
func (*BlobResourceContents) isResourceContents() {}

type SessionConfigOption interface{ isSessionConfigOption() }

func (*SessionConfigOptionSelect) isSessionConfigOption()  {}
func (*SessionConfigOptionBoolean) isSessionConfigOption() {}

type SessionUpdate interface{ isSessionUpdate() }

func (*UserMessageChunk) isSessionUpdate()        {}
func (*AgentMessageChunk) isSessionUpdate()       {}
func (*AgentThoughtChunk) isSessionUpdate()       {}
func (*ToolCallStart) isSessionUpdate()           {}
func (*ToolCallProgress) isSessionUpdate()        {}
func (*AgentPlanUpdate) isSessionUpdate()         {}
func (*AvailableCommandsUpdate) isSessionUpdate() {}
func (*CurrentModeUpdate) isSessionUpdate()       {}
func (*ConfigOptionUpdate) isSessionUpdate()      {}
func (*SessionInfoUpdate) isSessionUpdate()       {}
func (*UsageUpdate) isSessionUpdate()             {}

type ToolCallContent interface{ isToolCallContent() }

func (*ContentToolCallContent) isToolCallContent()  {}
func (*FileEditToolCallContent) isToolCallContent() {}
func (*TerminalToolCallContent) isToolCallContent() {}

type AgentCapabilities struct {
	Meta                *pyjson.Object       `acp:"_meta,py=field_meta"`
	LoadSession         *bool                `acp:"loadSession,py=load_session,def=false"`
	MCPCapabilities     *McpCapabilities     `acp:"mcpCapabilities,py=mcp_capabilities,def=model"`
	PromptCapabilities  *PromptCapabilities  `acp:"promptCapabilities,py=prompt_capabilities,def=model"`
	SessionCapabilities *SessionCapabilities `acp:"sessionCapabilities,py=session_capabilities,def=model"`
}

type AgentMessageChunk struct {
	Meta      *pyjson.Object `acp:"_meta,py=field_meta"`
	Content   ContentBlock   `acp:"content,py=content,req,union=TextContentBlock|ImageContentBlock|AudioContentBlock|ResourceContentBlock|EmbeddedResourceContentBlock"`
	MessageID *string        `acp:"messageId,py=message_id"`
	_         struct{}       `acp:"sessionUpdate,py=session_update,lit=agent_message_chunk"`
}

type AgentPlanUpdate struct {
	Meta    *pyjson.Object `acp:"_meta,py=field_meta"`
	Entries []PlanEntry    `acp:"entries,py=entries,req"`
	_       struct{}       `acp:"sessionUpdate,py=session_update,lit=plan"`
}

type AgentThoughtChunk struct {
	Meta      *pyjson.Object `acp:"_meta,py=field_meta"`
	Content   ContentBlock   `acp:"content,py=content,req,union=TextContentBlock|ImageContentBlock|AudioContentBlock|ResourceContentBlock|EmbeddedResourceContentBlock"`
	MessageID *string        `acp:"messageId,py=message_id"`
	_         struct{}       `acp:"sessionUpdate,py=session_update,lit=agent_thought_chunk"`
}

type AllowedOutcome struct {
	Meta     *pyjson.Object `acp:"_meta,py=field_meta"`
	OptionID string         `acp:"optionId,py=option_id,req"`
	_        struct{}       `acp:"outcome,py=outcome,lit=selected"`
}

type Annotations struct {
	Meta         *pyjson.Object `acp:"_meta,py=field_meta"`
	Audience     []string       `acp:"audience,py=audience,enum=assistant|user,enumtype"`
	LastModified *string        `acp:"lastModified,py=last_modified"`
	Priority     *float64       `acp:"priority,py=priority"`
}

type AudioContentBlock struct {
	Meta        *pyjson.Object `acp:"_meta,py=field_meta"`
	Annotations *Annotations   `acp:"annotations,py=annotations"`
	Data        string         `acp:"data,py=data,req"`
	MimeType    string         `acp:"mimeType,py=mime_type,req"`
	_           struct{}       `acp:"type,py=type,lit=audio"`
}

type AuthCapabilities struct {
	Meta     *pyjson.Object `acp:"_meta,py=field_meta"`
	Terminal *bool          `acp:"terminal,py=terminal,def=false"`
}

type AuthEnvVar struct {
	Meta     *pyjson.Object `acp:"_meta,py=field_meta"`
	Label    *string        `acp:"label,py=label"`
	Name     string         `acp:"name,py=name,req"`
	Optional *bool          `acp:"optional,py=optional,def=false"`
	Secret   *bool          `acp:"secret,py=secret,def=true"`
}

type AuthMethodAgent struct {
	Meta        *pyjson.Object `acp:"_meta,py=field_meta"`
	Description *string        `acp:"description,py=description"`
	ID          string         `acp:"id,py=id,req"`
	Name        string         `acp:"name,py=name,req"`
}

type AuthenticateRequest struct {
	Meta     *pyjson.Object `acp:"_meta,py=field_meta"`
	MethodID string         `acp:"methodId,py=method_id,req"`
}

type AuthenticateResponse struct {
	Meta *pyjson.Object `acp:"_meta,py=field_meta"`
}

type AvailableCommand struct {
	Meta        *pyjson.Object            `acp:"_meta,py=field_meta"`
	Description string                    `acp:"description,py=description,req"`
	Input       *UnstructuredCommandInput `acp:"input,py=input"`
	Name        string                    `acp:"name,py=name,req"`
}

type AvailableCommandsUpdate struct {
	Meta              *pyjson.Object     `acp:"_meta,py=field_meta"`
	AvailableCommands []AvailableCommand `acp:"availableCommands,py=available_commands,req"`
	_                 struct{}           `acp:"sessionUpdate,py=session_update,lit=available_commands_update"`
}

type BlobResourceContents struct {
	Meta     *pyjson.Object `acp:"_meta,py=field_meta"`
	Blob     string         `acp:"blob,py=blob,req"`
	MimeType *string        `acp:"mimeType,py=mime_type"`
	URI      string         `acp:"uri,py=uri,req"`
}

type CancelNotification struct {
	Meta      *pyjson.Object `acp:"_meta,py=field_meta"`
	SessionID string         `acp:"sessionId,py=session_id,req"`
}

type ClientCapabilities struct {
	Meta     *pyjson.Object          `acp:"_meta,py=field_meta"`
	Auth     *AuthCapabilities       `acp:"auth,py=auth,def=dict"`
	Fs       *FileSystemCapabilities `acp:"fs,py=fs,def=model"`
	Terminal *bool                   `acp:"terminal,py=terminal,def=false"`
}

type CloseSessionRequest struct {
	Meta      *pyjson.Object `acp:"_meta,py=field_meta"`
	SessionID string         `acp:"sessionId,py=session_id,req"`
}

type CloseSessionResponse struct {
	Meta *pyjson.Object `acp:"_meta,py=field_meta"`
}

type ConfigOptionUpdate struct {
	Meta          *pyjson.Object        `acp:"_meta,py=field_meta"`
	ConfigOptions []SessionConfigOption `acp:"configOptions,py=config_options,req,union=SessionConfigOptionSelect|SessionConfigOptionBoolean"`
	_             struct{}              `acp:"sessionUpdate,py=session_update,lit=config_option_update"`
}

type ContentToolCallContent struct {
	Meta    *pyjson.Object `acp:"_meta,py=field_meta"`
	Content ContentBlock   `acp:"content,py=content,req,union=TextContentBlock|ImageContentBlock|AudioContentBlock|ResourceContentBlock|EmbeddedResourceContentBlock"`
	_       struct{}       `acp:"type,py=type,lit=content"`
}

type Cost struct {
	Amount   float64 `acp:"amount,py=amount,req"`
	Currency string  `acp:"currency,py=currency,req"`
}

type CreateTerminalRequest struct {
	Meta            *pyjson.Object `acp:"_meta,py=field_meta"`
	Args            []string       `acp:"args,py=args"`
	Command         string         `acp:"command,py=command,req"`
	Cwd             *string        `acp:"cwd,py=cwd"`
	Env             []EnvVariable  `acp:"env,py=env"`
	OutputByteLimit *int           `acp:"outputByteLimit,py=output_byte_limit"`
	SessionID       string         `acp:"sessionId,py=session_id,req"`
}

type CreateTerminalResponse struct {
	Meta       *pyjson.Object `acp:"_meta,py=field_meta"`
	TerminalID string         `acp:"terminalId,py=terminal_id,req"`
}

type CurrentModeUpdate struct {
	Meta          *pyjson.Object `acp:"_meta,py=field_meta"`
	CurrentModeID string         `acp:"currentModeId,py=current_mode_id,req"`
	_             struct{}       `acp:"sessionUpdate,py=session_update,lit=current_mode_update"`
}

type DeniedOutcome struct {
	_ struct{} `acp:"outcome,py=outcome,lit=cancelled"`
}

type EmbeddedResourceContentBlock struct {
	Meta        *pyjson.Object   `acp:"_meta,py=field_meta"`
	Annotations *Annotations     `acp:"annotations,py=annotations"`
	Resource    ResourceContents `acp:"resource,py=resource,req,union=TextResourceContents|BlobResourceContents"`
	_           struct{}         `acp:"type,py=type,lit=resource"`
}

type EnvVarAuthMethod struct {
	Meta        *pyjson.Object `acp:"_meta,py=field_meta"`
	Description *string        `acp:"description,py=description"`
	ID          string         `acp:"id,py=id,req"`
	Link        *string        `acp:"link,py=link"`
	Name        string         `acp:"name,py=name,req"`
	Vars        []AuthEnvVar   `acp:"vars,py=vars,req"`
	_           struct{}       `acp:"type,py=type,lit=env_var"`
}

type EnvVariable struct {
	Meta  *pyjson.Object `acp:"_meta,py=field_meta"`
	Name  string         `acp:"name,py=name,req"`
	Value string         `acp:"value,py=value,req"`
}

type FileEditToolCallContent struct {
	Meta    *pyjson.Object `acp:"_meta,py=field_meta"`
	NewText string         `acp:"newText,py=new_text,req"`
	OldText *string        `acp:"oldText,py=old_text"`
	Path    string         `acp:"path,py=path,req"`
	_       struct{}       `acp:"type,py=type,lit=diff"`
}

type FileSystemCapabilities struct {
	Meta          *pyjson.Object `acp:"_meta,py=field_meta"`
	ReadTextFile  *bool          `acp:"readTextFile,py=read_text_file,def=false"`
	WriteTextFile *bool          `acp:"writeTextFile,py=write_text_file,def=false"`
}

type ForkSessionRequest struct {
	Meta       *pyjson.Object `acp:"_meta,py=field_meta"`
	Cwd        string         `acp:"cwd,py=cwd,req"`
	MCPServers []McpServer    `acp:"mcpServers,py=mcp_servers,union=HttpMcpServer|SseMcpServer|McpServerStdio"`
	SessionID  string         `acp:"sessionId,py=session_id,req"`
}

type ForkSessionResponse struct {
	Meta          *pyjson.Object        `acp:"_meta,py=field_meta"`
	ConfigOptions []SessionConfigOption `acp:"configOptions,py=config_options,union=SessionConfigOptionSelect|SessionConfigOptionBoolean"`
	Models        *SessionModelState    `acp:"models,py=models"`
	Modes         *SessionModeState     `acp:"modes,py=modes"`
	SessionID     string                `acp:"sessionId,py=session_id,req"`
}

type HttpHeader struct {
	Meta  *pyjson.Object `acp:"_meta,py=field_meta"`
	Name  string         `acp:"name,py=name,req"`
	Value string         `acp:"value,py=value,req"`
}

type HttpMcpServer struct {
	Meta    *pyjson.Object `acp:"_meta,py=field_meta"`
	Headers []HttpHeader   `acp:"headers,py=headers,req"`
	Name    string         `acp:"name,py=name,req"`
	URL     string         `acp:"url,py=url,req"`
	_       struct{}       `acp:"type,py=type,lit=http"`
}

type ImageContentBlock struct {
	Meta        *pyjson.Object `acp:"_meta,py=field_meta"`
	Annotations *Annotations   `acp:"annotations,py=annotations"`
	Data        string         `acp:"data,py=data,req"`
	MimeType    string         `acp:"mimeType,py=mime_type,req"`
	URI         *string        `acp:"uri,py=uri"`
	_           struct{}       `acp:"type,py=type,lit=image"`
}

type Implementation struct {
	Meta    *pyjson.Object `acp:"_meta,py=field_meta"`
	Name    string         `acp:"name,py=name,req"`
	Title   *string        `acp:"title,py=title"`
	Version string         `acp:"version,py=version,req"`
}

type InitializeRequest struct {
	Meta               *pyjson.Object      `acp:"_meta,py=field_meta"`
	ClientCapabilities *ClientCapabilities `acp:"clientCapabilities,py=client_capabilities,def=model"`
	ClientInfo         *Implementation     `acp:"clientInfo,py=client_info"`
	ProtocolVersion    int                 `acp:"protocolVersion,py=protocol_version,req"`
}

type InitializeResponse struct {
	Meta              *pyjson.Object     `acp:"_meta,py=field_meta"`
	AgentCapabilities *AgentCapabilities `acp:"agentCapabilities,py=agent_capabilities,def=model"`
	AgentInfo         *Implementation    `acp:"agentInfo,py=agent_info"`
	AuthMethods       []AuthMethod       `acp:"authMethods,py=auth_methods,def=[],union=EnvVarAuthMethod|TerminalAuthMethod|AuthMethodAgent"`
	ProtocolVersion   int                `acp:"protocolVersion,py=protocol_version,req"`
}

type KillTerminalRequest struct {
	Meta       *pyjson.Object `acp:"_meta,py=field_meta"`
	SessionID  string         `acp:"sessionId,py=session_id,req"`
	TerminalID string         `acp:"terminalId,py=terminal_id,req"`
}

type KillTerminalResponse struct {
	Meta *pyjson.Object `acp:"_meta,py=field_meta"`
}

type ListSessionsRequest struct {
	Meta   *pyjson.Object `acp:"_meta,py=field_meta"`
	Cursor *string        `acp:"cursor,py=cursor"`
	Cwd    *string        `acp:"cwd,py=cwd"`
}

type ListSessionsResponse struct {
	Meta       *pyjson.Object `acp:"_meta,py=field_meta"`
	NextCursor *string        `acp:"nextCursor,py=next_cursor"`
	Sessions   []SessionInfo  `acp:"sessions,py=sessions,req"`
}

type LoadSessionRequest struct {
	Meta       *pyjson.Object `acp:"_meta,py=field_meta"`
	Cwd        string         `acp:"cwd,py=cwd,req"`
	MCPServers []McpServer    `acp:"mcpServers,py=mcp_servers,req,union=HttpMcpServer|SseMcpServer|McpServerStdio"`
	SessionID  string         `acp:"sessionId,py=session_id,req"`
}

type LoadSessionResponse struct {
	Meta          *pyjson.Object        `acp:"_meta,py=field_meta"`
	ConfigOptions []SessionConfigOption `acp:"configOptions,py=config_options,union=SessionConfigOptionSelect|SessionConfigOptionBoolean"`
	Models        *SessionModelState    `acp:"models,py=models"`
	Modes         *SessionModeState     `acp:"modes,py=modes"`
}

type McpCapabilities struct {
	Meta *pyjson.Object `acp:"_meta,py=field_meta"`
	HTTP *bool          `acp:"http,py=http,def=false"`
	Sse  *bool          `acp:"sse,py=sse,def=false"`
}

type McpServerStdio struct {
	Meta    *pyjson.Object `acp:"_meta,py=field_meta"`
	Args    []string       `acp:"args,py=args,req"`
	Command string         `acp:"command,py=command,req"`
	Env     []EnvVariable  `acp:"env,py=env,req"`
	Name    string         `acp:"name,py=name,req"`
}

type ModelInfo struct {
	Meta        *pyjson.Object `acp:"_meta,py=field_meta"`
	Description *string        `acp:"description,py=description"`
	ModelID     string         `acp:"modelId,py=model_id,req"`
	Name        string         `acp:"name,py=name,req"`
}

type NewSessionRequest struct {
	Meta       *pyjson.Object `acp:"_meta,py=field_meta"`
	Cwd        string         `acp:"cwd,py=cwd,req"`
	MCPServers []McpServer    `acp:"mcpServers,py=mcp_servers,req,union=HttpMcpServer|SseMcpServer|McpServerStdio"`
}

type NewSessionResponse struct {
	Meta          *pyjson.Object        `acp:"_meta,py=field_meta"`
	ConfigOptions []SessionConfigOption `acp:"configOptions,py=config_options,union=SessionConfigOptionSelect|SessionConfigOptionBoolean"`
	Models        *SessionModelState    `acp:"models,py=models"`
	Modes         *SessionModeState     `acp:"modes,py=modes"`
	SessionID     string                `acp:"sessionId,py=session_id,req"`
}

type PermissionOption struct {
	Meta     *pyjson.Object `acp:"_meta,py=field_meta"`
	Kind     string         `acp:"kind,py=kind,req,enum=allow_once|allow_always|reject_once|reject_always"`
	Name     string         `acp:"name,py=name,req"`
	OptionID string         `acp:"optionId,py=option_id,req"`
}

type PlanEntry struct {
	Meta     *pyjson.Object `acp:"_meta,py=field_meta"`
	Content  string         `acp:"content,py=content,req"`
	Priority string         `acp:"priority,py=priority,req,enum=high|medium|low"`
	Status   string         `acp:"status,py=status,req,enum=pending|in_progress|completed"`
}

type PromptCapabilities struct {
	Meta            *pyjson.Object `acp:"_meta,py=field_meta"`
	Audio           *bool          `acp:"audio,py=audio,def=false"`
	EmbeddedContext *bool          `acp:"embeddedContext,py=embedded_context,def=false"`
	Image           *bool          `acp:"image,py=image,def=false"`
}

type PromptRequest struct {
	Meta      *pyjson.Object `acp:"_meta,py=field_meta"`
	MessageID *string        `acp:"messageId,py=message_id"`
	Prompt    []ContentBlock `acp:"prompt,py=prompt,req,union=TextContentBlock|ImageContentBlock|AudioContentBlock|ResourceContentBlock|EmbeddedResourceContentBlock"`
	SessionID string         `acp:"sessionId,py=session_id,req"`
}

type PromptResponse struct {
	Meta          *pyjson.Object `acp:"_meta,py=field_meta"`
	StopReason    string         `acp:"stopReason,py=stop_reason,req,enum=end_turn|max_tokens|max_turn_requests|refusal|cancelled"`
	Usage         *Usage         `acp:"usage,py=usage"`
	UserMessageID *string        `acp:"userMessageId,py=user_message_id"`
}

type ReadTextFileRequest struct {
	Meta      *pyjson.Object `acp:"_meta,py=field_meta"`
	Limit     *int           `acp:"limit,py=limit"`
	Line      *int           `acp:"line,py=line"`
	Path      string         `acp:"path,py=path,req"`
	SessionID string         `acp:"sessionId,py=session_id,req"`
}

type ReadTextFileResponse struct {
	Meta    *pyjson.Object `acp:"_meta,py=field_meta"`
	Content string         `acp:"content,py=content,req"`
}

type ReleaseTerminalRequest struct {
	Meta       *pyjson.Object `acp:"_meta,py=field_meta"`
	SessionID  string         `acp:"sessionId,py=session_id,req"`
	TerminalID string         `acp:"terminalId,py=terminal_id,req"`
}

type ReleaseTerminalResponse struct {
	Meta *pyjson.Object `acp:"_meta,py=field_meta"`
}

type RequestPermissionRequest struct {
	Meta      *pyjson.Object     `acp:"_meta,py=field_meta"`
	Options   []PermissionOption `acp:"options,py=options,req"`
	SessionID string             `acp:"sessionId,py=session_id,req"`
	ToolCall  ToolCallUpdate     `acp:"toolCall,py=tool_call,req"`
}

type RequestPermissionResponse struct {
	Meta    *pyjson.Object    `acp:"_meta,py=field_meta"`
	Outcome PermissionOutcome `acp:"outcome,py=outcome,req,union=DeniedOutcome|AllowedOutcome"`
}

type ResourceContentBlock struct {
	Meta        *pyjson.Object `acp:"_meta,py=field_meta"`
	Annotations *Annotations   `acp:"annotations,py=annotations"`
	Description *string        `acp:"description,py=description"`
	MimeType    *string        `acp:"mimeType,py=mime_type"`
	Name        string         `acp:"name,py=name,req"`
	Size        *int           `acp:"size,py=size"`
	Title       *string        `acp:"title,py=title"`
	URI         string         `acp:"uri,py=uri,req"`
	_           struct{}       `acp:"type,py=type,lit=resource_link"`
}

type ResumeSessionRequest struct {
	Meta       *pyjson.Object `acp:"_meta,py=field_meta"`
	Cwd        string         `acp:"cwd,py=cwd,req"`
	MCPServers []McpServer    `acp:"mcpServers,py=mcp_servers,union=HttpMcpServer|SseMcpServer|McpServerStdio"`
	SessionID  string         `acp:"sessionId,py=session_id,req"`
}

type ResumeSessionResponse struct {
	Meta          *pyjson.Object        `acp:"_meta,py=field_meta"`
	ConfigOptions []SessionConfigOption `acp:"configOptions,py=config_options,union=SessionConfigOptionSelect|SessionConfigOptionBoolean"`
	Models        *SessionModelState    `acp:"models,py=models"`
	Modes         *SessionModeState     `acp:"modes,py=modes"`
}

type SessionCapabilities struct {
	Meta   *pyjson.Object             `acp:"_meta,py=field_meta"`
	Close  *SessionCloseCapabilities  `acp:"close,py=close"`
	Fork   *SessionForkCapabilities   `acp:"fork,py=fork"`
	List   *SessionListCapabilities   `acp:"list,py=list"`
	Resume *SessionResumeCapabilities `acp:"resume,py=resume"`
}

type SessionCloseCapabilities struct {
	Meta *pyjson.Object `acp:"_meta,py=field_meta"`
}

type SessionConfigOptionBoolean struct {
	CurrentValue bool           `acp:"currentValue,py=current_value,req"`
	Meta         *pyjson.Object `acp:"_meta,py=field_meta"`
	Category     *string        `acp:"category,py=category"`
	Description  *string        `acp:"description,py=description"`
	ID           string         `acp:"id,py=id,req"`
	Name         string         `acp:"name,py=name,req"`
	_            struct{}       `acp:"type,py=type,lit=boolean"`
}

type SessionConfigOptionSelect struct {
	CurrentValue string         `acp:"currentValue,py=current_value,req"`
	Options      any            `acp:"options,py=options,req,union=list:SessionConfigSelectOption|list:SessionConfigSelectGroup"`
	Meta         *pyjson.Object `acp:"_meta,py=field_meta"`
	Category     *string        `acp:"category,py=category"`
	Description  *string        `acp:"description,py=description"`
	ID           string         `acp:"id,py=id,req"`
	Name         string         `acp:"name,py=name,req"`
	_            struct{}       `acp:"type,py=type,lit=select"`
}

type SessionConfigSelectGroup struct {
	Meta    *pyjson.Object              `acp:"_meta,py=field_meta"`
	Group   string                      `acp:"group,py=group,req"`
	Name    string                      `acp:"name,py=name,req"`
	Options []SessionConfigSelectOption `acp:"options,py=options,req"`
}

type SessionConfigSelectOption struct {
	Meta        *pyjson.Object `acp:"_meta,py=field_meta"`
	Description *string        `acp:"description,py=description"`
	Name        string         `acp:"name,py=name,req"`
	Value       string         `acp:"value,py=value,req"`
}

type SessionForkCapabilities struct {
	Meta *pyjson.Object `acp:"_meta,py=field_meta"`
}

type SessionInfo struct {
	Meta      *pyjson.Object `acp:"_meta,py=field_meta"`
	Cwd       string         `acp:"cwd,py=cwd,req"`
	SessionID string         `acp:"sessionId,py=session_id,req"`
	Title     *string        `acp:"title,py=title"`
	UpdatedAt *string        `acp:"updatedAt,py=updated_at"`
}

type SessionInfoUpdate struct {
	Meta      *pyjson.Object `acp:"_meta,py=field_meta"`
	Title     *string        `acp:"title,py=title"`
	UpdatedAt *string        `acp:"updatedAt,py=updated_at"`
	_         struct{}       `acp:"sessionUpdate,py=session_update,lit=session_info_update"`
}

type SessionListCapabilities struct {
	Meta *pyjson.Object `acp:"_meta,py=field_meta"`
}

type SessionMode struct {
	Meta        *pyjson.Object `acp:"_meta,py=field_meta"`
	Description *string        `acp:"description,py=description"`
	ID          string         `acp:"id,py=id,req"`
	Name        string         `acp:"name,py=name,req"`
}

type SessionModeState struct {
	Meta           *pyjson.Object `acp:"_meta,py=field_meta"`
	AvailableModes []SessionMode  `acp:"availableModes,py=available_modes,req"`
	CurrentModeID  string         `acp:"currentModeId,py=current_mode_id,req"`
}

type SessionModelState struct {
	Meta            *pyjson.Object `acp:"_meta,py=field_meta"`
	AvailableModels []ModelInfo    `acp:"availableModels,py=available_models,req"`
	CurrentModelID  string         `acp:"currentModelId,py=current_model_id,req"`
}

type SessionNotification struct {
	Meta      *pyjson.Object `acp:"_meta,py=field_meta"`
	SessionID string         `acp:"sessionId,py=session_id,req"`
	Update    SessionUpdate  `acp:"update,py=update,req,union=UserMessageChunk|AgentMessageChunk|AgentThoughtChunk|ToolCallStart|ToolCallProgress|AgentPlanUpdate|AvailableCommandsUpdate|CurrentModeUpdate|ConfigOptionUpdate|SessionInfoUpdate|UsageUpdate"`
}

type SessionResumeCapabilities struct {
	Meta *pyjson.Object `acp:"_meta,py=field_meta"`
}

type SetSessionConfigOptionBooleanRequest struct {
	Meta      *pyjson.Object `acp:"_meta,py=field_meta"`
	ConfigID  string         `acp:"configId,py=config_id,req"`
	SessionID string         `acp:"sessionId,py=session_id,req"`
	_         struct{}       `acp:"type,py=type,lit=boolean"`
	Value     bool           `acp:"value,py=value,req"`
}

type SetSessionConfigOptionResponse struct {
	Meta          *pyjson.Object        `acp:"_meta,py=field_meta"`
	ConfigOptions []SessionConfigOption `acp:"configOptions,py=config_options,req,union=SessionConfigOptionSelect|SessionConfigOptionBoolean"`
}

type SetSessionConfigOptionSelectRequest struct {
	Meta      *pyjson.Object `acp:"_meta,py=field_meta"`
	ConfigID  string         `acp:"configId,py=config_id,req"`
	SessionID string         `acp:"sessionId,py=session_id,req"`
	Value     string         `acp:"value,py=value,req"`
}

type SetSessionModeRequest struct {
	Meta      *pyjson.Object `acp:"_meta,py=field_meta"`
	ModeID    string         `acp:"modeId,py=mode_id,req"`
	SessionID string         `acp:"sessionId,py=session_id,req"`
}

type SetSessionModeResponse struct {
	Meta *pyjson.Object `acp:"_meta,py=field_meta"`
}

type SetSessionModelRequest struct {
	Meta      *pyjson.Object `acp:"_meta,py=field_meta"`
	ModelID   string         `acp:"modelId,py=model_id,req"`
	SessionID string         `acp:"sessionId,py=session_id,req"`
}

type SetSessionModelResponse struct {
	Meta *pyjson.Object `acp:"_meta,py=field_meta"`
}

type SseMcpServer struct {
	Meta    *pyjson.Object `acp:"_meta,py=field_meta"`
	Headers []HttpHeader   `acp:"headers,py=headers,req"`
	Name    string         `acp:"name,py=name,req"`
	URL     string         `acp:"url,py=url,req"`
	_       struct{}       `acp:"type,py=type,lit=sse"`
}

type TerminalAuthMethod struct {
	Meta        *pyjson.Object `acp:"_meta,py=field_meta"`
	Args        []string       `acp:"args,py=args"`
	Description *string        `acp:"description,py=description"`
	Env         *pyjson.Object `acp:"env,py=env"`
	ID          string         `acp:"id,py=id,req"`
	Name        string         `acp:"name,py=name,req"`
	_           struct{}       `acp:"type,py=type,lit=terminal"`
}

type TerminalExitStatus struct {
	Meta     *pyjson.Object `acp:"_meta,py=field_meta"`
	ExitCode *int           `acp:"exitCode,py=exit_code"`
	Signal   *string        `acp:"signal,py=signal"`
}

type TerminalOutputRequest struct {
	Meta       *pyjson.Object `acp:"_meta,py=field_meta"`
	SessionID  string         `acp:"sessionId,py=session_id,req"`
	TerminalID string         `acp:"terminalId,py=terminal_id,req"`
}

type TerminalOutputResponse struct {
	Meta       *pyjson.Object      `acp:"_meta,py=field_meta"`
	ExitStatus *TerminalExitStatus `acp:"exitStatus,py=exit_status"`
	Output     string              `acp:"output,py=output,req"`
	Truncated  bool                `acp:"truncated,py=truncated,req"`
}

type TerminalToolCallContent struct {
	Meta       *pyjson.Object `acp:"_meta,py=field_meta"`
	TerminalID string         `acp:"terminalId,py=terminal_id,req"`
	_          struct{}       `acp:"type,py=type,lit=terminal"`
}

type TextContentBlock struct {
	Meta        *pyjson.Object `acp:"_meta,py=field_meta"`
	Annotations *Annotations   `acp:"annotations,py=annotations"`
	Text        string         `acp:"text,py=text,req"`
	_           struct{}       `acp:"type,py=type,lit=text"`
}

type TextResourceContents struct {
	Meta     *pyjson.Object `acp:"_meta,py=field_meta"`
	MimeType *string        `acp:"mimeType,py=mime_type"`
	Text     string         `acp:"text,py=text,req"`
	URI      string         `acp:"uri,py=uri,req"`
}

type ToolCallLocation struct {
	Meta *pyjson.Object `acp:"_meta,py=field_meta"`
	Line *int           `acp:"line,py=line"`
	Path string         `acp:"path,py=path,req"`
}

type ToolCallProgress struct {
	Meta       *pyjson.Object     `acp:"_meta,py=field_meta"`
	Content    []ToolCallContent  `acp:"content,py=content,union=ContentToolCallContent|FileEditToolCallContent|TerminalToolCallContent"`
	Kind       *string            `acp:"kind,py=kind,enum=read|edit|delete|move|search|execute|think|fetch|switch_mode|other"`
	Locations  []ToolCallLocation `acp:"locations,py=locations"`
	RawInput   any                `acp:"rawInput,py=raw_input"`
	RawOutput  any                `acp:"rawOutput,py=raw_output"`
	Status     *string            `acp:"status,py=status,enum=pending|in_progress|completed|failed"`
	Title      *string            `acp:"title,py=title"`
	ToolCallID string             `acp:"toolCallId,py=tool_call_id,req"`
	_          struct{}           `acp:"sessionUpdate,py=session_update,lit=tool_call_update"`
}

type ToolCallStart struct {
	Meta       *pyjson.Object     `acp:"_meta,py=field_meta"`
	Content    []ToolCallContent  `acp:"content,py=content,union=ContentToolCallContent|FileEditToolCallContent|TerminalToolCallContent"`
	Kind       *string            `acp:"kind,py=kind,enum=read|edit|delete|move|search|execute|think|fetch|switch_mode|other"`
	Locations  []ToolCallLocation `acp:"locations,py=locations"`
	RawInput   any                `acp:"rawInput,py=raw_input"`
	RawOutput  any                `acp:"rawOutput,py=raw_output"`
	Status     *string            `acp:"status,py=status,enum=pending|in_progress|completed|failed"`
	Title      string             `acp:"title,py=title,req"`
	ToolCallID string             `acp:"toolCallId,py=tool_call_id,req"`
	_          struct{}           `acp:"sessionUpdate,py=session_update,lit=tool_call"`
}

type ToolCallUpdate struct {
	Meta       *pyjson.Object     `acp:"_meta,py=field_meta"`
	Content    []ToolCallContent  `acp:"content,py=content,union=ContentToolCallContent|FileEditToolCallContent|TerminalToolCallContent"`
	Kind       *string            `acp:"kind,py=kind,enum=read|edit|delete|move|search|execute|think|fetch|switch_mode|other"`
	Locations  []ToolCallLocation `acp:"locations,py=locations"`
	RawInput   any                `acp:"rawInput,py=raw_input"`
	RawOutput  any                `acp:"rawOutput,py=raw_output"`
	Status     *string            `acp:"status,py=status,enum=pending|in_progress|completed|failed"`
	Title      *string            `acp:"title,py=title"`
	ToolCallID string             `acp:"toolCallId,py=tool_call_id,req"`
}

type UnstructuredCommandInput struct {
	Meta *pyjson.Object `acp:"_meta,py=field_meta"`
	Hint string         `acp:"hint,py=hint,req"`
}

type Usage struct {
	CachedReadTokens  *int `acp:"cachedReadTokens,py=cached_read_tokens"`
	CachedWriteTokens *int `acp:"cachedWriteTokens,py=cached_write_tokens"`
	InputTokens       int  `acp:"inputTokens,py=input_tokens,req"`
	OutputTokens      int  `acp:"outputTokens,py=output_tokens,req"`
	ThoughtTokens     *int `acp:"thoughtTokens,py=thought_tokens"`
	TotalTokens       int  `acp:"totalTokens,py=total_tokens,req"`
}

type UsageUpdate struct {
	Meta *pyjson.Object `acp:"_meta,py=field_meta"`
	Cost *Cost          `acp:"cost,py=cost"`
	Size int            `acp:"size,py=size,req"`
	Used int            `acp:"used,py=used,req"`
	_    struct{}       `acp:"sessionUpdate,py=session_update,lit=usage_update"`
}

type UserMessageChunk struct {
	Meta      *pyjson.Object `acp:"_meta,py=field_meta"`
	Content   ContentBlock   `acp:"content,py=content,req,union=TextContentBlock|ImageContentBlock|AudioContentBlock|ResourceContentBlock|EmbeddedResourceContentBlock"`
	MessageID *string        `acp:"messageId,py=message_id"`
	_         struct{}       `acp:"sessionUpdate,py=session_update,lit=user_message_chunk"`
}

type WaitForTerminalExitRequest struct {
	Meta       *pyjson.Object `acp:"_meta,py=field_meta"`
	SessionID  string         `acp:"sessionId,py=session_id,req"`
	TerminalID string         `acp:"terminalId,py=terminal_id,req"`
}

type WaitForTerminalExitResponse struct {
	Meta     *pyjson.Object `acp:"_meta,py=field_meta"`
	ExitCode *int           `acp:"exitCode,py=exit_code"`
	Signal   *string        `acp:"signal,py=signal"`
}

type WriteTextFileRequest struct {
	Meta      *pyjson.Object `acp:"_meta,py=field_meta"`
	Content   string         `acp:"content,py=content,req"`
	Path      string         `acp:"path,py=path,req"`
	SessionID string         `acp:"sessionId,py=session_id,req"`
}

type WriteTextFileResponse struct {
	Meta *pyjson.Object `acp:"_meta,py=field_meta"`
}

var schemaTypes = map[string]reflect.Type{
	"AgentCapabilities":                    reflect.TypeFor[AgentCapabilities](),
	"AgentMessageChunk":                    reflect.TypeFor[AgentMessageChunk](),
	"AgentPlanUpdate":                      reflect.TypeFor[AgentPlanUpdate](),
	"AgentThoughtChunk":                    reflect.TypeFor[AgentThoughtChunk](),
	"AllowedOutcome":                       reflect.TypeFor[AllowedOutcome](),
	"Annotations":                          reflect.TypeFor[Annotations](),
	"AudioContentBlock":                    reflect.TypeFor[AudioContentBlock](),
	"AuthCapabilities":                     reflect.TypeFor[AuthCapabilities](),
	"AuthEnvVar":                           reflect.TypeFor[AuthEnvVar](),
	"AuthMethodAgent":                      reflect.TypeFor[AuthMethodAgent](),
	"AuthenticateRequest":                  reflect.TypeFor[AuthenticateRequest](),
	"AuthenticateResponse":                 reflect.TypeFor[AuthenticateResponse](),
	"AvailableCommand":                     reflect.TypeFor[AvailableCommand](),
	"AvailableCommandsUpdate":              reflect.TypeFor[AvailableCommandsUpdate](),
	"BlobResourceContents":                 reflect.TypeFor[BlobResourceContents](),
	"CancelNotification":                   reflect.TypeFor[CancelNotification](),
	"ClientCapabilities":                   reflect.TypeFor[ClientCapabilities](),
	"CloseSessionRequest":                  reflect.TypeFor[CloseSessionRequest](),
	"CloseSessionResponse":                 reflect.TypeFor[CloseSessionResponse](),
	"ConfigOptionUpdate":                   reflect.TypeFor[ConfigOptionUpdate](),
	"ContentToolCallContent":               reflect.TypeFor[ContentToolCallContent](),
	"Cost":                                 reflect.TypeFor[Cost](),
	"CreateTerminalRequest":                reflect.TypeFor[CreateTerminalRequest](),
	"CreateTerminalResponse":               reflect.TypeFor[CreateTerminalResponse](),
	"CurrentModeUpdate":                    reflect.TypeFor[CurrentModeUpdate](),
	"DeniedOutcome":                        reflect.TypeFor[DeniedOutcome](),
	"EmbeddedResourceContentBlock":         reflect.TypeFor[EmbeddedResourceContentBlock](),
	"EnvVarAuthMethod":                     reflect.TypeFor[EnvVarAuthMethod](),
	"EnvVariable":                          reflect.TypeFor[EnvVariable](),
	"FileEditToolCallContent":              reflect.TypeFor[FileEditToolCallContent](),
	"FileSystemCapabilities":               reflect.TypeFor[FileSystemCapabilities](),
	"ForkSessionRequest":                   reflect.TypeFor[ForkSessionRequest](),
	"ForkSessionResponse":                  reflect.TypeFor[ForkSessionResponse](),
	"HttpHeader":                           reflect.TypeFor[HttpHeader](),
	"HttpMcpServer":                        reflect.TypeFor[HttpMcpServer](),
	"ImageContentBlock":                    reflect.TypeFor[ImageContentBlock](),
	"Implementation":                       reflect.TypeFor[Implementation](),
	"InitializeRequest":                    reflect.TypeFor[InitializeRequest](),
	"InitializeResponse":                   reflect.TypeFor[InitializeResponse](),
	"KillTerminalRequest":                  reflect.TypeFor[KillTerminalRequest](),
	"KillTerminalResponse":                 reflect.TypeFor[KillTerminalResponse](),
	"ListSessionsRequest":                  reflect.TypeFor[ListSessionsRequest](),
	"ListSessionsResponse":                 reflect.TypeFor[ListSessionsResponse](),
	"LoadSessionRequest":                   reflect.TypeFor[LoadSessionRequest](),
	"LoadSessionResponse":                  reflect.TypeFor[LoadSessionResponse](),
	"McpCapabilities":                      reflect.TypeFor[McpCapabilities](),
	"McpServerStdio":                       reflect.TypeFor[McpServerStdio](),
	"ModelInfo":                            reflect.TypeFor[ModelInfo](),
	"NewSessionRequest":                    reflect.TypeFor[NewSessionRequest](),
	"NewSessionResponse":                   reflect.TypeFor[NewSessionResponse](),
	"PermissionOption":                     reflect.TypeFor[PermissionOption](),
	"PlanEntry":                            reflect.TypeFor[PlanEntry](),
	"PromptCapabilities":                   reflect.TypeFor[PromptCapabilities](),
	"PromptRequest":                        reflect.TypeFor[PromptRequest](),
	"PromptResponse":                       reflect.TypeFor[PromptResponse](),
	"ReadTextFileRequest":                  reflect.TypeFor[ReadTextFileRequest](),
	"ReadTextFileResponse":                 reflect.TypeFor[ReadTextFileResponse](),
	"ReleaseTerminalRequest":               reflect.TypeFor[ReleaseTerminalRequest](),
	"ReleaseTerminalResponse":              reflect.TypeFor[ReleaseTerminalResponse](),
	"RequestPermissionRequest":             reflect.TypeFor[RequestPermissionRequest](),
	"RequestPermissionResponse":            reflect.TypeFor[RequestPermissionResponse](),
	"ResourceContentBlock":                 reflect.TypeFor[ResourceContentBlock](),
	"ResumeSessionRequest":                 reflect.TypeFor[ResumeSessionRequest](),
	"ResumeSessionResponse":                reflect.TypeFor[ResumeSessionResponse](),
	"SessionCapabilities":                  reflect.TypeFor[SessionCapabilities](),
	"SessionCloseCapabilities":             reflect.TypeFor[SessionCloseCapabilities](),
	"SessionConfigOptionBoolean":           reflect.TypeFor[SessionConfigOptionBoolean](),
	"SessionConfigOptionSelect":            reflect.TypeFor[SessionConfigOptionSelect](),
	"SessionConfigSelectGroup":             reflect.TypeFor[SessionConfigSelectGroup](),
	"SessionConfigSelectOption":            reflect.TypeFor[SessionConfigSelectOption](),
	"SessionForkCapabilities":              reflect.TypeFor[SessionForkCapabilities](),
	"SessionInfo":                          reflect.TypeFor[SessionInfo](),
	"SessionInfoUpdate":                    reflect.TypeFor[SessionInfoUpdate](),
	"SessionListCapabilities":              reflect.TypeFor[SessionListCapabilities](),
	"SessionMode":                          reflect.TypeFor[SessionMode](),
	"SessionModeState":                     reflect.TypeFor[SessionModeState](),
	"SessionModelState":                    reflect.TypeFor[SessionModelState](),
	"SessionNotification":                  reflect.TypeFor[SessionNotification](),
	"SessionResumeCapabilities":            reflect.TypeFor[SessionResumeCapabilities](),
	"SetSessionConfigOptionBooleanRequest": reflect.TypeFor[SetSessionConfigOptionBooleanRequest](),
	"SetSessionConfigOptionResponse":       reflect.TypeFor[SetSessionConfigOptionResponse](),
	"SetSessionConfigOptionSelectRequest":  reflect.TypeFor[SetSessionConfigOptionSelectRequest](),
	"SetSessionModeRequest":                reflect.TypeFor[SetSessionModeRequest](),
	"SetSessionModeResponse":               reflect.TypeFor[SetSessionModeResponse](),
	"SetSessionModelRequest":               reflect.TypeFor[SetSessionModelRequest](),
	"SetSessionModelResponse":              reflect.TypeFor[SetSessionModelResponse](),
	"SseMcpServer":                         reflect.TypeFor[SseMcpServer](),
	"TerminalAuthMethod":                   reflect.TypeFor[TerminalAuthMethod](),
	"TerminalExitStatus":                   reflect.TypeFor[TerminalExitStatus](),
	"TerminalOutputRequest":                reflect.TypeFor[TerminalOutputRequest](),
	"TerminalOutputResponse":               reflect.TypeFor[TerminalOutputResponse](),
	"TerminalToolCallContent":              reflect.TypeFor[TerminalToolCallContent](),
	"TextContentBlock":                     reflect.TypeFor[TextContentBlock](),
	"TextResourceContents":                 reflect.TypeFor[TextResourceContents](),
	"ToolCallLocation":                     reflect.TypeFor[ToolCallLocation](),
	"ToolCallProgress":                     reflect.TypeFor[ToolCallProgress](),
	"ToolCallStart":                        reflect.TypeFor[ToolCallStart](),
	"ToolCallUpdate":                       reflect.TypeFor[ToolCallUpdate](),
	"UnstructuredCommandInput":             reflect.TypeFor[UnstructuredCommandInput](),
	"Usage":                                reflect.TypeFor[Usage](),
	"UsageUpdate":                          reflect.TypeFor[UsageUpdate](),
	"UserMessageChunk":                     reflect.TypeFor[UserMessageChunk](),
	"WaitForTerminalExitRequest":           reflect.TypeFor[WaitForTerminalExitRequest](),
	"WaitForTerminalExitResponse":          reflect.TypeFor[WaitForTerminalExitResponse](),
	"WriteTextFileRequest":                 reflect.TypeFor[WriteTextFileRequest](),
	"WriteTextFileResponse":                reflect.TypeFor[WriteTextFileResponse](),
}
