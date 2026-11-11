package agent

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Backfills the owner reserved row with the constructor facts, the identity fields only confirming the reservation
func (s *RuntimeSessionStore) CreateSession(n state.NewSession) error {
	if err := s.session(n.ID); err != nil {
		return err
	}
	var config any
	if n.ModelConfig != nil {
		config, _ = pyjson.AsObject(n.ModelConfig)
	}
	payload := pyjson.ObjectOf("source", n.Source, "model", noneIfEmpty(n.Model), "model_config", config,
		"system_prompt", noneIfEmpty(n.SystemPrompt), "user_id", noneIfEmpty(n.UserID), "session_key", noneIfEmpty(n.SessionKey),
		"chat_id", noneIfEmpty(n.ChatID), "chat_type", noneIfEmpty(n.ChatType), "thread_id", noneIfEmpty(n.ThreadID),
		"parent_session_id", noneIfEmpty(n.ParentSessionID), "cwd", noneIfEmpty(n.Cwd), "profile_name", noneIfEmpty(n.ProfileName),
		"git_repo_root", noneIfEmpty(n.GitRepoRoot), "origin_json", noneIfEmpty(n.OriginJSON), "display_name", noneIfEmpty(n.DisplayName))
	_, err := s.applyValue(context.Background(), "session.create", payload)
	return err
}

// Value of a title receipt, its error raised as ValueError
func titleResult(result any) (bool, error) {
	obj, ok := result.(*pyjson.Object)
	if !ok {
		return false, &workerStoreError{"AttributeError", fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(result))}
	}
	if failure := obj.GetOr("error", nil); pyrepr.Truthy(failure) {
		return false, &workerStoreError{"ValueError", pyrepr.Str(failure)}
	}
	value, err := receiptItem(result, "value")
	return pyrepr.Truthy(value), err
}

// Sets a user title through the owner
func (s *RuntimeSessionStore) SetSessionTitle(ctx context.Context, sessionID, title string) (bool, error) {
	if err := s.session(sessionID); err != nil {
		return false, err
	}
	result, err := s.apply(ctx, "session.title", pyjson.ObjectOf("title", title, "source", "user"))
	if err != nil {
		return false, err
	}
	return titleResult(result)
}

// Sets a derived or llm title through the owner, refusing any other source
func (s *RuntimeSessionStore) SetAutoTitleContext(ctx context.Context, sessionID, title, source string) (bool, error) {
	if source != "derived" && source != "llm" {
		return false, &workerStoreError{"ValueError", "invalid automatic title source: " + pyrepr.Repr(source)}
	}
	if err := s.session(sessionID); err != nil {
		return false, err
	}
	result, err := s.apply(ctx, "session.title", pyjson.ObjectOf("title", title, "source", source))
	if err != nil {
		return false, err
	}
	return titleResult(result)
}

// Automatic title without a caller context
func (s *RuntimeSessionStore) SetAutoTitle(sessionID, title, source string) (bool, error) {
	return s.SetAutoTitleContext(context.Background(), sessionID, title, source)
}

// Provenance of the session title, absent while the row has no title
func (s *RuntimeSessionStore) GetSessionTitleSource(ctx context.Context, sessionID string) (string, bool, error) {
	row, err := s.sessionRow(ctx, sessionID)
	if err != nil {
		return "", false, err
	}
	if row.GetOr("title", nil) == nil {
		return "", false, nil
	}
	source := row.GetOr("title_source", nil)
	if source == nil {
		return "", false, nil
	}
	text, err := receiptText(source)
	return text, err == nil, err
}

// Sets the title provenance through the owner
func (s *RuntimeSessionStore) SetSessionTitleSource(ctx context.Context, sessionID, source string) (bool, error) {
	if source != "user" && source != "derived" && source != "llm" {
		return false, &workerStoreError{"ValueError", "invalid title source: " + pyrepr.Repr(source)}
	}
	if err := s.session(sessionID); err != nil {
		return false, err
	}
	value, err := s.applyValue(ctx, "session.title_source", pyjson.ObjectOf("source", source))
	return pyrepr.Truthy(value), err
}

