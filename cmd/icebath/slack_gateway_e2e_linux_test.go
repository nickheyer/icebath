//go:build linux

package main

import (
	"encoding/json"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway/platforms/slack/slacktest"
)

// Site hook routing a gateway child's slack.com traffic to the parent's loopback Slack
const slackLoopbackSite = "slack_loopback"

// Identities of the loopback workspace, its bot and the allowlisted human
const (
	slackTeamID    = "T0STANDIN"
	slackBotUserID = "UBOT0001"
	slackBotID     = "B0STANDIN"
	slackBotName   = "icebath"
	slackBotToken  = "xoxb-standin"
	slackAppToken  = "xapp-standin"
	slackUserID    = "U0000111"
)

// Budget of one gateway turn seen from the workspace
const slackTurnTimeout = 30 * time.Second

func init() { siteHooks[slackLoopbackSite] = slacktest.RouteFromEnv }

// One bot message a human sees in a channel, later edits applied to its text
type slackPost struct{ channel, ts, text string }

// Loopback workspace answering the Web API the way Slack does and keeping what a human now sees
type slackWorkspace struct {
	fake   *slacktest.Server
	mu     sync.Mutex
	base   int64
	serial int
	posts  []*slackPost
}

// Workspace whose posts get their own timestamps and whose users and channels answer in Slack's shapes
func newSlackWorkspace(t *testing.T) *slackWorkspace {
	t.Helper()
	w := &slackWorkspace{fake: slacktest.New(t), base: time.Now().Unix(), serial: 100}
	w.fake.Respond("auth.test", map[string]any{"ok": true, "url": "https://standin.slack.com/", "team": "Standin", "user": slackBotName,
		"team_id": slackTeamID, "user_id": slackBotUserID, "bot_id": slackBotID, "is_enterprise_install": false})
	w.fake.RespondFunc("chat.postMessage", func(call slacktest.Call) map[string]any {
		channel, text := fmt.Sprint(call.Args["channel"]), fmt.Sprint(call.Args["text"])
		w.mu.Lock()
		ts := w.nextTSLocked()
		w.posts = append(w.posts, &slackPost{channel: channel, ts: ts, text: text})
		w.mu.Unlock()
		return map[string]any{"ok": true, "channel": channel, "ts": ts,
			"message": map[string]any{"type": "message", "user": slackBotUserID, "bot_id": slackBotID, "text": text, "ts": ts, "team": slackTeamID}}
	})
	w.fake.RespondFunc("chat.update", func(call slacktest.Call) map[string]any {
		channel, ts, text := fmt.Sprint(call.Args["channel"]), fmt.Sprint(call.Args["ts"]), fmt.Sprint(call.Args["text"])
		w.mu.Lock()
		defer w.mu.Unlock()
		for _, post := range w.posts {
			if post.channel == channel && post.ts == ts {
				post.text = text
				return map[string]any{"ok": true, "channel": channel, "ts": ts, "text": text}
			}
		}
		return map[string]any{"ok": false, "error": "message_not_found"}
	})
	w.fake.RespondFunc("users.info", func(call slacktest.Call) map[string]any {
		id := fmt.Sprint(call.Args["user"])
		isBot := id == slackBotUserID
		name := "user" + strings.ToLower(id)
		if isBot {
			name = slackBotName
		}
		return map[string]any{"ok": true, "user": map[string]any{"id": id, "team_id": slackTeamID, "name": name, "real_name": name,
			"deleted": false, "is_bot": isBot, "is_app_user": false, "tz": "UTC", "profile": map[string]any{"display_name": name, "real_name": name}}}
	})
	w.fake.RespondFunc("conversations.info", func(call slacktest.Call) map[string]any {
		id := fmt.Sprint(call.Args["channel"])
		direct := strings.HasPrefix(id, "D")
		info := map[string]any{"id": id, "is_im": direct, "is_channel": strings.HasPrefix(id, "C"), "is_group": strings.HasPrefix(id, "G"),
			"is_mpim": false, "is_private": !strings.HasPrefix(id, "C"), "is_member": true, "is_archived": false, "context_team_id": slackTeamID}
		if direct {
			info["user"] = "U" + id[1:]
		} else {
			info["name"] = "standin-" + strings.ToLower(id)
		}
		return map[string]any{"ok": true, "channel": info}
	})
	return w
}

