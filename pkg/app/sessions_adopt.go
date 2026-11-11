package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	adoptProgressEvery = 25
	// Frame cap of a resume reply carrying a whole long history
	adoptMaxFrameBytes = 384 * 1024 * 1024
)

var adoptSkipHints = map[string]string{
	"launch_credentials_unavailable": "its saved model route is gone; open it once and pick a model",
	"not_found":                      "not adoptable here (another identity or profile owns it)",
}

// Text of the first column of a scanned row
func adoptCellText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	}
	return pyrepr.Str(v)
}

// Unadopted pre gateway local chats of a store, one compression tip each, oldest first
func adoptableSessions(ctx context.Context, path string) ([]*pyjson.Object, error) {
	db, err := state.NewSessionDB(ctx, path, true)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	local := state.LegacyLocalSources()
	refusedSet := state.NotClientLabels()
	refused := make([]string, 0, len(refusedSet))
	for label := range refusedSet {
		refused = append(refused, label)
	}
	sort.Strings(refused)
	marks := func(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }
	query := "SELECT id FROM sessions WHERE source IN (" + marks(len(local)) + ")" +
		" OR (source NOT IN (" + marks(len(refused)) + ") AND ifnull(session_key,'')=''" +
		" AND ifnull(chat_id,'')='' AND ifnull(origin_json,'')='') ORDER BY started_at, id"
	args := make([]any, 0, len(local)+len(refused))
	for _, s := range append(append([]string{}, local...), refused...) {
		args = append(args, s)
	}
	var ids []string
	bound := map[string]bool{}
	err = db.ReadCtx(ctx, func(conn state.SQLiteConn) error {
		rows, err := conn.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var v any
			if err := rows.Scan(&v); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, adoptCellText(v))
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		keys, err := conn.QueryContext(ctx, "SELECT key FROM state_meta WHERE key LIKE ? OR key LIKE ?",
			state.LocalPolicyPrefix+"%", state.LocalLegacyPrefix+"%")
		if err != nil {
			return err
		}
		defer keys.Close()
		for keys.Next() {
			var v any
			if err := keys.Scan(&v); err != nil {
				return err
			}
			bound[adoptCellText(v)] = true
		}
		return keys.Err()
	})
	if err != nil {
		return nil, err
	}
	found := []*pyjson.Object{}
	seen := map[string]bool{}
	for _, sid := range ids {
		tipID, err := db.GetCompressionTip(ctx, sid)
		if err != nil {
			return nil, err
		}
		if tipID == "" {
			tipID = sid
		}
		if seen[tipID] {
			continue
		}
		seen[tipID] = true
		tip, err := db.GetSession(ctx, tipID)
		if err != nil {
			return nil, err
		}
		if tip == nil {
			continue
		}
		key := tipID
		if chatID := rowGet(tip, "chat_id"); pyrepr.Truthy(chatID) {
			text, ok := chatID.(string)
			if !ok {
				return nil, &pyrepr.FormatError{Class: "TypeError",
					Msg: "can only concatenate str (not \"" + pyrepr.TypeName(chatID) + "\") to str"}
			}
			key = text
		}
		if bound[state.LocalPolicyPrefix+key] || bound[state.LocalLegacyPrefix+key] {
			continue
		}
		if err := db.ValidateLegacyRow(tip, gateway.HostOwnerSubject()); err != nil {
			var refusal *state.RuntimeStoreError
			if errors.As(err, &refusal) {
				continue
			}
			return nil, err
		}
		found = append(found, tip)
	}
	return found, nil
}

// One owner refusal as the report prints it, the reason code then why
func adoptRefusal(exc *gateway.GatewayRPCError) string {
	reason := exc.Error()
	field, detail := exc.Data["field"], exc.Data["detail"]
	why, ok := adoptSkipHints[reason]
	if !ok || why == "" {
		if pyrepr.Truthy(field) && pyrepr.Truthy(detail) {
			why = pyrepr.Str(field) + ": " + pyrepr.Str(detail)
		} else {
			why = "refused by the gateway"
		}
	}
	return reason + " (" + why + ")"
}

