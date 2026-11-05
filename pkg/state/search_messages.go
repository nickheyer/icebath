package state

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Keyword arguments of search_messages, NewSearchMessagesOptions giving the Python defaults
type SearchMessagesOptions struct {
	SourceFilter    []string
	ExcludeSources  []string
	RoleFilter      []string
	Limit           int
	Offset          int
	Sort            string
	IncludeInactive bool
	Fields          []string
	AfterTS         *int64
	BeforeTS        *int64
}

// Options with the search_messages defaults, twenty rows from offset zero ranked by BM25
func NewSearchMessagesOptions() SearchMessagesOptions {
	return SearchMessagesOptions{Limit: 20}
}

// Order, window and filters every route of one search shares
type searchRoute struct {
	orderBy string
	limit   int
	offset  int
	filter  searchFilter
}

// Searches messages through the routed indexes, logging the path of any search slower than search_slow_ms
func (s *sessionSearch) SearchMessages(ctx context.Context, query string, o SearchMessagesOptions) ([]*pyjson.Object, error) {
	started := time.Now()
	rows, err := s.searchMessagesImpl(ctx, query, o)
	elapsedMS := float64(time.Since(started)) / float64(time.Millisecond)
	threshold, terr := searchSlowMS(ctx)
	if terr != nil {
		return nil, terr
	}
	if elapsedMS >= threshold {
		count := "err"
		if err == nil {
			count = fmt.Sprint(len(rows))
		}
		head := query
		if runes := []rune(query); len(runes) > 200 {
			head = string(runes[:200])
		}
		slog.Info(fmt.Sprintf("slow session search: path=%s elapsed=%.0fms rows=%s query=%s",
			s.describeSearchPath(query), elapsedMS, count, pyrepr.StrRepr(head)))
	}
	return rows, err
}

// Routes a sanitized query to the LIKE fallback, base, CJK, trigram and OR relaxed searches, then finalizes the hits
func (s *sessionSearch) searchMessagesImpl(ctx context.Context, query string, o SearchMessagesOptions) ([]*pyjson.Object, error) {
	resultFields, err := searchMessageFields(o.Fields)
	if err != nil {
		return nil, err
	}
	if pystr.Strip(query) == "" {
		return []*pyjson.Object{}, nil
	}
	if query = sanitizeFTS5Query(query); query == "" {
		return []*pyjson.Object{}, nil
	}
	f := searchFilter{includeInactive: o.IncludeInactive, sourceFilter: o.SourceFilter, excludeSources: o.ExcludeSources,
		roleFilter: o.RoleFilter, afterTS: o.AfterTS, beforeTS: o.BeforeTS}
	wantsTool := slices.Contains(o.RoleFilter, "tool")
	likeFallback := func() ([]*pyjson.Object, error) {
		matches, err := s.searchMessagesLikeFallback(ctx, query, o.Limit, o.Offset, o.Sort, f)
		if err != nil {
			return nil, err
		}
		return s.finalizeSearchMatches(ctx, matches, resultFields), nil
	}
	if wantsTool {
		return likeFallback()
	}
	if err := s.refreshFTSStaleState(ctx); err != nil {
		return nil, err
	}
	if s.fts.ftsStale.Load() {
		return likeFallback()
	}
	if !s.fts.ftsEnabled.Load() {
		return []*pyjson.Object{}, nil
	}
	orderBy, ok := ftsOrderBy[pystr.Lower(pystr.Strip(o.Sort))]
	if !ok {
		orderBy = "ORDER BY rank"
	}
	route := searchRoute{orderBy: orderBy, limit: o.Limit, offset: o.Offset, filter: f}
	wantsUnindexed := wantsTool || slices.ContainsFunc(o.SourceFilter, func(src string) bool {
		return slices.Contains(FTSTrigramExcludedSources, src)
	})
	isCJK := queryContainsCJK(query)
	var matches []*pyjson.Object
	if isCJK {
		if matches, err = s.searchCJK(ctx, query, wantsUnindexed, route); err != nil {
			return nil, err
		}
	} else {
		sqlText, params := ftsMatchSQL("messages_fts", query, route.orderBy, route.limit, route.offset, route.filter)
		matches, err = s.host.readAll(ctx, sqlText, params...)
		if err != nil {
			if isOperationalError(err) {
				return []*pyjson.Object{}, nil
			}
			if !isDatabaseError(err) {
				return nil, err
			}
			detached, ferr := s.fts.enterFTSFailOpen(ctx, err, nil, nil)
			if ferr != nil {
				return nil, ferr
			}
			if !detached {
				return nil, err
			}
			if matches, err = s.searchMessagesLikeFallback(ctx, query, o.Limit, o.Offset, o.Sort, f); err != nil {
				return nil, err
			}
		}
	}
	status, err := s.FTSRebuildStatus(ctx)
	if err != nil {
		return nil, err
	}
	if status != nil && len(matches) < o.Limit {
		gap, err := s.searchUnindexedGap(ctx, query, o.Limit-len(matches), f)
		if err != nil && !isOperationalError(err) {
			return nil, err
		}
		if err != nil {
			slog.Debug(fmt.Sprintf("Unindexed-gap supplement skipped: %s", errText(err)))
		} else {
			seen := map[string]bool{}
			for _, m := range matches {
				id, _ := m.Get("id")
				seen[contextKey(id)] = true
			}
			for _, m := range gap {
				if id, _ := m.Get("id"); !seen[contextKey(id)] {
					matches = append(matches, m)
				}
			}
		}
	}
	retry := func(table, matchQuery, failOpen, operationalDebug string) error {
		rows, ok, err := s.matchRows(ctx, table, matchQuery, route.orderBy, failOpen, operationalDebug, route.limit, route.offset, route.filter)
		if err == nil && ok && len(rows) > 0 {
			matches = rows
		}
		return err
	}
	if len(matches) == 0 && !isCJK && !wantsTool {
		fbQuery := quoteFTSTokens(pystr.Strip(strings.Trim(query, `"`)))
		if s.fts.ftsCJKAvailable.Load() {
			if err := retry("messages_fts_cjk", fbQuery, "", ""); err != nil {
				return nil, err
			}
		}
		if len(matches) == 0 && s.fts.trigramAvailable.Load() && trigramEligibleTokens(query) {
			if err := retry("messages_fts_trigram", fbQuery, "", ""); err != nil {
				return nil, err
			}
		}
	}
	if len(matches) == 0 && !isCJK && !s.fts.ftsStale.Load() {
		if relaxed, ok := orRelaxedQuery(query); ok {
			if err := retry("messages_fts", relaxed, "OR-relaxed", "OR-relaxed FTS retry failed; keeping empty result"); err != nil {
				return nil, err
			}
		}
	}
	if matches == nil {
		matches = []*pyjson.Object{}
	}
	return s.finalizeSearchMatches(ctx, matches, resultFields), nil
}

