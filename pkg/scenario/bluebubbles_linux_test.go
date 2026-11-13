package scenario

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/fakellm"
)

const blueBubblesPassword = "scenario bubbles/pass"

// One by one pixel PNG the fake BlueBubbles server serves as an inbound attachment
var blueBubblesPNG, _ = base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")

// Fake BlueBubbles REST server that keeps the webhooks registered with it and answers every call the adapter makes
func blueBubblesFakeServer() http.Handler {
	var mu sync.Mutex
	hooks := []any{}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		answer := func(code int, v any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			json.NewEncoder(w).Encode(v)
		}
		mu.Lock()
		defer mu.Unlock()
		switch path := r.URL.Path; {
		case r.URL.Query().Get("password") != blueBubblesPassword:
			answer(401, map[string]any{"status": 401, "message": "Unauthorized"})
		case path == "/api/v1/ping":
			answer(200, map[string]any{"status": 200, "message": "Ping received!", "data": "pong"})
		case path == "/api/v1/server/info":
			answer(200, map[string]any{"status": 200, "data": map[string]any{"private_api": false, "helper_connected": false}})
		case path == "/api/v1/webhook" && r.Method == http.MethodGet:
			answer(200, map[string]any{"status": 200, "data": hooks})
		case path == "/api/v1/webhook" && r.Method == http.MethodPost:
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			body["id"] = 7
			hooks = append(hooks, body)
			answer(200, map[string]any{"status": 200, "data": body})
		case path == "/api/v1/webhook/7" && r.Method == http.MethodDelete:
			hooks = []any{}
			answer(200, map[string]any{"status": 200})
		case path == "/api/v1/attachment/att-wave-1/download":
			w.Header().Set("Content-Type", "image/png")
			w.Write(blueBubblesPNG)
		case path == "/api/v1/message/text":
			answer(200, map[string]any{"status": 200, "data": map[string]any{"guid": "sent-by-the-fake"}})
		default:
			answer(404, map[string]any{"status": 404, "message": "no such route " + path})
		}
	})
}

// Message texts the fake server received on the text send route for one chat, in arrival order
func blueBubblesTextsSent(s *S, server *Peer, chat string) []string {
	s.t.Helper()
	var texts []string
	for _, req := range server.Requests() {
		if req.Method != http.MethodPost || !strings.HasPrefix(req.Path, "/api/v1/message/text?") {
			continue
		}
		var body map[string]any
		if err := json.Unmarshal([]byte(req.Body), &body); err != nil {
			s.Failf("a text send carried no JSON body: %q", req.Body)
		}
		if body["chatGuid"] == chat {
			text, _ := body["message"].(string)
			texts = append(texts, text)
		}
	}
	return texts
}

