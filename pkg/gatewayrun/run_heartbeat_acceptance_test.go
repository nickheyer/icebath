package gatewayrun

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

func TestInflightHeartbeatOnTheLogicalOwnerBlocksAnother(t *testing.T) {
	envelopes := map[string]func() *pyjson.Object{
		"native": func() *pyjson.Object {
			return pyjson.ObjectOf("text", "check", "native_text_v1", pyjson.ObjectOf("automation", pyjson.ObjectOf("heartbeat", "root")))
		},
		"local": func() *pyjson.Object {
			return pyjson.ObjectOf("text", "check", "local_automation_v1", pyjson.ObjectOf("heartbeat", "root"))
		},
	}
	for name, payload := range envelopes {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("ICEBATH_HOME", dir)
			ctx := context.Background()
			db, err := state.NewSessionDB(ctx, filepath.Join(dir, "state.db"), false)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := db.CreateSession(state.NewSession{ID: "root", Source: "telegram"}); err != nil {
				t.Fatal(err)
			}
			model, prompt := "offline", "offline"
			if err := db.PublishCompressionChild(ctx, state.CompressionChildPublish{ParentSessionID: "root", ChildSessionID: "tip",
				Source: "telegram", Model: &model, ModelConfig: pyjson.NewObject(), SystemPrompt: &prompt,
				Messages: []map[string]any{{"role": "user", "content": "retained"}}}); err != nil {
				t.Fatal(err)
			}
			epoch, err := db.BeginRuntimeEpoch(ctx, "test")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.AdmitSessionInput(ctx, state.SessionInput{Epoch: epoch, PrincipalID: "automation:route", SessionID: "root",
				RequestID: "hb-1", Payload: payload()}); err != nil {
				t.Fatal(err)
			}
			heartbeat := agent.NewHeartbeatState("check", 60)
			heartbeat.CreatedAt = 1
			agent.SaveHeartbeat(ctx, "tip", heartbeat)
			r := allocGatewayRunner()
			r.config = gateway.DefaultGatewayConfig(ctx)
			r.SetSessionAuthority(gateway.NewSessionAuthority(r, "default", "test", db, epoch))
			source := &gateway.SessionSource{Platform: gateway.PlatformTelegram, ChatID: "1"}
			user := "1"
			source.UserID = &user
			if err := admitHeartbeat(ctx, r, nil, source, "tip", "route"); err != nil {
				t.Fatal(err)
			}
			rows, err := db.ListSessionAdmissions(ctx, "root", false)
			if err != nil || len(rows) != 1 {
				t.Fatalf("admissions = %d, %v", len(rows), err)
			}
			if _, due := agent.NewHeartbeatManager(ctx, "tip").DuePrompt(nil); !due {
				t.Fatal("the coalesced tick must stay due")
			}
		})
	}
}
