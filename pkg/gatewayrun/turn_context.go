package gatewayrun

import (
	"context"
	"sync"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
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
	holderMu                        sync.Mutex
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
	progressReplyTo                 *string
	message                         string
	history                         []map[string]any
	contextPrompt                   string
	channelPrompt                   *string
	sessionID                       string
	sessionKey                      string
	runGeneration                   *int
	processTaskID                   string
	processBaseline                 map[string]bool
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
	capturedResult                  *pyjson.Object
	voiceTurn                       bool
	enabledToolsets                 []string
	disabledToolsets                []string
	logModeEnabled                  bool
	interimAssistantMessagesEnabled bool
	needsProgressQueue              bool
	resolveDisplaySetting           func(ctx context.Context, userConfig map[string]any, platformKey, setting string, fallback any) any
	resultHolder                    map[string]any
	toolsHolder                     []map[string]any
	streamConsumerHolder            *gateway.GatewayStreamConsumer
	streamingTTSConsumerHolder      *gateway.StreamingTTSConsumer
	voiceAckFired                   bool
	voiceAckGuild                   *int
	voiceAckLoop                    gateway.EventLoop
	loopForStep                     gateway.EventLoop
	hooksRef                        *ProfileHookRegistries
	statusAdapter                   gateway.PlatformAdapter
	statusChatID                    string
	statusThreadMetadata            map[string]any
	progressCallback                any
	voiceAckCallback                any
	stepCallbackSync                any
	eventCallbackSync               any
	statusCallbackSync              any
	nativeSlackTaskCards            bool
	nativeToolStartCallback         any
	nativeToolCompleteCallback      any
	// Session scoped tool environment the agent runs its tools in, nil for the process environment
	sessionEnv *tools.Env
}

// Context carrying the dataclass defaults of every field
func newTurnContext() *TurnContext {
	return &TurnContext{liveStatusMode: "off", progressMode: "off", progressGrouping: "grouped",
		awakeIdle: state.NewAwakeIdleMeter(), longToolThresholdS: 30.0, processBaseline: map[string]bool{}}
}

// Agent the turn runs, nil before the turn wired one
func (c *TurnContext) currentAgent() *agent.AIAgent {
	c.holderMu.Lock()
	defer c.holderMu.Unlock()
	return c.agentHolder
}

// Publishes the agent the turn runs for interrupt support
func (c *TurnContext) setAgent(a *agent.AIAgent) {
	c.holderMu.Lock()
	c.agentHolder = a
	c.holderMu.Unlock()
}

// Stream consumer the turn created, nil before run_sync made one
func (c *TurnContext) streamConsumer() *gateway.GatewayStreamConsumer {
	c.holderMu.Lock()
	defer c.holderMu.Unlock()
	return c.streamConsumerHolder
}

// Publishes the stream consumer the turn created
func (c *TurnContext) setStreamConsumer(sc *gateway.GatewayStreamConsumer) {
	c.holderMu.Lock()
	c.streamConsumerHolder = sc
	c.holderMu.Unlock()
}

// Result run_sync left for the outer turn, nil before it returned
func (c *TurnContext) result() map[string]any {
	c.holderMu.Lock()
	defer c.holderMu.Unlock()
	return c.resultHolder
}

// Publishes the run_sync result for the outer turn
func (c *TurnContext) setResult(result map[string]any) {
	c.holderMu.Lock()
	c.resultHolder = result
	c.holderMu.Unlock()
}

// Streaming TTS consumer the outer turn created, nil when voice streaming is off
func (c *TurnContext) streamingTTSConsumer() *gateway.StreamingTTSConsumer {
	c.holderMu.Lock()
	defer c.holderMu.Unlock()
	return c.streamingTTSConsumerHolder
}

// Publishes the streaming TTS consumer of the turn
func (c *TurnContext) setStreamingTTSConsumer(sc *gateway.StreamingTTSConsumer) {
	c.holderMu.Lock()
	c.streamingTTSConsumerHolder = sc
	c.holderMu.Unlock()
}
