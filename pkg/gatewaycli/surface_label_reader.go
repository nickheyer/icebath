package gatewaycli

import (
	"context"
	"sort"
	"sync"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Read only session store whose source filtered listings also match gateway local rows by created surface
type SurfaceLabelReader struct {
	wrapped   *state.SessionDB
	mu        sync.Mutex
	hasColumn bool
}

// Arguments of a source filtered session search, a zero Limit meaning the default page of 20
type SearchSessionsOptions struct {
	Sources         []string
	Limit           int
	Offset          int
	WorkspaceKey    string
	SkipLaunchStubs bool
}

// Wraps a session store so its listings read the labels the classic CLI wrote
func NewSurfaceLabelReader(db *state.SessionDB) *SurfaceLabelReader {
	return &SurfaceLabelReader{wrapped: db}
}

// Wrapped session store serving every read the label reader does not rewrite
func (r *SurfaceLabelReader) DB() *state.SessionDB { return r.wrapped }

// Releases the wrapped store handles
func (r *SurfaceLabelReader) Close() error { return r.wrapped.Close() }

// Whether sessions has created_source, only a present answer cached since another connection may add it
func (r *SurfaceLabelReader) hasCreatedSourceColumn(ctx context.Context) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.hasColumn {
		rows, err := r.wrapped.ReadAll(ctx, "PRAGMA table_info(sessions)")
		if err != nil {
			return false, err
		}
		for _, row := range rows {
			if len(row.Keys) > 1 && row.Vals[row.Keys[1]] == "created_source" {
				r.hasColumn = true
				break
			}
		}
	}
	return r.hasColumn, nil
}

// Live source labels and created source labels of a requested filter, ok false for a raw source filter
func (r *SurfaceLabelReader) expand(ctx context.Context, requested []string) (live, created []string, ok bool, err error) {
	wanted := map[string]bool{}
	for _, name := range requested {
		wanted[name] = true
	}
	createdSet := map[string]bool{}
	liveSet := map[string]bool{}
	for name := range wanted {
		liveSet[name] = true
	}
	for name, group := range gateway.SurfaceGroups {
		if wanted[name] {
			for member := range group {
				liveSet[member] = true
				createdSet[member] = true
			}
		}
	}
	if len(createdSet) == 0 {
		return nil, nil, false, nil
	}
	has, err := r.hasCreatedSourceColumn(ctx)
	if err != nil || !has {
		return nil, nil, false, err
	}
	liveSet["local"] = true
	return sortedNames(liveSet), sortedNames(createdSet), true, nil
}

// Set members in ascending order
func sortedNames(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Source filter and created source filter a listing runs with, the request itself without the column
func (r *SurfaceLabelReader) labelled(ctx context.Context, requested []string) (sources, surfaces []string, err error) {
	live, created, ok, err := r.expand(ctx, requested)
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return requested, nil, nil
	}
	return live, created, nil
}

// Plain map of one stored row, nil for a missing row
func plainRow(row *pyjson.Object) map[string]any {
	if row == nil {
		return nil
	}
	plain, _ := pyjson.Plain(row).(map[string]any)
	return plain
}

// Sessions most recently active first, a cli filter also matching local rows the CLI created
func (r *SurfaceLabelReader) SearchSessions(ctx context.Context, o SearchSessionsOptions) ([]map[string]any, error) {
	limit := o.Limit
	if limit == 0 {
		limit = 20
	}
	sources, surfaces, err := r.labelled(ctx, o.Sources)
	if err != nil {
		return nil, err
	}
	rows, err := r.wrapped.SearchSessions(ctx, state.SearchSessionsOptions{Sources: sources, Limit: limit, Offset: o.Offset,
		WorkspaceKey: o.WorkspaceKey, LocalCreatedSources: surfaces, SkipLaunchStubs: o.SkipLaunchStubs})
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, len(rows))
	for i, row := range rows {
		out[i] = plainRow(row)
	}
	return out, nil
}

// Rich session page whose source filter also matches local rows created by the requested surfaces
func (r *SurfaceLabelReader) ListSessionsRich(ctx context.Context, o state.ListSessionsRichOptions) ([]*pyjson.Object, error) {
	requested := o.Sources
	if len(requested) == 0 && o.Source != "" {
		requested = []string{o.Source}
	}
	o.Source, o.Sources = "", nil
	if len(requested) == 0 {
		return r.wrapped.ListSessionsRich(ctx, o)
	}
	sources, surfaces, err := r.labelled(ctx, requested)
	if err != nil {
		return nil, err
	}
	o.Sources, o.LocalCreatedSources = sources, surfaces
	return r.wrapped.ListSessionsRich(ctx, o)
}

// Session count under the list filters with the same created surface matching as the listings
func (r *SurfaceLabelReader) SessionCount(ctx context.Context, o state.SessionCountOptions) (int, error) {
	requested := o.Sources
	if len(requested) == 0 && o.Source != "" {
		requested = []string{o.Source}
	}
	live, created, ok, err := r.expand(ctx, requested)
	if err != nil {
		return 0, err
	}
	if !ok {
		return r.wrapped.SessionCount(ctx, o)
	}
	o.Source, o.Sources, o.LocalCreatedSources = "", live, created
	return r.wrapped.SessionCount(ctx, o)
}

// Stored session row as a plain map, nil when the id is unknown
func (r *SurfaceLabelReader) GetSession(ctx context.Context, sessionID string) (map[string]any, error) {
	row, err := r.wrapped.GetSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return plainRow(row), nil
}

// Session id a title resolves to, empty when no session carries it
func (r *SurfaceLabelReader) ResolveSessionByTitle(ctx context.Context, title string) (string, error) {
	id, found, err := r.wrapped.ResolveSessionByTitle(ctx, title)
	if err != nil || !found {
		return "", err
	}
	return id, nil
}

// Exact session id or the single unambiguous prefix match, empty otherwise
func (r *SurfaceLabelReader) ResolveSessionID(ctx context.Context, idOrPrefix string) (string, error) {
	return r.wrapped.ResolveSessionID(idOrPrefix)
}

// Live compression tip of a session lineage
func (r *SurfaceLabelReader) GetCompressionTip(ctx context.Context, sessionID string) (string, error) {
	return r.wrapped.GetCompressionTip(ctx, sessionID)
}

// Durable local receipt of a session as a plain map, nil without one
func (r *SurfaceLabelReader) LocalReceipt(ctx context.Context, sessionID string) (map[string]any, error) {
	receipt, err := r.wrapped.LocalReceipt(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return plainRow(receipt), nil
}
