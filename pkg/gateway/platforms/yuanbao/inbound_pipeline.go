package yuanbao

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// One inbound image, file or video reference with the file name a file carries
type InboundMediaRef struct{ Kind, URL, Name string }

// InboundContext is the mutable state every inbound middleware reads and fills in registration order
type InboundContext struct {
	Adapter          *YuanbaoAdapter
	RawFrames        [][]byte
	Push             *pyjson.Object
	DecodedVia       string
	FromAccount      any
	GroupCode        any
	GroupName        any
	SenderNickname   any
	MsgBody          []any
	MsgID            any
	CloudCustomData  any
	ChatID           string
	ChatType         string
	ChatName         any
	RawText          string
	MediaRefs        []InboundMediaRef
	ForwardedRecords *pyjson.Object
	OwnerCommand     *string
	Source           *gateway.SessionSource
	MsgType          any
	ReplyToMessageID *string
	ReplyToText      *string
	QuoteMediaRefs   []ybresRef
	MediaURLs        []string
	MediaTypes       []string
	ChannelPrompt    *string
}

// Context of one debounced batch of raw frames with every text field empty
func NewInboundContext(adapter *YuanbaoAdapter, rawFrames [][]byte) *InboundContext {
	return &InboundContext{Adapter: adapter, RawFrames: rawFrames, FromAccount: "", GroupCode: "", GroupName: "",
		SenderNickname: "", MsgBody: []any{}, MsgID: "", CloudCustomData: "", ChatName: ""}
}

// MiddlewareFunc is one pipeline step, calling next continues the chain and returning without it stops
type MiddlewareFunc func(ctx context.Context, ic *InboundContext, next func() error) error

// InboundMiddleware is a named pipeline step
type InboundMiddleware interface {
	Name() string
	Handle(ctx context.Context, ic *InboundContext, next func() error) error
}

// Class name and registered name every middleware carries
type middlewareBase struct{ class, name string }

func (m middlewareBase) Name() string { return m.name }

// Class and name the way the Python repr shows a middleware
func (m middlewareBase) String() string {
	return fmt.Sprintf("<%s name=%s>", m.class, pyrepr.StrRepr(m.name))
}

type pipelineEntry struct {
	name    string
	handler MiddlewareFunc
	when    func(*InboundContext) bool
}

// InboundPipeline is the onion model chain of named middlewares with optional guards
type InboundPipeline struct {
	mu          sync.Mutex
	middlewares []pipelineEntry
}

func NewInboundPipeline() *InboundPipeline { return &InboundPipeline{} }

// Name and handler of a middleware instance, or of a plain name with its handler
func normalizeMiddleware(nameOrMW any, handler MiddlewareFunc) (string, MiddlewareFunc) {
	if mw, ok := nameOrMW.(InboundMiddleware); ok {
		return mw.Name(), mw.Handle
	}
	return pyrepr.Str(nameOrMW), handler
}

// Appends a middleware instance, or a plain handler under a name
func (p *InboundPipeline) Use(nameOrMW any, handler MiddlewareFunc, when func(*InboundContext) bool) *InboundPipeline {
	return p.insertRelative(nil, 0, nameOrMW, handler, when)
}

// Inserts at the index of target plus offset, appending when target is nil or not registered
func (p *InboundPipeline) insertRelative(target *string, offset int, nameOrMW any, handler MiddlewareFunc, when func(*InboundContext) bool) *InboundPipeline {
	name, h := normalizeMiddleware(nameOrMW, handler)
	p.mu.Lock()
	defer p.mu.Unlock()
	at := len(p.middlewares)
	if target != nil {
		for i, entry := range p.middlewares {
			if entry.name == *target {
				at = i + offset
				break
			}
		}
	}
	p.middlewares = append(p.middlewares, pipelineEntry{})
	copy(p.middlewares[at+1:], p.middlewares[at:])
	p.middlewares[at] = pipelineEntry{name: name, handler: h, when: when}
	return p
}

// Inserts a middleware right before the one registered as target
func (p *InboundPipeline) UseBefore(target string, nameOrMW any, handler MiddlewareFunc, when func(*InboundContext) bool) *InboundPipeline {
	return p.insertRelative(&target, 0, nameOrMW, handler, when)
}

// Inserts a middleware right after the one registered as target
func (p *InboundPipeline) UseAfter(target string, nameOrMW any, handler MiddlewareFunc, when func(*InboundContext) bool) *InboundPipeline {
	return p.insertRelative(&target, 1, nameOrMW, handler, when)
}

// Drops every middleware registered under the name
func (p *InboundPipeline) Remove(name string) *InboundPipeline {
	p.mu.Lock()
	defer p.mu.Unlock()
	kept := make([]pipelineEntry, 0, len(p.middlewares))
	for _, entry := range p.middlewares {
		if entry.name != name {
			kept = append(kept, entry)
		}
	}
	p.middlewares = kept
	return p
}

// Registered middleware names in chain order
func (p *InboundPipeline) MiddlewareNames() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	names := make([]string, len(p.middlewares))
	for i, entry := range p.middlewares {
		names[i] = entry.name
	}
	return names
}

// Runs the chain, each middleware receiving the context and the continuation, an error logged at every level it crosses
func (p *InboundPipeline) Execute(ctx context.Context, ic *InboundContext) error {
	p.mu.Lock()
	chain := p.middlewares
	p.mu.Unlock()
	index := 0
	var next func() error
	next = func() error {
		for index < len(chain) {
			entry := chain[index]
			index++
			if entry.when != nil && !entry.when(ic) {
				continue
			}
			var err error
			if entry.handler == nil {
				err = &pyErr{"TypeError", "'NoneType' object is not callable"}
			} else {
				err = entry.handler(ctx, ic, next)
			}
			if err != nil {
				if ctx.Err() == nil {
					slog.Error(fmt.Sprintf("[InboundPipeline] middleware [%s] error", entry.name), "error", err)
				}
				return err
			}
			return nil
		}
		return nil
	}
	return next()
}

// Constructors of the default Yuanbao inbound pipeline in the order it runs
var defaultMiddlewares = []func() InboundMiddleware{
	newDecodeMiddleware, newExtractFieldsMiddleware, newRecallGuardMiddleware, newDedupMiddleware, newSkipSelfMiddleware,
	newChatRoutingMiddleware, newAccessGuardMiddleware, newExtractContentMiddleware, newPlaceholderFilterMiddleware,
	newOwnerCommandMiddleware, newBuildSourceMiddleware, newGroupAtGuardMiddleware, newAutoSetHomeMiddleware,
	newGroupAttributionMiddleware, newClassifyMessageTypeMiddleware, newQuoteContextMiddleware,
	newForwardedRecordsParseMiddleware, newMediaResolveMiddleware, newPatchAnchorsMiddleware, newDispatchMiddleware,
}

// Default Yuanbao inbound pipeline with a fresh instance of every middleware
func BuildInboundPipeline() *InboundPipeline {
	pipeline := NewInboundPipeline()
	for _, construct := range defaultMiddlewares {
		pipeline.Use(construct(), nil, nil)
	}
	return pipeline
}
