package gatewayrun

import (
	"context"
	"sync"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/state"
)

// Unbounded thread safe FIFO whose put never blocks, the queue.Queue of hermes
type turnQueue struct {
	mu    sync.Mutex
	items []any
}

// Empty queue
func newTurnQueue() *turnQueue { return &turnQueue{} }

// Appends item at the tail
func (q *turnQueue) put(item any) {
	q.mu.Lock()
	q.items = append(q.items, item)
	q.mu.Unlock()
}

// Head item and true, or false when the queue is empty
func (q *turnQueue) getNowait() (any, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return nil, false
	}
	item := q.items[0]
	q.items = q.items[1:]
	return item, true
}

// Whether no item waits
func (q *turnQueue) empty() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items) == 0
}

// Per turn context shared between the agent run and its TurnRunner, the message the one rebindable field
type TurnContext struct {
	source                          *gateway.SessionSource
	replyExpected                   *bool
	scheduledHeartbeat              bool
	runStillCurrent                 func() bool
	liveStatusAdapter               gateway.PlatformAdapter
	liveStatusMode                  string
	thinkingEnabled                 bool
	progressMode                    string
	progressGrouping                string
	toolProgressEnabled             bool
	progressQueue                   *turnQueue
	logQueue                        *turnQueue
	lastProgressMsg                 *string
	lastTool                        *string
	lastWasTerminalBlock            bool
	repeatCount                     int
	longToolHintFired               bool
	agentHolder                     *agent.AIAgent
	awakeIdle                       *state.AwakeIdleMeter
	longToolThresholdS              float64
	cleanupProgress                 bool
	cleanupMsgIDs                   []string
	progressMetadata                map[string]any
	progressReplyTo                 any
	message                         *string
	history                         []any
	contextPrompt                   *string
	channelPrompt                   *string
	sessionID                       *string
	sessionKey                      *string
	runGeneration                   *int
	processTaskID                   string
	processBaseline                 map[string]struct{}
	interruptDepth                  int
	eventMessageID                  *string
	inboundMessageID                *string
	moaConfig                       map[string]any
	titleUserMessage                *string
	persistUserMessage              any
	persistUserTimestamp            *float64
	persistUserDisplayKind          *string
	persistUserDisplayMetadata      map[string]any
	userConfig                      map[string]any
	muteNotificationReply           bool
	voiceTurn                       bool
	enabledToolsets                 []string
	disabledToolsets                []string
	logModeEnabled                  bool
	interimAssistantMessagesEnabled bool
	needsProgressQueue              bool
	aiAgent                         any
	resolveDisplaySetting           func(ctx context.Context, userConfig map[string]any, platformKey, setting string, fallback any) any
	resultHolder                    map[string]any
	toolsHolder                     any
	streamConsumerHolder            any
	streamingTTSConsumerHolder      any
	voiceAckFired                   bool
	voiceAckGuild                   *int
	voiceAckLoop                    gateway.EventLoop
	loopForStep                     gateway.EventLoop
	hooksRef                        *ProfileHookRegistries
	statusAdapter                   gateway.PlatformAdapter
	statusChatID                    *string
	statusThreadMetadata            map[string]any
	progressCallback                any
	voiceAckCallback                any
	stepCallbackSync                any
	eventCallbackSync               any
	statusCallbackSync              any
	nativeSlackTaskCards            bool
	nativeToolStartCallback         any
	nativeToolCompleteCallback      any
}

// Context carrying the dataclass defaults of every field
func newTurnContext() *TurnContext {
	return &TurnContext{liveStatusMode: "off", progressMode: "off", progressGrouping: "grouped",
		awakeIdle: state.NewAwakeIdleMeter(), longToolThresholdS: 30.0, processBaseline: map[string]struct{}{}}
}
