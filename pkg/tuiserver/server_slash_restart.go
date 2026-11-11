package tuiserver

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Replaces a spawned slash worker with a fresh one on the session's current model, none when it never spawned one
func restartSlashWorker(ctx context.Context, sid string, session *tuigateway.Session) {
	worker := session.Get("slash_worker")
	if worker == nil {
		return
	}
	if closer, ok := worker.(interface{ Close() }); ok {
		suppress(closer.Close)
	}
	sessionKey, hasKey := session.Lookup("session_key")
	if !hasKey {
		session.Set("slash_worker", nil)
		return
	}
	model, providerName := "", ""
	if a := sessionAgent(session); a != nil {
		model, providerName = a.Model(), a.Provider()
	} else {
		model = resolveModel(ctx)
	}
	replacement, err := func() (w *slashWorker, err error) {
		defer func() {
			if r := recover(); r != nil {
				w, err = nil, fmt.Errorf("%s", failureText(r))
			}
		}()
		return newSlashWorker(ctx, strOr(sessionKey), model, strOr(session.Get("profile_home")), providerName)
	}()
	if err != nil {
		session.Set("slash_worker", nil)
		return
	}
	attachWorker(sid, session, replacement)
}
