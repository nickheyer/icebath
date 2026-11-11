package agent

import (
	"context"
	"log/slog"
	"reflect"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Store the agent persists its session through, the canonical SessionDB or a managed worker's RuntimeSessionStore
type SessionStore interface {
	CreateSession(n state.NewSession) error
	GetSession(ctx context.Context, sessionID string) (*pyjson.Object, error)
	GetSessionTitle(ctx context.Context, sessionID string) (string, bool, error)
	GetSessionTitleSource(ctx context.Context, sessionID string) (string, bool, error)
	SetSessionTitle(ctx context.Context, sessionID, title string) (bool, error)
	SetSessionTitleSource(ctx context.Context, sessionID, source string) (bool, error)
	UpdateSystemPrompt(ctx context.Context, sessionID string, systemPrompt *string) error
	SetUserMessageContent(ctx context.Context, sessionID string, rowID int64, content any, displayMetadata any) (int64, error)
	SetMessageAPIContent(ctx context.Context, sessionID string, rowID int64, content any, apiContent string) (int64, error)
	SetLatestUserAPIContent(ctx context.Context, sessionID string, content any, apiContent string) (int64, error)
	PatchSessionModelConfig(ctx context.Context, sessionID string, patch map[string]any) error
	GetModelConfigValue(sessionID, key string) (any, bool, error)
	PatchModelConfig(sessionID string, patch map[string]any) error
	GetConversationRoot(sessionID string) (string, error)
	GetCompressionTip(ctx context.Context, sessionID string) (string, error)
	ResolveResumeSessionID(sessionID string) (string, error)
	ClearSessionActivityLabels(sessionID string) error
	TouchSessionActivity(sessionID string, at *float64, description string, provenance state.ActivityProvenance) error
	UpdateSessionBillingRoute(ctx context.Context, sessionID, provider, baseURL string, billingMode *string) error
	QueueTokenCounts(sessionID string, delta state.TokenCountDelta) error
	RecordAuxiliaryUsage(ctx context.Context, sessionID, task string, usage state.AuxiliaryUsage) error
	FlushTokenCountsChecked(timeoutS float64) (bool, error)
	AppendMessagesBatch(ctx context.Context, sessionID string, messages []map[string]any, guards state.AppendGuards, chunkRows *int) (int, error)
	PublishCompressionChild(ctx context.Context, p state.CompressionChildPublish) error
	ArchiveAndCompact(ctx context.Context, sessionID string, compacted []map[string]any, o state.ArchiveCompactOptions) (int, error)
	LatestConversationRole(ctx context.Context, sessionID string) (string, error)
	GetMessageRole(ctx context.Context, sessionID string, rowID int64) (string, bool, error)
	GetCompressionLockHolder(sessionID string) (*string, error)
	GetActiveMessageWatermark(ctx context.Context, sessionID string) (int64, error)
	UpdateSessionToolNames(sessionID string, pin any) error
	GetMessagesAsConversation(ctx context.Context, sessionID string, o state.ConversationOptions) ([]map[string]any, error)
	EndSession(sessionID, endReason string) error
	AcquireSessionTurnLease(sessionID, holder string, o state.TurnLeaseWait) (bool, error)
	RefreshSessionTurnLease(sessionID, holder string, ttlSeconds float64, patienceS *float64) (bool, error)
	ReleaseSessionTurnLease(sessionID, holder string) error
	GetCompressionLineage(ctx context.Context, sessionID string) ([]string, error)
	IsExplicitForkChild(ctx context.Context, sessionID string) (bool, error)
	DeclaredScopeIdentity(ctx context.Context, sessionID string) (bool, string, error)
	LatestConversationBoundary(ctx context.Context, sessionKey, source string) (int64, bool, error)
	UpdateConversationModelConfigValue(sessionID, key string, update func(current any) any) (string, error)
	SessionIDsWithModelConfigKey(key string) ([]string, error)
}

var (
	_ SessionStore = (*state.SessionDB)(nil)
	_ SessionStore = (*RuntimeSessionStore)(nil)
	_ SessionStore = (*ChildSessionStore)(nil)
)

// The store or nil, a typed nil pointer read as no store the way Python reads None
func sessionStoreOrNil(store SessionStore) SessionStore {
	if store == nil {
		return nil
	}
	if v := reflect.ValueOf(store); v.Kind() == reflect.Pointer && v.IsNil() {
		return nil
	}
	return store
}

// db_path of the store, empty when the store has none the way getattr(store, 'db_path', None) reads it
func sessionStorePath(store SessionStore) string {
	if p, ok := store.(interface{ Path() string }); ok {
		return p.Path()
	}
	return ""
}

// Python class name of the store an AttributeError names
func sessionStoreKind(store SessionStore) string {
	switch s := store.(type) {
	case *RuntimeSessionStore:
		return s.kind
	case *ChildSessionStore:
		return s.kind
	case nil:
		return "NoneType"
	}
	return "SessionDB"
}

// Auxiliary usage recorder writing through the agent's store, nil without one
func auxUsageRecorderFor(store SessionStore) provider.AuxUsageRecorder {
	if store = sessionStoreOrNil(store); store == nil {
		return nil
	}
	return storeAuxUsageRecorder{store: store}
}

type storeAuxUsageRecorder struct{ store SessionStore }

// Records off a running event loop, where a worker store refuses synchronous owner calls
func (r storeAuxUsageRecorder) RecordAuxiliaryUsage(ctx context.Context, sessionID, task string, usage provider.AuxiliaryUsage) error {
	return CallOffEventLoop(ctx, func(ctx context.Context) error {
		return r.store.RecordAuxiliaryUsage(ctx, sessionID, task, provider.StateAuxiliaryUsage(usage))
	})
}

// Releases a registry shared database or closes any other store, the release_or_close of hermes_state_registry
func releaseOrCloseStore(store SessionStore) {
	if db := canonicalSessionDB(store); db != nil {
		state.ReleaseOrClose(db)
		return
	}
	if closer, ok := store.(interface{ Close() error }); ok {
		if err := closer.Close(); err != nil {
			slog.Debug("release_or_close fallback close failed", "error", err)
		}
	}
}

// The canonical SessionDB behind the store, nil for a worker store
func canonicalSessionDB(store SessionStore) *state.DB {
	db, _ := store.(*state.DB)
	return db
}
