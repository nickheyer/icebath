package gateway

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/mediacache"
	"github.com/nickheyer/icebath/pkg/state"
)

// Seconds between two settled media sweeps of one store
const mediaSweepIntervalS = 3600

var (
	mediaSweepMu   sync.Mutex
	mediaLastSweep = map[string]float64{}
)

// Collection of a terminal admission's media, replaced by tests that fail the cleanup write
var releaseAdmissionMedia func(ctx context.Context, db *state.SessionDB, admissionID string) error

func init() { releaseAdmissionMedia = collectAdmissionMedia }

// Deletes the terminal admission's native bytes no other retained input holds
func collectAdmissionMedia(ctx context.Context, db *state.SessionDB, admissionID string) error {
	_, err := releaseAdmissionMediaCount(ctx, db, admissionID)
	return err
}

// Count of files a terminal admission's settlement released, compacting a settled API payload first
func releaseAdmissionMediaCount(ctx context.Context, db *state.SessionDB, admissionID string) (int, error) {
	row, err := db.GetSessionAdmission(ctx, admissionID)
	if err != nil {
		return 0, err
	}
	if row == nil || rowString(row, "status") != AdmissionTerminal {
		return 0, nil
	}
	payloadValue, _ := row.Get("payload")
	payload, _ := payloadValue.(*pyjson.Object)
	if payload == nil {
		payload = pyjson.NewObject()
	}
	if apiValue, ok := payload.Get("api_turn_v1"); ok {
		if apiTurn, ok := apiValue.(*pyjson.Object); ok {
			media, _ := apiTurn.Get("media")
			history, _ := apiTurn.Get("history")
			if _, isList := history.([]any); pyTruthy(media) || isList {
				if _, err := compactSettledAPIPayloads(ctx, db, &admissionID); err != nil {
					return 0, err
				}
			}
		}
	}
	native, err := AdmissionMediaReferences(payload)
	if err != nil {
		return 0, err
	}
	released, err := releaseUnheldMedia(ctx, db, referenceList(native), true)
	if err != nil {
		return 0, err
	}
	hosted, err := hostedDocumentReferences(ctx, rowString(row, "request_id"), payload)
	if err != nil {
		return 0, err
	}
	more, err := releaseUnheldMedia(ctx, db, referenceList(hosted), true)
	if err != nil {
		return 0, err
	}
	swept, err := sweepSettledMedia(ctx, db, nil)
	if err != nil {
		return 0, err
	}
	return released + more + swept, nil
}

// References as the release pass takes them
func referenceList(references []*pyjson.Object) []any {
	out := make([]any, len(references))
	for i, reference := range references {
		out[i] = reference
	}
	return out
}

// Collects settled native and attachment inputs older than the media cache age, at most hourly per store
func sweepSettledMedia(ctx context.Context, db *state.SessionDB, now *float64) (int, error) {
	current := float64(time.Now().UnixNano()) / 1e9
	if now != nil {
		current = *now
	}
	mediaSweepMu.Lock()
	if current-mediaLastSweep[db.Path()] < mediaSweepIntervalS {
		mediaSweepMu.Unlock()
		return 0, nil
	}
	mediaLastSweep[db.Path()] = current
	mediaSweepMu.Unlock()
	var encoded []string
	err := db.ReadCtx(ctx, func(conn state.SQLiteConn) error {
		encoded = encoded[:0]
		rows, err := conn.QueryContext(ctx, `SELECT json_extract(payload_json, '$.attachments_v1.media', '$.native_text_v1.media')
            FROM session_admissions WHERE status='terminal' AND (json_type(payload_json, '$.attachments_v1.media')
            IS NOT NULL OR json_type(payload_json, '$.native_text_v1.media') IS NOT NULL)`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var value string
			if err := rows.Scan(&value); err != nil {
				return err
			}
			encoded = append(encoded, value)
		}
		return rows.Err()
	})
	if err != nil {
		return 0, err
	}
	cutoff := current - mediacache.MediaCacheMaxAgeHours*3600
	stale := map[string]any{}
	var order []string
	for _, text := range encoded {
		decoded, err := pyjson.Loads(text)
		if err != nil {
			return 0, err
		}
		pair, _ := decoded.([]any)
		for _, side := range pair {
			if !pyTruthy(side) {
				continue
			}
			references, err := jsonList(side)
			if err != nil {
				return 0, err
			}
			for _, reference := range references {
				raw, err := referenceItem(reference, "path")
				if err != nil {
					return 0, err
				}
				path, ok := raw.(string)
				if !ok {
					return 0, pathArgError(raw)
				}
				info, err := os.Lstat(pypath.PathStr(path))
				if err != nil {
					continue
				}
				if float64(info.ModTime().UnixNano())/1e9 < cutoff {
					if _, seen := stale[path]; !seen {
						order = append(order, path)
					}
					stale[path] = reference
				}
			}
		}
	}
	if len(stale) == 0 {
		return 0, nil
	}
	references := make([]any, 0, len(order))
	for _, path := range order {
		references = append(references, stale[path])
	}
	return releaseUnheldMedia(ctx, db, references, false)
}

// Collects retained API images of the active profile store that no admission holds any more
func collectUnheldAPIImages(ctx context.Context, db *state.SessionDB) (int, error) {
	store, err := pypath.Resolve(db.Path())
	if err != nil {
		return 0, err
	}
	active, err := pypath.Resolve(home.Dir(ctx))
	if err != nil {
		return 0, err
	}
	if !samePath(pypath.PathParent(store), active) {
		slog.Warn("Skipped retained API image collection: " + db.Path() + " is not the active profile store")
		return 0, nil
	}
	root, err := nativeInputsRoot(ctx)
	if err != nil {
		return 0, err
	}
	var references []any
	if info, statErr := os.Stat(root); statErr == nil && info.IsDir() {
		matches, _ := filepath.Glob(filepath.Join(root, "*", "api_*"))
		for _, path := range matches {
			name, parent := filepath.Base(path), filepath.Base(filepath.Dir(path))
			if ingressAPIImageName.MatchString(name) && len(parent) >= 32 && name[4:36] == parent[:32] {
				references = append(references, pyjson.ObjectOf("path", path, "sha256", parent))
			}
		}
	}
	released, err := releaseUnheldMedia(ctx, db, references, true)
	if err != nil {
		if class, ok := state.SQLiteClassOf(err); ok && class != state.SQLiteInterfaceError {
			slog.Error("API image collection failed; files stay for the next sweep", "error", err)
			return 0, nil
		}
		return 0, err
	}
	return released, nil
}
