package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gateway/platforms/slack/slacktest"
	"github.com/nickheyer/icebath/pkg/testfakes"
)

// Site hook routing a gateway child's slack.com traffic to the parent's loopback Slack
const slackLoopbackSite = "slack_loopback"

func init() { siteHooks[slackLoopbackSite] = slacktest.RouteFromEnv }

// The gateway process builds Slack from the plugin registry, connects Socket Mode and answers a DM through a model turn
func TestGatewayRunsSlackFromThePluginRegistryThroughATurn(t *testing.T) {
	model, err := testfakes.NewFakeLLM([]any{testfakes.Text{Text: "pong from the model"}}, testfakes.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer model.Stop()
	slack := slacktest.New(t)
	slack.Respond("auth.test", map[string]any{"ok": true, "user_id": "U_BOT", "user": "icebath", "team_id": "T_E2E",
		"team": "E2E Workspace", "bot_id": "B_BOT"})
	tmp, h, user := gatewayCaseHomes(t, map[string]any{})
	if err := testfakes.WriteFakeLLMHome(h, model.BaseURL(), "", "auxiliary:\n  title_generation:\n    enabled: false\n", 0); err != nil {
		t.Fatal(err)
	}
	dotenv := "SLACK_BOT_TOKEN=xoxb-e2e-token\nSLACK_APP_TOKEN=xapp-e2e-token\nSLACK_ALLOWED_USERS=U_HUMAN\n"
	f, err := os.OpenFile(filepath.Join(h, ".env"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(dotenv); err != nil {
		t.Fatal(err)
	}
	f.Close()
	routing, err := slack.ChildEnv(tmp)
	if err != nil {
		t.Fatal(err)
	}
	env := gatewayChildEnv(h, user, append(routing, testSiteEnv+"="+slackLoopbackSite)...)
	child := startGatewayChild(t, filepath.Join(tmp, "gateway.log"), env, gateway.GatewayRunModuleEntry)
	conn, err := slack.NextConn(60 * time.Second)
	if err != nil {
		t.Fatalf("%v\n%s", err, child.output())
	}
	if opens := slack.CallsTo("apps.connections.open"); len(opens) != 1 || opens[0].Header.Get("Authorization") != "Bearer xapp-e2e-token" {
		t.Fatalf("apps.connections.open calls = %#v", opens)
	}
	auth := slack.CallsTo("auth.test")
	if len(auth) == 0 || auth[0].Header.Get("Authorization") != "Bearer xoxb-e2e-token" {
		t.Fatalf("auth.test calls = %#v", auth)
	}
	envelope, err := conn.SendEvent("T_E2E", map[string]any{"type": "message", "channel": "D_E2E", "channel_type": "im",
		"user": "U_HUMAN", "text": "ping", "ts": "1700000000.000100", "client_msg_id": "c-e2e-1", "team": "T_E2E"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Ack(envelope, 30*time.Second); err != nil {
		t.Fatalf("%v\n%s", err, child.output())
	}
	deadline := time.Now().Add(90 * time.Second)
	for {
		var reply string
		for _, call := range slack.CallsTo("chat.postMessage") {
			if text, _ := call.Args["text"].(string); call.Args["channel"] == "D_E2E" && strings.Contains(text, "pong from the model") {
				reply = text
			}
		}
		if reply != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no reply reached the DM, calls %#v\n%s", slack.AllCalls(), child.output())
		}
		time.Sleep(100 * time.Millisecond)
	}
	requests := model.MainRequests()
	if len(requests) != 1 || !strings.Contains(fmt.Sprint(requests[0]["messages"]), "ping") {
		t.Fatalf("model requests = %v", requests)
	}
}