// Next free numbered title in the base title's lineage
func (s *RuntimeSessionStore) GetNextTitleInLineage(ctx context.Context, baseTitle string) (string, error) {
	value, err := s.applyValue(ctx, "session.next_title", pyjson.ObjectOf("base_title", baseTitle))
	if err != nil {
		return "", err
	}
	return receiptText(value)
}

// Stamps the session's last activity through the owner
func (s *RuntimeSessionStore) TouchSessionActivity(sessionID string, at *float64, description string, provenance state.ActivityProvenance) error {
	if err := s.session(sessionID); err != nil {
		return err
	}
	_, err := s.apply(context.Background(), "session.activity", pyjson.ObjectOf("ts", noneIfNil(at), "description", noneIfEmpty(description),
		"provenance", noneIfEmpty(string(provenance))))
	return err
}

// Clears the activity labels after a turn through the owner
func (s *RuntimeSessionStore) ClearSessionActivityLabels(sessionID string) error {
	if err := s.session(sessionID); err != nil {
		return err
	}
	_, err := s.apply(context.Background(), "session.activity_clear", pyjson.NewObject())
	return err
}

// Switches the billing route after every earlier usage receipt landed
func (s *RuntimeSessionStore) UpdateSessionBillingRoute(ctx context.Context, sessionID, provider, baseURL string, billingMode *string) error {
	if err := s.session(sessionID); err != nil {
		return err
	}
	_, err := s.apply(ctx, "session.billing_route", pyjson.ObjectOf("provider", provider, "base_url", baseURL, "billing_mode", noneIfNil(billingMode)))
	return err
}

// Backfills the newest user row's API content through the owner
func (s *RuntimeSessionStore) SetLatestUserAPIContent(ctx context.Context, sessionID string, content any, apiContent string) (int64, error) {
	if err := s.session(sessionID); err != nil {
		return 0, err
	}
	value, err := s.applyValue(ctx, "session.api_content", pyjson.ObjectOf("content", content, "api_content", apiContent))
	if err != nil {
		return 0, err
	}
	return receiptInt(value)
}

// Backfills one addressed row's API content through the owner
func (s *RuntimeSessionStore) SetMessageAPIContent(ctx context.Context, sessionID string, rowID int64, content any, apiContent string) (int64, error) {
	if err := s.session(sessionID); err != nil {
		return 0, err
	}
	value, err := s.applyValue(ctx, "session.message_api_content", pyjson.ObjectOf("row_id", rowID, "content", content, "api_content", apiContent))
	if err != nil {
		return 0, err
	}
	return receiptInt(value)
}

// Rewrites one active user row through the owner, display metadata only when given
func (s *RuntimeSessionStore) SetUserMessageContent(ctx context.Context, sessionID string, rowID int64, content any, displayMetadata any) (int64, error) {
	if err := s.session(sessionID); err != nil {
		return 0, err
	}
	payload := pyjson.ObjectOf("row_id", rowID, "content", content)
	if displayMetadata != nil {
		payload.Set("display_metadata", displayMetadata)
	}
	value, err := s.applyValue(ctx, "session.user_content", payload)
	if err != nil {
		return 0, err
	}
	return receiptInt(value)
}

// Ends the assigned session through the owner
func (s *RuntimeSessionStore) EndSession(sessionID, endReason string) error {
	if err := s.session(sessionID); err != nil {
		return err
	}
	_, err := s.apply(context.Background(), "session.end", pyjson.ObjectOf("end_reason", endReason))
	return err
}

// Lifecycle status of the assigned session, the only one a worker may ask about
func (s *RuntimeSessionStore) SessionLifecycleStatuses(ctx context.Context, sessionIDs []string) (map[string]string, error) {
	var ids []string
	for _, sid := range sessionIDs {
		if sid != "" {
			ids = append(ids, sid)
		}
	}
	for _, sid := range ids {
		if err := s.session(sid); err != nil {
			return nil, err
		}
	}
	if len(ids) == 0 {
		return map[string]string{}, nil
	}
	value, err := s.applyValue(ctx, "session.lifecycle", pyjson.NewObject())
	if err != nil {
		return nil, err
	}
	status, err := receiptText(value)
	if err != nil {
		return nil, err
	}
	return map[string]string{s.scopeSessionID(): status}, nil
}
