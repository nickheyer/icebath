package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

func rawText(s string) json.RawMessage {
	enc, _ := json.Marshal(s)
	return enc
}

func rawTexts(values []string) json.RawMessage {
	elements := make([]json.RawMessage, len(values))
	for i, v := range values {
		elements[i] = rawText(v)
	}
	return rawArray(elements)
}

func jsonMatchesCell(raw json.RawMessage, cell sql.NullString) bool {
	if !cell.Valid {
		return jsonIsNull(raw)
	}
	return jsonIsString(raw) && jsonString(raw) == cell.String
}

func routedEntry(ctx context.Context, c Conn, route json.RawMessage, tip string) ([]configMember, error) {
	if !jsonIsString(route) {
		return nil, nil
	}
	saved, err := queryStrings(ctx, c, "SELECT entry_json FROM gateway_routing WHERE scope='' AND session_key=?", jsonString(route))
	if err != nil || len(saved) == 0 {
		return nil, err
	}
	raw := json.RawMessage(saved[0])
	if !json.Valid(raw) {
		return nil, runtimeErr(ReasonStorageUnavailable)
	}
	if jsonIsNull(raw) {
		return nil, nil
	}
	routed, ok := objectMembers(raw)
	if !ok {
		return nil, runtimeErr(ReasonStorageUnavailable)
	}
	target, has := memberValue(routed, "session_id")
	if !has {
		return nil, runtimeErr(ReasonStorageUnavailable)
	}
	if !jsonIsString(target) || jsonString(target) != tip {
		return nil, nil
	}
	return routed, nil
}

func commitLocalTarget(ctx context.Context, c Conn, receipt, routed []configMember) error {
	entry, err := canonicalObject(rawObject(routed))
	if err != nil {
		return err
	}
	var key any
	if route, _ := memberValue(receipt, "route"); jsonIsString(route) {
		key = jsonString(route)
	}
	if _, err := c.ExecContext(ctx, "UPDATE gateway_routing SET entry_json=? WHERE scope='' AND session_key=?", entry, key); err != nil {
		return err
	}
	stored, err := canonicalObject(rawObject(receipt))
	if err != nil {
		return err
	}
	owner, _ := memberText(receipt, "session_id")
	if _, err := c.ExecContext(ctx, "UPDATE state_meta SET value=? WHERE key=?", stored, localPolicyPrefix+owner); err != nil {
		return err
	}
	_, err = c.ExecContext(ctx, "UPDATE sessions SET runtime_revision=runtime_revision+1 WHERE id=?", owner)
	return err
}

func advanceLocalTarget(ctx context.Context, c Conn, parentID, childID string, entry []configMember) error {
	var chatID, userID, sessionKey sql.NullString
	err := c.QueryRowContext(ctx, "SELECT chat_id, user_id, session_key FROM sessions WHERE id=?", parentID).Scan(&chatID, &userID, &sessionKey)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	logical := chatID.String
	if logical == "" {
		return nil
	}
	saved, found, err := metaValue(ctx, c, localPolicyPrefix+logical)
	if err != nil {
		return err
	}
	unavailable := runtimeErr(ReasonStorageUnavailable)
	if !found {
		legacy, err := metaExists(ctx, c, localLegacyPrefix+logical)
		if err != nil {
			return err
		}
		if strings.HasPrefix(logical, "local-") || legacy {
			return unavailable
		}
		return nil
	}
	receipt, ok := objectMembers(json.RawMessage(saved))
	if !ok {
		return unavailable
	}
	owner, hasOwner := memberValue(receipt, "session_id")
	rawEntry, _ := memberValue(receipt, "entry")
	current, entryIsObject := objectMembers(rawEntry)
	target, hasTarget := memberValue(current, "session_id")
	principal, hasPrincipal := memberValue(receipt, "principal_id")
	route, hasRoute := memberValue(receipt, "route")
	if !hasOwner || !jsonMatchesCell(owner, sql.NullString{String: logical, Valid: true}) || !entryIsObject || !hasTarget ||
		!jsonMatchesCell(target, sql.NullString{String: parentID, Valid: true}) || !hasPrincipal || !jsonMatchesCell(principal, userID) ||
		!hasRoute || !jsonMatchesCell(route, sessionKey) {
		return unavailable
	}
	routed, err := routedEntry(ctx, c, route, parentID)
	if err != nil {
		return err
	}
	if routed == nil {
		return runtimeErr(ReasonAdmissionConflict)
	}
	base := entry
	if len(base) == 0 {
		base = routed
	}
	candidate := withMember(base, "session_id", rawText(childID))
	lineage := []string{logical}
	if raw, has := memberValue(receipt, "lineage"); has {
		if lineage, ok = stringList(raw); !ok {
			return unavailable
		}
	}
	receipt = withMember(receipt, "entry", rawObject(candidate))
	receipt = withMember(receipt, "lineage", rawTexts(append(lineage, childID)))
	if _, err := validateLocalLineage(ctx, c, receipt); err != nil {
		return err
	}
	return commitLocalTarget(ctx, c, receipt, candidate)
}

