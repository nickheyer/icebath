package state

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
)

// Meta key prefixes of the terminal tombstones, a stored result, a retired admission, its identity pointer and a retired worker
const (
	resultPrefix            = "gateway.admission.result.v1."
	terminalAdmissionPrefix = "gateway.terminal_admission.v1."
	terminalIdentityPrefix  = "gateway.terminal_identity.v1."
	terminalWorkerPrefix    = "gateway.terminal_worker.v1."
)

// Top level key only the result encoder writes
const storedKey = "_stored"

// Integer a JSON number spells, false for anything that is not a plain integer
func jsonInt(raw json.RawMessage) (int64, bool) {
	t := strings.TrimSpace(string(raw))
	if t == "" || strings.ContainsAny(t, ".eE\"") {
		return 0, false
	}
	n, err := strconv.ParseInt(t, 10, 64)
	return n, err == nil
}

// Reports a JSON string literal
func jsonIsString(raw json.RawMessage) bool {
	t := strings.TrimSpace(string(raw))
	return t != "" && t[0] == '"'
}

// Reports the JSON null literal
func jsonIsNull(raw json.RawMessage) bool { return strings.TrimSpace(string(raw)) == "null" }

// JSON object text of members, each value kept as its own bytes
func rawObject(members []configMember) json.RawMessage {
	var b strings.Builder
	b.WriteByte('{')
	for i, m := range members {
		if i > 0 {
			b.WriteByte(',')
		}
		key, _ := json.Marshal(m.key)
		b.Write(key)
		b.WriteByte(':')
		b.Write(m.value)
	}
	b.WriteByte('}')
	return json.RawMessage(b.String())
}

// JSON array text of elements, each kept as its own bytes
func rawArray(elements []json.RawMessage) json.RawMessage {
	parts := make([]string, len(elements))
	for i, e := range elements {
		parts[i] = string(e)
	}
	return json.RawMessage("[" + strings.Join(parts, ",") + "]")
}

// Members with key set to value, a copy that leaves the input untouched
func withMember(members []configMember, key string, value json.RawMessage) []configMember {
	return setConfigMember(append([]configMember{}, members...), key, value)
}

// Identity pointer key of a retired admission, its principal, target and request
func terminalIdentityKey(principalID, sessionID, requestID string) (string, error) {
	digest, err := AdmissionFingerprint(sessionID, map[string]string{"principal": principalID, "request": requestID})
	return terminalIdentityPrefix + digest, err
}

// Index of the user message a turn is anchored on after compaction rebuilt the list, the exact text first and else the last human turn
func reanchorTurnUserIndex(messages []json.RawMessage, userMessage json.RawMessage) int {
	fallback := -1
	want := ""
	if !jsonAbsent(userMessage) {
		want = jsonCompareKey(userMessage)
	}
	matches := func(c Content) bool {
		enc, err := c.MarshalJSON()
		return err == nil && jsonCompareKey(enc) == want && want != ""
	}
	for i := len(messages) - 1; i >= 0; i-- {
		members, ok := objectMembers(messages[i])
		if !ok {
			continue
		}
		role, _ := memberValue(members, "role")
		if jsonString(role) != "user" {
			continue
		}
		rawContent, _ := memberValue(members, "content")
		content := JSONContent(rawContent)
		if (want == "" && jsonAbsent(rawContent) && jsonAbsent(userMessage)) || matches(content) {
			return i
		}
		kind, _ := memberValue(members, "display_kind")
		flag, _ := memberValue(members, "_compressed_summary")
		split := splitUserOriginatedTurn(userTurn{content: content, displayKind: jsonString(kind), summary: jsonTruthy(flag)})
		if !split.live {
			continue
		}
		if matches(split.liveContent) {
			return i
		}
		if fallback < 0 {
			fallback = i
		}
	}
	return fallback
}

// Result that keeps replay and accounting plus this turn's output, never the cumulative history
func compactResult(result []configMember, userMessage json.RawMessage) []configMember {
	raw, _ := memberValue(result, "result")
	value, ok := objectMembers(raw)
	if !ok {
		return result
	}
	value = dropMember(value, "tools")
	if rawMessages, has := memberValue(value, "messages"); has {
		if messages, isList := arrayElements(rawMessages); isList {
			compressed, _ := memberValue(value, "_compressed")
			suffix, _ := memberValue(value, "_messages_are_turn_suffix")
			if !jsonTruthy(compressed) && !jsonTruthy(suffix) {
				rawAnchor, _ := memberValue(value, "current_turn_user_idx")
				anchor, isInt := jsonInt(rawAnchor)
				valid := false
				if isInt && anchor >= 0 && int(anchor) < len(messages) {
					if at, isObject := objectMembers(messages[anchor]); isObject {
						role, _ := memberValue(at, "role")
						valid = jsonString(role) == "user"
					}
				}
				index := int(anchor)
				if !valid {
					index = reanchorTurnUserIndex(messages, userMessage)
				}
				value = withMember(value, "messages", rawArray(messages[index+1:]))
				if index >= 0 {
					if at, isObject := objectMembers(messages[index]); isObject {
						if api, _ := memberValue(at, "api_content"); jsonTruthy(api) {
							var row []configMember
							for _, key := range []string{"role", "content", "api_content"} {
								v, has := memberValue(at, key)
								if !has {
									v = json.RawMessage("null")
								}
								row = append(row, configMember{key, v})
							}
							value = withMember(value, "_turn_user_row", rawObject(row))
						}
					}
				}
				value = withMember(value, "_messages_are_turn_suffix", json.RawMessage("true"))
				value = dropMember(value, "current_turn_user_idx")
			}
		}
	}
	return withMember(result, "result", rawObject(value))
}