// Resumes each row once over the native host connection then detaches, returning adopted and refusal counts
func adoptSessions(ctx context.Context, rows []*pyjson.Object, report func(string)) (int, map[string]int, error) {
	adopted, skipped := 0, map[string]int{}
	client, err := gateway.ConnectGatewayWith(ctx, gateway.ConnectGatewayOptions{MaxSize: adoptMaxFrameBytes})
	if err != nil {
		return 0, nil, err
	}
	defer client.Close()
	for i, row := range rows {
		done := i + 1
		snapshot, err := client.RPC(ctx, "session.resume", map[string]any{"session_id": rowGet(row, "id")})
		if err != nil {
			var refusal *gateway.GatewayRPCError
			if errors.As(err, &refusal) {
				skipped[adoptRefusal(refusal)]++
				continue
			}
			return 0, nil, err
		}
		sessionID := snapshot["session_id"]
		if !pyrepr.Truthy(sessionID) {
			sessionID = rowGet(row, "id")
		}
		subscriptionID, ok := snapshot["subscription_id"]
		if !ok {
			return 0, nil, &pyrepr.FormatError{Class: "KeyError", Msg: "'subscription_id'"}
		}
		if _, err := client.RPC(ctx, "session.detach", map[string]any{"session_id": sessionID,
			"subscription_id": subscriptionID}); err != nil {
			return 0, nil, err
		}
		adopted++
		if done%adoptProgressEvery == 0 {
			report(fmt.Sprintf("  %d/%d …", done, len(rows)))
		}
	}
	return adopted, skipped, nil
}

// One line for a store adopt cannot read, naming what is wrong and what to run
func adoptUnreadableStore(exc error, store string) string {
	if state.IsSQLiteLockError(exc) {
		return "Error: " + store + " is locked by another program. Close that program, then run this command again."
	}
	if state.SchemaNotBuilt(exc) {
		return "Error: " + store + " is from an older Icebath and has not been upgraded (" + sessionsExcText(exc) + "). Run " +
			"`icebath sessions repair` to upgrade it (it makes a backup first), then run this command again."
	}
	return "Error: could not read " + store + " (" + sessionsExcText(exc) + "). Run `icebath sessions repair` to check it."
}

// Brings the pre gateway local chats of the profile under its gateway, counting only on a dry run
func cmdAdopt(ctx context.Context, h string, ns map[string]any, w io.Writer) (int, error) {
	path := filepath.Join(h, "state.db")
	if _, err := os.Stat(path); err != nil {
		fmt.Fprintf(w, "No session database at %s/state.db (nothing to adopt).\n", home.Display(h))
		return 0, nil
	}
	rows, err := adoptableSessions(ctx, path)
	if err != nil {
		if _, ok := state.SQLiteClassOf(err); ok {
			fmt.Fprintln(stderrWriter(), adoptUnreadableStore(err, home.Display(h)+"/state.db"))
			return 1, nil
		}
		return 1, err
	}
	if len(rows) == 0 {
		fmt.Fprintln(w, "Nothing to adopt: every local chat in this profile already belongs to its gateway.")
		return 0, nil
	}
	counts := map[string]int{}
	for _, row := range rows {
		counts[pyrepr.Str(rowGet(row, "source"))]++
	}
	sources := make([]string, 0, len(counts))
	for s := range counts {
		sources = append(sources, s)
	}
	sort.Strings(sources)
	parts := make([]string, 0, len(sources))
	for _, s := range sources {
		parts = append(parts, fmt.Sprintf("%d %s", counts[s], s))
	}
	fmt.Fprintf(w, "%d pre-gateway chat(s) to adopt (%s).\n", len(rows), strings.Join(parts, ", "))
	if nsTruthy(ns, "dry_run") {
		fmt.Fprintln(w, "Dry run: nothing was adopted.")
		return 0, nil
	}
	adopted, skipped, err := adoptSessions(ctx, rows, func(line string) { fmt.Fprintln(w, line) })
	if err != nil {
		message := "Gateway connection/read failed"
		var refused *gateway.GatewayClientError
		if errors.As(err, &refused) {
			message = refused.Error()
		}
		fmt.Fprintln(stderrWriter(), "Error: "+message)
		return 1, nil
	}
	fmt.Fprintf(w, "Adopted %d chat(s).\n", adopted)
	refusals := make([]string, 0, len(skipped))
	for r := range skipped {
		refusals = append(refusals, r)
	}
	sort.Strings(refusals)
	for _, r := range refusals {
		fmt.Fprintf(w, "Skipped %d: %s.\n", skipped[r], r)
	}
	return 0, nil
}