func untrackedSteps(ctx context.Context, c Conn, tip string, lineage []string) ([]string, bool, error) {
	seen := map[string]bool{tip: true}
	for _, id := range lineage {
		seen[id] = true
	}
	var steps []string
	current := tip
	for {
		chain, err := walkCompressionChain(current, func(id string) (string, error) { return chainStep(ctx, c, id) })
		if err != nil {
			return nil, false, err
		}
		batch := chain[1:]
		if len(batch) == 0 {
			return steps, true, nil
		}
		for _, id := range batch {
			if seen[id] {
				return nil, false, nil
			}
		}
		for _, id := range batch {
			seen[id] = true
		}
		steps = append(steps, batch...)
		current = batch[len(batch)-1]
	}
}

func adoptableCompression(ctx context.Context, c Conn, receipt []configMember) ([]string, []configMember, error) {
	unavailable := runtimeErr(ReasonStorageUnavailable)
	rawEntry, _ := memberValue(receipt, "entry")
	entry, ok := objectMembers(rawEntry)
	if !ok {
		return nil, nil, unavailable
	}
	tip, ok := memberText(entry, "session_id")
	if !ok {
		return nil, nil, unavailable
	}
	owner, hasOwner := memberText(receipt, "session_id")
	lineage := []string{owner}
	if raw, has := memberValue(receipt, "lineage"); has {
		if lineage, ok = stringList(raw); !ok {
			return nil, nil, unavailable
		}
	} else if !hasOwner {
		return nil, nil, unavailable
	}
	children, acyclic, err := untrackedSteps(ctx, c, tip, lineage)
	if err != nil || !acyclic || len(children) == 0 {
		return nil, nil, err
	}
	route, hasRoute := memberValue(receipt, "route")
	principal, hasPrincipal := memberValue(receipt, "principal_id")
	rawOwner, _ := memberValue(receipt, "session_id")
	if !hasRoute || !hasPrincipal || !hasOwner {
		return nil, nil, unavailable
	}
	for _, childID := range children {
		var key, chat, user sql.NullString
		if err := c.QueryRowContext(ctx, "SELECT session_key,chat_id,user_id FROM sessions WHERE id=?", childID).Scan(&key, &chat, &user); err != nil {
			return nil, nil, err
		}
		if !jsonMatchesCell(route, key) || !jsonMatchesCell(rawOwner, chat) || !jsonMatchesCell(principal, user) {
			return nil, nil, nil
		}
	}
	routed, err := routedEntry(ctx, c, route, tip)
	if err != nil || routed == nil {
		return nil, nil, err
	}
	return children, routed, nil
}

func (s *Store) AdoptableCompression(ctx context.Context, receipt json.RawMessage) (bool, error) {
	members, ok := objectMembers(receipt)
	if !ok {
		return false, runtimeErr(ReasonStorageUnavailable)
	}
	adoptable := false
	err := s.Read(ctx, func(c Conn) error {
		children, _, err := adoptableCompression(ctx, c, members)
		adoptable = len(children) > 0
		return err
	})
	return adoptable, err
}

func (s *Store) AdoptUntrackedCompression(ctx context.Context, epoch int64, sessionID string, expected json.RawMessage) error {
	return s.Write(ctx, WritePatience, func(c Conn) error {
		if err := checkEpoch(ctx, c, epoch); err != nil {
			return err
		}
		saved, found, err := metaValue(ctx, c, localPolicyPrefix+sessionID)
		if err != nil {
			return err
		}
		if !found || !json.Valid([]byte(saved)) {
			return runtimeErr(ReasonStorageUnavailable)
		}
		if jsonCompareKey(json.RawMessage(saved)) != jsonCompareKey(expected) {
			return nil
		}
		receipt, ok := objectMembers(json.RawMessage(saved))
		if !ok {
			return runtimeErr(ReasonStorageUnavailable)
		}
		children, routed, err := adoptableCompression(ctx, c, receipt)
		if err != nil || len(children) == 0 {
			return err
		}
		tip := children[len(children)-1]
		rawEntry, _ := memberValue(receipt, "entry")
		entry, _ := objectMembers(rawEntry)
		lineage := []string{sessionID}
		if raw, has := memberValue(receipt, "lineage"); has {
			lineage, _ = stringList(raw)
		}
		receipt = withMember(receipt, "entry", rawObject(withMember(entry, "session_id", rawText(tip))))
		receipt = withMember(receipt, "lineage", rawTexts(append(append([]string{}, lineage...), children...)))
		if _, err := validateLocalLineage(ctx, c, receipt); err != nil {
			return err
		}
		return commitLocalTarget(ctx, c, receipt, withMember(routed, "session_id", rawText(tip)))
	})
}
