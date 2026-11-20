package discord

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestSafeSyncDeletesBeforeCreating(t *testing.T) {
	a, api := sendAdapter(t)
	prev := commandSyncMutationInterval
	commandSyncMutationInterval = 0
	t.Cleanup(func() { commandSyncMutationInterval = prev })
	a.client.State.Application = &discordgo.Application{ID: "777000"}
	var existing []string
	for i := 0; i < 100; i++ {
		existing = append(existing, fmt.Sprintf(`{"id":"%d","type":1,"name":"cmd_%d","description":"d"}`, 5000+i, i))
	}
	api.mu.Lock()
	api.get = map[string]string{"/applications/777000/commands": "[" + strings.Join(existing, ",") + "]"}
	api.mu.Unlock()
	for i := 1; i < 100; i++ {
		a.desiredCommands = append(a.desiredCommands, map[string]any{"type": int64(1), "name": fmt.Sprintf("cmd_%d", i), "description": "d"})
	}
	a.desiredCommands = append(a.desiredCommands, map[string]any{"type": int64(1), "name": "cmd_new", "description": "d"})
	if _, err := a.safeSyncSlashCommands(context.Background()); err != nil {
		t.Fatal(err)
	}
	lastDelete, firstCreate, deletes, creates := -1, -1, 0, 0
	api.mu.Lock()
	for i, c := range api.calls {
		switch {
		case c.Method == http.MethodDelete && strings.HasPrefix(c.Path, "/applications/777000/commands/"):
			deletes++
			lastDelete = i
		case c.Method == http.MethodPost && c.Path == "/applications/777000/commands":
			creates++
			if firstCreate < 0 {
				firstCreate = i
			}
		}
	}
	api.mu.Unlock()
	if deletes < 1 || creates < 1 || lastDelete > firstCreate {
		t.Fatalf("deletes %d creates %d last delete %d first create %d", deletes, creates, lastDelete, firstCreate)
	}
}