// Stored form of a result, the reply kept once when the closing assistant message repeats it
func encodeResult(result []configMember) []configMember {
	if raw, has := memberValue(result, storedKey); has {
		return withMember(result, storedKey, rawObject([]configMember{{"v", json.RawMessage("1")}, {"raw", raw}}))
	}
	raw, _ := memberValue(result, "result")
	value, ok := objectMembers(raw)
	if !ok {
		return result
	}
	rawMessages, _ := memberValue(value, "messages")
	messages, isList := arrayElements(rawMessages)
	if !isList {
		return result
	}
	reply, _ := memberValue(value, "final_response")
	last := -1
	var lastMembers []configMember
	for i := len(messages) - 1; i >= 0; i-- {
		members, isObject := objectMembers(messages[i])
		if !isObject {
			continue
		}
		if role, _ := memberValue(members, "role"); jsonIsString(role) && jsonString(role) == "assistant" {
			last, lastMembers = i, members
			break
		}
	}
	if !jsonIsString(reply) || jsonString(reply) == "" || last < 0 {
		return result
	}
	content, _ := memberValue(lastMembers, "content")
	if !jsonIsString(content) || jsonString(content) != jsonString(reply) {
		return result
	}
	rewritten := append([]json.RawMessage{}, messages...)
	rewritten[last] = rawObject(withMember(lastMembers, "content", json.RawMessage("null")))
	out := withMember(result, "result", rawObject(withMember(value, "messages", rawArray(rewritten))))
	return setConfigMember(out, storedKey, rawObject([]configMember{{"v", json.RawMessage("1")}, {"reply_ref", json.RawMessage(strconv.Itoa(last))}}))
}

// Reports whether members hold exactly the two given keys
func hasExactKeys(members []configMember, a, b string) bool {
	if len(members) != 2 {
		return false
	}
	_, hasA := memberValue(members, a)
	_, hasB := memberValue(members, b)
	return hasA && hasB
}

// Decoded stored result row, only a row carrying the encoder's exact envelope changing
func loadResult(text string) ([]configMember, bool) {
	result, ok := objectMembers(json.RawMessage(text))
	if !ok {
		return nil, false
	}
	rawEnvelope, _ := memberValue(result, storedKey)
	envelope, isObject := objectMembers(rawEnvelope)
	if !isObject {
		return result, true
	}
	version, _ := memberValue(envelope, "v")
	v, isInt := jsonInt(version)
	if hasExactKeys(envelope, "v", "raw") && isInt && v == 1 {
		raw, _ := memberValue(envelope, "raw")
		return setConfigMember(result, storedKey, raw), true
	}
	rawValue, _ := memberValue(result, "result")
	value, valueIsObject := objectMembers(rawValue)
	rawRef, _ := memberValue(envelope, "reply_ref")
	ref, refIsInt := jsonInt(rawRef)
	rawMessages, _ := memberValue(value, "messages")
	messages, isList := arrayElements(rawMessages)
	reply, _ := memberValue(value, "final_response")
	if !(hasExactKeys(envelope, "v", "reply_ref") && isInt && v == 1 && refIsInt && valueIsObject && isList &&
		ref >= 0 && int(ref) < len(messages) && jsonIsString(reply)) {
		return result, true
	}
	at, isMessage := objectMembers(messages[ref])
	role, _ := memberValue(at, "role")
	content, hasContent := memberValue(at, "content")
	if !isMessage || !jsonIsString(role) || jsonString(role) != "assistant" || !hasContent || !jsonIsNull(content) {
		return result, true
	}
	rewritten := append([]json.RawMessage{}, messages...)
	rewritten[ref] = rawObject(withMember(at, "content", reply))
	result = withMember(result, "result", rawObject(withMember(value, "messages", rawArray(rewritten))))
	return dropMember(result, storedKey), true
}

// Retired admission tombstone of an id, nil when none is stored
func terminalAdmission(ctx context.Context, c Conn, admissionID string) ([]configMember, error) {
	text, ok, err := metaValue(ctx, c, terminalAdmissionPrefix+admissionID)
	if err != nil || !ok {
		return nil, err
	}
	members, isObject := objectMembers(json.RawMessage(text))
	if !isObject {
		return nil, runtimeErr(ReasonStorageUnavailable)
	}
	return members, nil
}