// The BlueBubbles adapter registers its webhook, caches an inbound image by mime, runs the turn and sends text and a GIF link back
func TestScenarioBlueBubblesInboundImageAndAnimationReply(t *testing.T) {
	s := Start(t, Setup{})
	server := s.Peer("bluebubbles", blueBubblesFakeServer())
	s.Port("bbhook")
	s.Write(".env", s.Read(".env")+"BLUEBUBBLES_SERVER_URL=${PEER_bluebubbles}/\nBLUEBUBBLES_PASSWORD="+blueBubblesPassword+"\n"+
		"BLUEBUBBLES_WEBHOOK_PORT=${PORT_bbhook}\nBLUEBUBBLES_ALLOWED_USERS=+15551230000\nBLUEBUBBLES_HOME_CHANNEL=iMessage;-;+15550009999\n")
	s.Set("BBHOOK", "http://127.0.0.1:${PORT_bbhook}")
	animation := server.URL + "/media/wave.gif"
	s.Model.Push(fakellm.Text{Text: "Here is the wave.\n\n![wave](" + animation + ")"})

	gw := s.Spawn("gateway", "run")
	s.WaitHTTP("${BBHOOK}/health")
	gatewayRunning(s)
	const quotedPassword = "scenario%20bubbles%2Fpass"
	registered := false
	for _, req := range server.Requests() {
		if req.Method == http.MethodPost && req.Path == "/api/v1/webhook?password="+quotedPassword {
			registered = true
			s.SameJSON(req.Body, `{"url": "http://localhost:${PORT_bbhook}/bluebubbles-webhook?password=`+quotedPassword+`",
				"events": ["new-message", "updated-message"]}`)
		}
	}
	if !registered {
		s.Failf("the adapter never registered its webhook with the server, it sent %d requests", len(server.Requests()))
	}

	hook := "${BBHOOK}/bluebubbles-webhook"
	s.SameJSON(s.HTTP("POST", hook+"?password=wrong", `{"type": "new-message"}`).Code(401).Body, `{"error": "unauthorized"}`)
	s.SameJSON(s.HTTP("POST", hook, `{"type": "new-message"}`, "X-Password", "also wrong").Code(401).Body, `{"error": "unauthorized"}`)
	authed := hook + "?password=" + quotedPassword
	s.SameJSON(s.HTTP("POST", authed, `{"type": "new-message", "data": {"guid": "m-empty"}}`).Code(400).Body, `{"error": "missing message fields"}`)
	s.SameJSON(s.HTTP("POST", authed, `not json at all`).Code(400).Body, `{"error": "missing message fields"}`)
	chat := "iMessage;-;+15551230000"
	message := func(guid, sender, text string, extra map[string]any) map[string]any {
		data := map[string]any{"guid": guid, "text": text, "isFromMe": false, "handle": map[string]any{"address": sender},
			"chats": []any{map[string]any{"guid": "iMessage;-;" + sender, "chatIdentifier": sender}}}
		for k, v := range extra {
			data[k] = v
		}
		return map[string]any{"type": "new-message", "data": data}
	}
	for _, ignored := range []map[string]any{
		{"type": "typing-indicator", "data": map[string]any{"display": true, "guid": chat}},
		message("m-own", "+15551230000", "my own echo", map[string]any{"isFromMe": true}),
		message("m-tapback", "+15551230000", "Loved an image", map[string]any{"associatedMessageType": 2000}),
		message("m-stranger", "+19998887777", "Let me in", nil),
	} {
		if got := s.HTTP("POST", authed, ignored).Code(200).Body; got != "ok" {
			s.Failf("an ignored event answered %q", got)
		}
	}

	inbound := message("m-wave-1", "+15551230000", "What does this show?", map[string]any{"attachments": []any{
		map[string]any{"guid": "att-wave-1", "mimeType": "Image/PNG; charset=binary", "transferName": "IMG_0001.PNG"}}})
	if got := s.HTTP("POST", hook, inbound, "X-Password", blueBubblesPassword).Code(200).Body; got != "ok" {
		s.Failf("the inbound message answered %q", got)
	}
	if err := s.Model.WaitForRequests(1, 2*time.Minute, "main"); err != nil {
		s.Failf("the inbound message never reached the model: %v", err)
	}
	asked := false
	for _, m := range s.LastMessages() {
		raw, _ := json.Marshal(Dig(m, "content"))
		asked = asked || (Dig(m, "role") == "user" && strings.Contains(string(raw), "What does this show?"))
		if strings.Contains(string(raw), "Let me in") || strings.Contains(string(raw), "my own echo") || strings.Contains(string(raw), "Loved an image") {
			s.Failf("an ignored event reached the model: %s", raw)
		}
	}
	if !asked {
		s.Failf("the model did not receive the sender's text")
	}
	var cached []string
	for _, dir := range []string{"cache/images", "image_cache"} {
		found, _ := filepath.Glob(filepath.Join(s.Path(dir), "*"))
		cached = append(cached, found...)
	}
	if len(cached) != 1 || filepath.Ext(cached[0]) != ".png" {
		s.Failf("the image cache holds %v, want one .png file named by the attachment mime", cached)
	} else if stored, err := os.ReadFile(cached[0]); err != nil || !bytes.Equal(stored, blueBubblesPNG) {
		s.Failf("the cached attachment differs from the bytes the server served (%v)", err)
	}

	want := []string{"Here is the wave.", "wave\n" + animation}
	s.Eventually("the reply text and the animation link at the server", 2*time.Minute, func() bool {
		return len(blueBubblesTextsSent(s, server, chat)) >= len(want)
	})
	if got := blueBubblesTextsSent(s, server, chat); len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		s.Failf("the server received the texts %q, want %q", got, want)
	}
	if got := blueBubblesTextsSent(s, server, "iMessage;-;+19998887777"); len(got) != 0 {
		s.Failf("a sender outside the allowlist was answered with %q", got)
	}
	downloads := 0
	for _, req := range server.Requests() {
		if strings.HasPrefix(req.Path, "/media/wave.gif") || strings.HasPrefix(req.Path, "/api/v1/message/attachment") {
			s.Failf("a loopback image address must go out as text, the server saw %s %s", req.Method, req.Path)
		}
		if req.Method == http.MethodGet && strings.HasPrefix(req.Path, "/api/v1/attachment/att-wave-1/download?password=") {
			downloads++
		}
	}
	if downloads != 1 {
		s.Failf("the inbound attachment was downloaded %d times, want once", downloads)
	}
	if n := len(s.Model.MainRequests()); n != 1 {
		s.Failf("the model received %d turns, want the one of the allowed sender", n)
	}

	gw.Stop()
	removed := false
	for _, req := range server.Requests() {
		removed = removed || (req.Method == http.MethodDelete && strings.HasPrefix(req.Path, "/api/v1/webhook/7?password="))
	}
	if !removed {
		s.Failf("the adapter left its webhook registered with the server at shutdown")
	}
}
