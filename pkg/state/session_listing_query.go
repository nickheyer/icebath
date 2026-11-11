package state

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Store surface an interactive session listing reads its rows from
type SessionLister interface {
	ListSessionsRich(ctx context.Context, o ListSessionsRichOptions) ([]*pyjson.Object, error)
}

// Arguments of an interactive session listing, a zero Limit meaning ten rows
type SessionListingQuery struct {
	Source                string
	SessionKey            string
	CurrentSessionID      string
	IncludeCurrentSession bool
	IncludeAllSources     bool
	IncludeUnnamed        bool
	SearchQuery           string
	Limit                 int
	ExcludeSources        []string
}

// Session rows for the listing surfaces, source scoped, unnamed and current rows hidden unless asked for
func QuerySessionListing(ctx context.Context, db SessionLister, q SessionListingQuery) ([]*pyjson.Object, error) {
	limit := q.Limit
	if limit == 0 {
		limit = 10
	}
	search := pystr.Strip(q.SearchQuery)
	source := q.Source
	if q.IncludeAllSources {
		source = ""
	}
	fetch := max(limit*4, limit)
	rows, err := db.ListSessionsRich(ctx, ListSessionsRichOptions{Source: source, SessionKey: q.SessionKey,
		ExcludeSources: q.ExcludeSources, Limit: &fetch, SearchQuery: search, OrderByLastActive: search != ""})
	if err != nil {
		return nil, err
	}
	result := []*pyjson.Object{}
	for _, row := range rows {
		isCurrent := q.CurrentSessionID != "" && row.Vals["id"] == q.CurrentSessionID
		if (isCurrent && !q.IncludeCurrentSession) || (!q.IncludeUnnamed && !pyTruthy(row.Vals["title"]) && search == "" && !isCurrent) {
			continue
		}
		if isCurrent {
			row = row.Copy()
			row.Set("is_current_session", true)
		}
		result = append(result, row)
		if len(result) >= limit {
			break
		}
	}
	return result, nil
}
