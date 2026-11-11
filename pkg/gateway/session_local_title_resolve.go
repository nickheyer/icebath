package gateway

import (
	"context"
	"sort"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Sources of the classic CLI family, its finite one shot runs included
var cliFamilySources = []string{"cli", provider.OneshotSource}

// Refusal reason of an error, empty for one that is no runtime store refusal
func refusalReasonOf(err error) string {
	if refusal := runtimeStoreErrorOf(err); refusal != nil {
		return refusal.Reason
	}
	return ""
}

// Local session an exact id or title names, projected to its compression tip, nil when missing and allowed
func resolveTitledSession(ctx context.Context, a *SessionAuthority, actor Principal, name any, missingOK bool) (*SessionRef, error) {
	text, isText := name.(string)
	if !isText || pystr.Strip(text) == "" {
		return nil, runtimeRefusal("invalid_params")
	}
	db := a.DB
	row, err := db.GetSession(ctx, text)
	if err != nil {
		return nil, err
	}
	sid := ""
	if row != nil {
		sid, _ = row.GetOr("id", "").(string)
	} else if sid, _, err = db.ResolveSessionByTitle(ctx, text); err != nil {
		return nil, err
	}
	var tipID string
	if sid != "" {
		tip, err := db.GetCompressionTip(ctx, sid)
		if err != nil {
			return nil, err
		}
		if tip == "" {
			tip = sid
		}
		tipRow, err := db.GetSession(ctx, tip)
		if err != nil {
			return nil, err
		}
		if tipRow != nil {
			tipID, _ = tipRow.GetOr("id", "").(string)
		}
	}
	if tipID == "" {
		if missingOK {
			return nil, nil
		}
		return nil, runtimeRefusal("not_found")
	}
	moved, err := handedOffRef(ctx, a, tipID)
	if err != nil {
		return nil, err
	}
	var ref SessionRef
	if moved != nil {
		err = a.Authorize(ctx, actor, *moved, "session:read")
		ref = *moved
	} else {
		ref, err = resolveLocalTarget(ctx, a, actor, tipID)
	}
	if err != nil {
		if refusalReasonOf(err) == "permission_denied" {
			return nil, runtimeRefusal("not_found")
		}
		return nil, err
	}
	return &ref, nil
}

// Most recently active local session of a surface family the actor may read, its workspace first
func resolveLatestSession(ctx context.Context, a *SessionAuthority, actor Principal, family any, workspace any) (SessionRef, error) {
	families := map[string][]string{"tui": {"tui"}}
	cli := append([]string(nil), cliFamilySources...)
	sort.Strings(cli)
	families["cli"] = cli
	name, _ := family.(string)
	members, known := families[name]
	scope, scopeText := workspace.(string)
	if _, isText := family.(string); !isText || !known || (workspace != nil && (!scopeText || scope == "")) {
		return SessionRef{}, runtimeRefusal("invalid_params")
	}
	sources := append(append([]string(nil), members...), "local")
	scopes := []string{}
	if scope != "" {
		scopes = append(scopes, scope)
	}
	scopes = append(scopes, "")
	for _, workspaceKey := range scopes {
		rows, err := a.DB.SearchSessions(ctx, state.SearchSessionsOptions{Sources: sources, LocalCreatedSources: []string{name},
			Limit: 50, WorkspaceKey: workspaceKey, SkipLaunchStubs: true})
		if err != nil {
			return SessionRef{}, err
		}
		for _, row := range rows {
			id, _ := row.GetOr("id", "").(string)
			tip, err := a.DB.GetCompressionTip(ctx, id)
			if err != nil {
				return SessionRef{}, err
			}
			if tip == "" {
				tip = id
			}
			ref, err := resolveLocalTarget(ctx, a, actor, tip)
			if err == nil {
				return ref, nil
			}
			switch refusalReasonOf(err) {
			case "permission_denied", "not_found", "profile_mismatch":
			default:
				return SessionRef{}, err
			}
		}
	}
	return SessionRef{}, runtimeRefusal("not_found")
}