// Next message timestamp of the workspace, the caller holding its lock
func (w *slackWorkspace) nextTSLocked() string {
	w.serial++
	return fmt.Sprintf("%d.%06d", w.base, w.serial)
}

// Texts of the bot messages a human sees in the channel, in post order
func (w *slackWorkspace) visible(channel string) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var texts []string
	for _, post := range w.posts {
		if post.channel == channel {
			texts = append(texts, post.text)
		}
	}
	return texts
}

// Delivers a direct message of the user over the socket and returns its channel once the client acknowledged it
func (w *slackWorkspace) dm(t *testing.T, conn *slacktest.Conn, user, text string) string {
	t.Helper()
	w.mu.Lock()
	ts := w.nextTSLocked()
	w.mu.Unlock()
	channel := "D" + user[1:]
	envelope, err := conn.SendEvent(slackTeamID, map[string]any{"type": "message", "channel_type": "im", "user": user, "text": text,
		"ts": ts, "event_ts": ts, "channel": channel, "team": slackTeamID, "client_msg_id": "msg-" + ts,
		"blocks": []any{map[string]any{"type": "rich_text", "block_id": "b1", "elements": []any{
			map[string]any{"type": "rich_text_section", "elements": []any{map[string]any{"type": "text", "text": text}}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Ack(envelope, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	return channel
}

var slackSpaceRE = regexp.MustCompile(`\s+`)

// Message text with markup escapes undone and whitespace collapsed, what the human reads
func slackNorm(text string) string {
	return strings.TrimSpace(slackSpaceRE.ReplaceAllString(strings.ReplaceAll(html.UnescapeString(text), `\`, ""), " "))
}

// Visible messages of the channel that carry the marker
func (w *slackWorkspace) copies(channel, marker string) []string {
	var out []string
	for _, text := range w.visible(channel) {
		if strings.Contains(slackNorm(text), marker) {
			out = append(out, text)
		}
	}
	return out
}

// Every Web API call and visible message so far, what a failed case prints
func (w *slackWorkspace) describe() string {
	var out []string
	for _, call := range w.fake.AllCalls() {
		args, _ := json.Marshal(call.Args)
		out = append(out, call.Method+" "+tail(string(args), 300))
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, post := range w.posts {
		out = append(out, fmt.Sprintf("visible %s %s %q", post.channel, post.ts, post.text))
	}
	return strings.Join(out, "\n")
}

// Gateway child serving the loopback workspace with the native model behind it
type slackRig struct {
	workspace *slackWorkspace
	peer      *nativeModel
	child     *gatewayChild
	conn      *slacktest.Conn
	home      string
}

// Workspace calls, child output and gateway log tail, the context of a failed assertion
func (r *slackRig) context() string {
	gatewayLog, _ := os.ReadFile(filepath.Join(r.home, "logs", "gateway.log"))
	return r.workspace.describe() + "\n--- gateway output\n" + tail(r.child.output(), 6000) + "\n--- gateway.log\n" + tail(string(gatewayLog), 6000)
}

// Starts gateway run on a fresh home with the real slack adapter connected to the loopback workspace
func newSlackRig(t *testing.T) *slackRig {
	t.Helper()
	peer := newNativeModel(t)
	workspace := newSlackWorkspace(t)
	modelURL := peer.server.URL + "/v1"
	tmp, h, user := gatewayCaseHomes(t, map[string]any{
		"gateway":   map[string]any{"multiplex_profiles": false},
		"model":     map[string]any{"provider": "custom", "default": "slack-fixture", "base_url": modelURL},
		"auxiliary": map[string]any{"title_generation": map[string]any{"enabled": false}},
		"updates":   map[string]any{"check": false},
		"platforms": map[string]any{"slack": map[string]any{"enabled": true}},
	})
	route, err := workspace.fake.ChildEnv(tmp)
	if err != nil {
		t.Fatal(err)
	}
	env := gatewayChildEnv(h, user, append(route, testSiteEnv+"="+slackLoopbackSite, "SLACK_BOT_TOKEN="+slackBotToken,
		"SLACK_APP_TOKEN="+slackAppToken, "SLACK_ALLOWED_USERS="+slackUserID,
		"OPENAI_API_KEY=loopback-only", "OPENAI_BASE_URL="+modelURL, runMainEnv+"=1")...)
	rig := &slackRig{workspace: workspace, peer: peer, home: h}
	rig.child = startGatewayChild(t, filepath.Join(tmp, "gateway.out"), env, "gateway", "run")
	for deadline := time.Now().Add(120 * time.Second); rig.conn == nil; {
		select {
		case err := <-rig.child.exited:
			rig.child.stopped = true
			t.Fatalf("the gateway exited before its adapter connected: %v\n%s", err, rig.context())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the slack adapter never opened its socket\n%s", rig.context())
		}
		rig.conn, _ = workspace.fake.NextConn(200 * time.Millisecond)
	}
	if len(workspace.fake.CallsTo("auth.test")) == 0 {
		t.Fatalf("the adapter opened a socket without authenticating\n%s", rig.context())
	}
	return rig
}

// Waits until a message carrying the marker is visible in the channel
func (r *slackRig) waitReply(t *testing.T, channel, marker string) {
	t.Helper()
	for deadline := time.Now().Add(slackTurnTimeout); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if len(r.workspace.copies(channel, marker)) > 0 {
			return
		}
	}
	t.Fatalf("no reply carrying %q became visible in %s\n%s", marker, channel, r.context())
}

// Waits until the gateway reports no agent run in flight in its runtime status file
func (r *slackRig) waitIdle(t *testing.T) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		raw, err := os.ReadFile(filepath.Join(r.home, "gateway_state.json"))
		var status struct {
			ActiveAgents *int `json:"active_agents"`
		}
		if err == nil && json.Unmarshal(raw, &status) == nil && status.ActiveAgents != nil && *status.ActiveAgents == 0 {
			return
		}
	}
	t.Fatalf("the gateway never went idle\n%s", r.context())
}

// One more answered message in the same chat, proof that everything queued before it was handled
func (r *slackRig) barrier(t *testing.T, token string) {
	t.Helper()
	r.waitIdle(t)
	channel := r.workspace.dm(t, r.conn, slackUserID, "barrier [in:"+token+"]")
	r.waitReply(t, channel, "NATIVE_ACK_barrier [in:"+token+"]")
}

// Model turns the inbound carrying the token started
func (r *slackRig) turns(token string) int {
	n := 0
	for _, text := range r.peer.texts() {
		if strings.Contains(text, "[in:"+token+"]") {
			n++
		}
	}
	return n
}

// Persisted user rows whose content carries the needle
func (r *slackRig) userRows(t *testing.T, needle string) int {
	t.Helper()
	hn := &nativeHarness{home: r.home}
	return len(hn.query(t, "SELECT content FROM messages WHERE role='user' AND content LIKE ?", "%"+needle+"%"))
}

func TestSlackDMGetsExactlyOneReplyThroughTheGatewayChild(t *testing.T) {
	rig := newSlackRig(t)
	token, body := "dm-one", "hello back (v1.2) - ok! #1 = {x}"
	channel := rig.workspace.dm(t, rig.conn, slackUserID, body+" [in:"+token+"]")
	answer := "NATIVE_ACK_" + body + " [in:" + token + "]"
	rig.waitReply(t, channel, answer)
	rig.barrier(t, "dmb-one")
	copies := rig.workspace.copies(channel, "[in:"+token+"]")
	if len(copies) != 1 || !strings.Contains(slackNorm(copies[0]), answer) {
		t.Fatalf("expected one complete reply, got %q\n%s", copies, rig.context())
	}
	if n := rig.turns(token); n != 1 {
		t.Fatalf("the inbound started %d model turns\n%s", n, rig.context())
	}
	if n := rig.userRows(t, "[in:"+token+"]"); n != 1 {
		t.Fatalf("the inbound was persisted %d times", n)
	}
	if exited, err := rig.child.interrupt(60 * time.Second); !exited || err != nil {
		t.Fatalf("gateway exit = %v, %v\n%s", exited, err, rig.context())
	}
}