// CJK routing through the bigram index, then trigram, then a LIKE scan with one clause per non operator token
func (s *sessionSearch) searchCJK(ctx context.Context, query string, wantsUnindexedRows bool, route searchRoute) ([]*pyjson.Object, error) {
	rawQuery := pystr.Strip(strings.Trim(stripCJKWildcards(query), `"`))
	matchQuery := quoteFTSTokens(rawQuery)
	if s.fts.ftsCJKAvailable.Load() && !wantsUnindexedRows && !hasLoneCJKRun(rawQuery) {
		rows, ok, err := s.matchRows(ctx, "messages_fts_cjk", matchQuery, route.orderBy, "CJK-bigram",
			"messages_fts_cjk query failed; falling back to trigram/LIKE", route.limit, route.offset, route.filter)
		if err != nil || ok {
			return rows, err
		}
	}
	if s.trigramRouteOK(rawQuery) && !wantsUnindexedRows {
		rows, ok, err := s.matchRows(ctx, "messages_fts_trigram", matchQuery, route.orderBy, "Trigram", "",
			route.limit, route.offset, route.filter)
		if err != nil || ok {
			return rows, err
		}
	}
	tokens := nonOperatorTokens(rawQuery)
	if len(tokens) == 0 {
		tokens = []string{rawQuery}
	}
	params := []any{}
	clauses := make([]string, len(tokens))
	for i, tok := range tokens {
		params = append(params, stringArgs(likeParams(tok))...)
		clauses[i] = likeAnyColumnSQL
	}
	where, params := searchFilterClauses([]string{"(" + strings.Join(clauses, " OR ") + ")"}, params, route.filter)
	args := append(append([]any{tokens[0]}, params...), route.limit, route.offset)
	return s.likeRows(ctx, where, args, "ORDER BY m.timestamp DESC", "LIMIT ? OFFSET ?")
}

// LIKE scan of the ids the deferred rebuild has not indexed yet, AND joining the query terms
func (s *sessionSearch) searchUnindexedGap(ctx context.Context, ftsQuery string, limit int, f searchFilter) ([]*pyjson.Object, error) {
	st, err := s.rebuildState(ctx, "fts_rebuild")
	if err != nil {
		return nil, err
	}
	if st != nil {
		if _, err := st.info(); err != nil {
			return nil, err
		}
	}
	if st == nil || limit <= 0 {
		return []*pyjson.Object{}, nil
	}
	var terms []string
	for _, raw := range likeTokenRE.FindAllString(ftsQuery, -1) {
		tok := pystr.Strip(strings.Trim(strings.Trim(raw, `"`), "*"))
		if tok != "" && !slices.Contains(likeSkipTokens, pystr.Upper(tok)) {
			terms = append(terms, tok)
		}
	}
	if len(terms) == 0 {
		return []*pyjson.Object{}, nil
	}
	indexed, err := sqliteInt(st.indexed)
	if err != nil {
		return nil, err
	}
	total, err := sqliteInt(st.total)
	if err != nil {
		return nil, err
	}
	where := []string{"m.id > ? AND m.id <= ?"}
	params := []any{indexed, total}
	for _, term := range terms {
		where = append(where, likeAnyColumnSQL)
		params = append(params, stringArgs(likeParams(term))...)
	}
	where, params = searchFilterClauses(where, params, f)
	args := append(append([]any{terms[0]}, params...), limit)
	return s.likeRows(ctx, where, args, "ORDER BY m.timestamp DESC", "LIMIT ?")
}
