package app

import (
	"context"
	"errors"
	"strings"

	"github.com/nickheyer/icebath/pkg/activesessions"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// Refuses resuming a session another runtime of this profile owns unless an explicit gateway URL is set
func refuseOwnedTUIResume(env map[string]string, sessionID, registryHome string) error {
	if sessionID == "" || strings.TrimSpace(env["ICEBATH_TUI_GATEWAY_URL"]) != "" {
		return nil
	}
	if registryHome == "" {
		registryHome = home.Dir(context.Background())
	}
	profileHome, err := pypath.Resolve(registryHome)
	if err != nil {
		return err
	}
	entries, err := activesessions.RegistrySnapshot(context.Background(), profileHome, true)
	if err != nil {
		return err
	}
	var owners []*pyjson.Object
	for _, entry := range entries {
		if pyjson.Equal(entry.GetOr("session_id", nil), sessionID) {
			owners = append(owners, entry)
		}
	}
	if len(owners) == 0 {
		return nil
	}
	if len(owners) != 1 {
		return errors.New("Session owner identity is ambiguous; nothing was resumed.")
	}
	return errors.New("This chat is open in another Icebath window/terminal. Close the chat " +
		"there and run icebath --resume " + sessionID + " here to take it over.\n" +
		activesessions.SessionOwnerDetails(sessionID, owners[0]))
}
