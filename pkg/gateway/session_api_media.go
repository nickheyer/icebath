package gateway

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pybase64"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

// File extension of each image mime an API turn may commit
var apiMimeExt = map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp"}

// Reference prefix a settled admission's inline image is reduced to
const apiRetainedPrefix = "hermes-retained:"

// Terminal API admissions still holding inline image bytes or a non responses caller history
const apiCompactCandidates = "SELECT admission_id FROM session_admissions WHERE status='terminal' AND principal_id='api' " +
	"AND ((json_type(payload_json, '$.api_turn_v1.media') IS NOT NULL " +
	"AND payload_json LIKE '%data:image/%') OR (json_type(payload_json, '$.api_turn_v1.history')='array' " +
	"AND request_id NOT LIKE 'responses:%'))"

// Mime and bytes of a committable base64 image data URL, false for anything else
func apiDataURLBytes(url string) (string, []byte, bool) {
	header, encoded, _ := strings.Cut(url, ",")
	runes := []rune(header)
	rest := ""
	if len(runes) > len("data:") {
		rest = string(runes[len("data:"):])
	}
	mimeText, _, _ := strings.Cut(rest, ";")
	mime := pystr.Lower(pystr.Strip(mimeText))
	_, known := apiMimeExt[mime]
	if !strings.HasPrefix(pystr.Lower(url), "data:") || !known || !strings.Contains(pystr.Lower(header), ";base64") {
		return "", nil, false
	}
	decoded, err := pybase64.B64DecodeValidate(encoded)
	if err != nil {
		return "", nil, false
	}
	return mime, decoded, true
}

// Image URL of an image_url content part, empty for any other part
func apiPartImageURL(part any) (string, error) {
	obj, ok := part.(*pyjson.Object)
	if !ok {
		return "", nil
	}
	if kind, _ := obj.Get("type"); kind != "image_url" {
		return "", nil
	}
	raw, _ := obj.Get("image_url")
	if !pyTruthy(raw) {
		return "", nil
	}
	image, ok := raw.(*pyjson.Object)
	if !ok {
		return "", &PyError{Kind: "AttributeError", Msg: "'" + jsonValueTypeName(raw) + "' object has no attribute 'get'"}
	}
	url, found := image.Get("url")
	if !found {
		return "", nil
	}
	text, ok := url.(string)
	if !ok {
		return "", &PyError{Kind: "AttributeError", Msg: "'" + jsonValueTypeName(url) + "' object has no attribute 'partition'"}
	}
	return text, nil
}

// Content with each committed inline image reduced to its digest reference, nil when nothing changes
func apiCompacted(content, media any) ([]any, error) {
	parts, isList := content.([]any)
	references, _ := media.([]any)
	if !isList || !pyTruthy(media) {
		return nil, nil
	}
	next := 0
	out := make([]any, 0, len(parts))
	changed := false
	for _, part := range parts {
		url, err := apiPartImageURL(part)
		if err != nil {
			return nil, err
		}
		var decoded []byte
		ok := false
		if url != "" {
			_, decoded, ok = apiDataURLBytes(url)
		}
		var digest any
		if ok && next < len(references) {
			reference, isObj := references[next].(*pyjson.Object)
			next++
			if !isObj {
				return nil, &PyError{Kind: "TypeError", Msg: "'" + jsonValueTypeName(references[next-1]) + "' object is not subscriptable"}
			}
			value, found := reference.Get("sha256")
			if !found {
				return nil, &pyKeyError{"sha256"}
			}
			digest = value
		}
		sum := sha256.Sum256(decoded)
		if text, isText := digest.(string); ok && isText && hex.EncodeToString(sum[:]) == text {
			obj := part.(*pyjson.Object).Copy()
			rawImage, _ := obj.Get("image_url")
			image := rawImage.(*pyjson.Object).Copy()
			image.Set("url", apiRetainedPrefix+text)
			obj.Set("image_url", image)
			part = obj
			changed = true
		}
		out = append(out, part)
	}
	if !changed {
		return nil, nil
	}
	return out, nil
}

// Drops the inline image copies and caller history terminal API admissions no longer need, returning rows rewritten
func compactSettledAPIPayloads(ctx context.Context, db *state.SessionDB, admissionID *string) (int, error) {
	var ids []string
	if admissionID == nil {
		err := db.ReadCtx(ctx, func(conn state.SQLiteConn) error {
			ids = nil
			rows, err := conn.QueryContext(ctx, apiCompactCandidates)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					return err
				}
				ids = append(ids, id)
			}
			return rows.Err()
		})
		if err != nil {
			return 0, err
		}
	} else {
		ids = []string{*admissionID}
	}
	if len(ids) == 0 {
		return 0, nil
	}
	compacted := 0
	err := db.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
		compacted = 0
		query := strings.Replace(apiCompactCandidates, "SELECT admission_id", "SELECT payload_json, request_id", 1) + " AND admission_id=?"
		for _, rowID := range ids {
			var payloadJSON, requestID string
			if err := conn.QueryRowContext(ctx, query, rowID).Scan(&payloadJSON, &requestID); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					continue
				}
				return err
			}
			changed, payload, err := compactAPIPayload(payloadJSON, requestID)
			if err != nil {
				return err
			}
			if !changed {
				continue
			}
			text, err := state.RuntimeJSON(payload)
			if err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, "UPDATE session_admissions SET payload_json=? WHERE admission_id=?", text, rowID); err != nil {
				return err
			}
			compacted++
		}
		return nil
	})
	return compacted, err
}

// Payload with its committed images compacted and its caller history dropped, false when neither applies
func compactAPIPayload(payloadJSON, requestID string) (bool, *pyjson.Object, error) {
	decoded, err := pyjson.Loads(payloadJSON)
	if err != nil {
		return false, nil, err
	}
	payload, ok := decoded.(*pyjson.Object)
	if !ok {
		return false, nil, &PyError{Kind: "TypeError", Msg: "'" + jsonValueTypeName(decoded) + "' object is not subscriptable"}
	}
	rawData, found := payload.Get("api_turn_v1")
	if !found {
		return false, nil, &pyKeyError{"api_turn_v1"}
	}
	data, ok := rawData.(*pyjson.Object)
	if !ok {
		return false, nil, &PyError{Kind: "AttributeError", Msg: "'" + jsonValueTypeName(rawData) + "' object has no attribute 'get'"}
	}
	content, _ := payload.Get("text")
	media, _ := data.Get("media")
	text, err := apiCompacted(content, media)
	if err != nil {
		return false, nil, err
	}
	history, _ := data.Get("history")
	_, isList := history.([]any)
	drop := isList && !strings.HasPrefix(requestID, "responses:")
	if text == nil && !drop {
		return false, payload, nil
	}
	rebuilt := payload.Copy()
	if text != nil {
		rebuilt.Set("text", text)
	} else {
		rebuilt.Set("text", content)
	}
	if drop {
		trimmed := data.Copy()
		trimmed.Set("history", nil)
		rebuilt.Set("api_turn_v1", trimmed)
	}
	return true, rebuilt, nil
}
