package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/state"
)

// Final path component under the host os path rules
func osPathBasename(p string) string {
	if runtime.GOOS == "windows" {
		return pypath.NtBasename(p)
	}
	return pypath.PosixBasename(p)
}

// Exports the current session and sends it to the chat as a document
func (r *GatewayRunner) handleSaveCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	parts := pystr.SplitWhitespace(event.GetCommandArgs(), -1)
	redact := len(parts) > 0 && (pystr.Lower(parts[len(parts)-1]) == "redact" || pystr.Lower(parts[len(parts)-1]) == "--redact")
	if redact {
		parts = parts[:len(parts)-1]
	}
	if len(parts) == 0 {
		return state.SaveUsage, nil
	}
	format, err := state.NormalizeSaveFormat(parts[0])
	if err != nil {
		if !isValueError(err) {
			return "", err
		}
		return pyrepr.ExceptionMessage(err) + "\n\n" + state.SaveUsage, nil
	}
	source := event.Source
	entry, err := r.slashStore().GetOrCreateSession(ctx, source, false, true)
	if err != nil {
		return "", err
	}
	sessionID := entry.SessionID
	asyncDB := r.sessionDB(ctx)
	if asyncDB == nil {
		return i18n.T(ctx, "gateway.shared.session_db_unavailable", "", nil), nil
	}
	filename := state.DefaultSaveFilename(sessionID, format)
	if len(parts) > 1 {
		filename = parts[1]
	}
	if filename = osPathBasename(filename); filename == "" {
		filename = state.DefaultSaveFilename(sessionID, format)
	}
	exportData, err := state.AsyncCall(ctx, asyncDB, func(d *state.SessionDB) (*pyjson.Object, error) {
		return state.LoadSaveSnapshot(ctx, d, sessionID, format)
	})
	if err != nil {
		var tooLarge *state.SessionExportTooLargeError
		if errors.As(err, &tooLarge) {
			return pyrepr.ExceptionMessage(err), nil
		}
		return "", err
	}
	if exportData == nil || exportData.Len() == 0 {
		return i18n.T(ctx, "gateway.save.no_messages", "", map[string]any{"session_id": sessionID}), nil
	}
	if redact {
		exportData = memory.RedactSessionData(exportData)
	}
	tempDir, err := os.MkdirTemp("", "icebath_save_")
	if err != nil {
		return "", err
	}
	tempPath := filepath.Join(tempDir, filename)
	defer func() {
		if os.Remove(tempPath) == nil {
			_ = os.Remove(tempDir)
		}
	}()
	reply, err := func() (string, error) {
		if _, err := r.runInExecutorWithContext(ctx, func(context.Context) (any, error) {
			rendered, err := state.RenderSessionForSave(exportData, format)
			if err != nil {
				return nil, err
			}
			return nil, os.WriteFile(tempPath, []byte(rendered), 0o644)
		}); err != nil {
			return "", err
		}
		adapter, err := r.deliveryAdapterFor(ctx, source)
		if err != nil {
			return "", err
		}
		if adapter == nil {
			return i18n.T(ctx, "gateway.save.no_adapter", "", nil), nil
		}
		if _, err := adapter.SendDocument(ctx, source.ChatID, tempPath, i18n.T(ctx, "gateway.save.caption", "", map[string]any{"filename": filename}),
			filename, "", nil, nil); err != nil {
			return "", err
		}
		return i18n.T(ctx, "gateway.save.complete", "", nil), nil
	}()
	if err != nil {
		runLog().Warn(fmt.Sprintf("Session /save failed: %s", pyrepr.ExceptionMessage(err)))
		return i18n.T(ctx, "gateway.save.failed", "", map[string]any{"error": pyrepr.ExceptionMessage(err)}), nil
	}
	return reply, nil
}

// Sets or shows the current session's title, persisting the messaging origin for later resume scoping
func (r *GatewayRunner) handleTitleCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	source := event.Source
	entry, err := r.slashStore().GetOrCreateSession(ctx, source, false, true)
	if err != nil {
		return "", err
	}
	sessionID := entry.SessionID
	asyncDB := r.sessionDB(ctx)
	if asyncDB == nil {
		return sessionDBUnavailableReply(ctx), nil
	}
	db := asyncDB.DB()
	if _, has, err := db.GetSessionTitle(ctx, sessionID); err != nil {
		return "", err
	} else if !has {
		platform := string(source.Platform)
		if platform == "" {
			platform = "unknown"
		}
		_ = db.CreateSession(state.NewSession{ID: sessionID, Source: platform, UserID: sattr(source.UserID), ChatID: source.ChatID,
			ChatType: source.ChatType, ThreadID: sattr(source.ThreadID)})
	}
	titleArg := pystr.Strip(event.GetCommandArgs())
	if titleArg == "" {
		title, _, err := db.GetSessionTitle(ctx, sessionID)
		if err != nil {
			return "", err
		}
		if title != "" {
			return i18n.T(ctx, "gateway.title.current_with_title", "", map[string]any{"session_id": sessionID, "title": title}), nil
		}
		return i18n.T(ctx, "gateway.title.current_no_title", "", map[string]any{"session_id": sessionID}), nil
	}
	sanitized, err := state.SanitizeTitle(titleArg)
	if err != nil {
		return i18n.T(ctx, "gateway.shared.warn_passthrough", "", map[string]any{"error": pyrepr.ExceptionMessage(err)}), nil
	}
	if sanitized == "" {
		return i18n.T(ctx, "gateway.title.empty_after_clean", "", nil), nil
	}
	ok, err := state.AsyncCall(ctx, asyncDB, func(d *state.SessionDB) (bool, error) { return d.SetSessionTitle(ctx, sessionID, sanitized) })
	if err != nil {
		if !isValueError(err) {
			return "", err
		}
		return i18n.T(ctx, "gateway.shared.warn_passthrough", "", map[string]any{"error": pyrepr.ExceptionMessage(err)}), nil
	}
	if !ok {
		return i18n.T(ctx, "gateway.title.not_found", "", nil), nil
	}
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				runLog().Debug("Failed to rename Telegram topic from /title", "exc_info", rec)
			}
		}()
		r.scheduleTelegramTopicTitleRename(ctx, source, sessionID, sanitized)
	}()
	return i18n.T(ctx, "gateway.title.set_to", "", map[string]any{"title": sanitized}), nil
}