// Hex SHA-256 of an adoption secret, refusing an empty or oversized one
func secretDigest(secret string) (string, error) {
	if err := checkText(secret); err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:]), nil
}

// Retired API admissions that carried a request id, a deleted session's tombstones still spending its idempotency key
func (s *Store) TerminalAPIAdmissions(ctx context.Context, requestID string) ([]Admission, error) {
	var out []Admission
	err := s.Read(ctx, func(c Conn) error {
		out = nil
		values, err := queryStrings(ctx, c, "SELECT value FROM state_meta WHERE key GLOB ? "+
			"AND json_extract(value, '$.principal_id')='api' AND json_extract(value, '$.request_id')=?", terminalAdmissionPrefix+"*", requestID)
		if err != nil {
			return err
		}
		for _, v := range values {
			members, ok := objectMembers(json.RawMessage(v))
			if !ok {
				continue
			}
			out = append(out, admissionOfMembers(members))
		}
		return nil
	})
	return out, err
}

// Exact retry lookup against a retired target, nil for a live target and a refusal for a retired miss or conflict
func (s *Store) RetryTerminalAdmission(ctx context.Context, epoch int64, principalID, sessionID, requestID string, payload json.RawMessage, intent string) (*Admission, error) {
	if err := checkText(principalID, sessionID, requestID); err != nil {
		return nil, err
	}
	if intent == "" {
		intent = "queue"
	}
	encoded, err := canonicalObject(payload)
	if err != nil {
		return nil, err
	}
	digest, err := AdmissionFingerprint(sessionID, map[string]any{"input": json.RawMessage(encoded), "intent": intent})
	if err != nil {
		return nil, err
	}
	identity, err := terminalIdentityKey(principalID, sessionID, requestID)
	if err != nil {
		return nil, err
	}
	var out *Admission
	err = s.Read(ctx, func(c Conn) error {
		out = nil
		if err := checkEpoch(ctx, c, epoch); err != nil {
			return err
		}
		retired, err := metaExists(ctx, c, retiredSessionPrefix+sessionID)
		if err != nil || !retired {
			return err
		}
		saved, ok, err := metaValue(ctx, c, identity)
		if err != nil {
			return err
		}
		if !ok {
			return runtimeErr(ReasonNotFound)
		}
		row, err := terminalAdmission(ctx, c, jsonString(json.RawMessage(saved)))
		if err != nil {
			return err
		}
		if row == nil {
			return runtimeErr(ReasonNotFound)
		}
		stored, _ := memberValue(row, "payload_digest")
		if jsonString(stored) != digest {
			return admittedRequestConflict()
		}
		a := admissionOfMembers(row)
		out = &a
		return nil
	})
	return out, err
}

// Exact retired producer evidence of one worker receipt, possession never reactivating the worker
func (s *Store) TerminalWorkerReceipt(ctx context.Context, executionID, sessionID string, generation, sequence int64, adoptionSecret, payloadDigest string) (json.RawMessage, error) {
	proof, err := secretDigest(adoptionSecret)
	if err != nil {
		return nil, err
	}
	var out json.RawMessage
	err = s.Read(ctx, func(c Conn) error {
		saved, ok, err := metaValue(ctx, c, terminalWorkerPrefix+executionID)
		if err != nil {
			return err
		}
		if !ok {
			return runtimeErr(ReasonNotFound)
		}
		worker, isObject := objectMembers(json.RawMessage(saved))
		if !isObject {
			return runtimeErr(ReasonStorageUnavailable)
		}
		owner, _ := memberValue(worker, "session_id")
		digest, _ := memberValue(worker, "adoption_digest")
		if jsonString(owner) != sessionID || subtle.ConstantTimeCompare([]byte(jsonString(digest)), []byte(proof)) != 1 {
			return runtimeErr(ReasonPermissionDenied)
		}
		rawGeneration, _ := memberValue(worker, "generation")
		if g, isInt := jsonInt(rawGeneration); !isInt || g != generation {
			return runtimeErr(ReasonStaleGeneration)
		}
		rawReceipts, _ := memberValue(worker, "receipts")
		receipts, _ := arrayElements(rawReceipts)
		for _, raw := range receipts {
			receipt, _ := objectMembers(raw)
			rawSequence, _ := memberValue(receipt, "sequence")
			if n, isInt := jsonInt(rawSequence); !isInt || n != sequence {
				continue
			}
			stored, _ := memberValue(receipt, "payload_digest")
			if jsonString(stored) != payloadDigest {
				return runtimeErr(ReasonAdmissionConflict)
			}
			result, _ := memberValue(receipt, "result_json")
			if !jsonIsString(result) {
				return runtimeErr(ReasonStaleGeneration)
			}
			out = json.RawMessage(jsonString(result))
			return nil
		}
		return runtimeErr(ReasonStaleGeneration)
	})
	return out, err
}
