package googlemeet

import (
	"errors"
	"fmt"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Gateway side RPC client, one short lived websocket per call
type nodeClient struct {
	url, token string
	timeout    time.Duration
}

// Client for a node url and token with the call timeout
func newNodeClient(url, token string, timeout time.Duration) (*nodeClient, error) {
	if url == "" {
		return nil, &ValueError{"url must be a non-empty string"}
	}
	if token == "" {
		return nil, &ValueError{"token must be a non-empty string"}
	}
	return &nodeClient{url: url, token: token, timeout: timeout}, nil
}

// Sends one request and returns its payload, failing on an error envelope or an id mismatch
func (c *nodeClient) rpc(typ string, payload *pyjson.Object) (*pyjson.Object, error) {
	req, err := makeRequest(typ, c.token, payload, "")
	if err != nil {
		return nil, err
	}
	text, err := encodeEnvelope(req)
	if err != nil {
		return nil, err
	}
	ws, err := dialSyncWS(c.url, nil, c.timeout)
	if err != nil {
		return nil, err
	}
	defer ws.Close()
	if err := ws.Send(text); err != nil {
		return nil, err
	}
	raw, err := ws.Recv(c.timeout)
	if err != nil {
		return nil, err
	}
	resp, err := decodeEnvelope([]byte(raw))
	if err != nil {
		return nil, err
	}
	if t, _ := resp.Get("type"); t == "error" {
		msg, ok := resp.Get("error")
		if !ok {
			msg = "<unknown>"
		}
		return nil, fmt.Errorf("node error: %s", pyrepr.Str(msg))
	}
	reqID, _ := req.Get("id")
	if id, _ := resp.Get("id"); id != reqID {
		return nil, fmt.Errorf("response id mismatch: sent %s, got %s", reqID, pyrepr.Repr(id))
	}
	out, ok := resp.Vals["payload"].(*pyjson.Object)
	if !ok {
		return nil, errors.New("response missing payload dict")
	}
	return out, nil
}

// Starts the bot on the node
func (c *nodeClient) startBot(url, guestName string, duration *string, headed bool, mode string) (*pyjson.Object, error) {
	payload := pyjson.ObjectOf("url", url, "guest_name", guestName, "headed", headed, "mode", mode)
	if duration != nil {
		payload.Set("duration", *duration)
	}
	return c.rpc("start_bot", payload)
}

func (c *nodeClient) stop() (*pyjson.Object, error) { return c.rpc("stop", pyjson.NewObject()) }

func (c *nodeClient) status() (*pyjson.Object, error) { return c.rpc("status", pyjson.NewObject()) }

// Transcript of the node's meeting, the last n lines when last is set
func (c *nodeClient) transcript(last *int) (*pyjson.Object, error) {
	payload := pyjson.NewObject()
	if last != nil {
		payload.Set("last", *last)
	}
	return c.rpc("transcript", payload)
}

func (c *nodeClient) say(text string) (*pyjson.Object, error) {
	return c.rpc("say", pyjson.ObjectOf("text", text))
}

func (c *nodeClient) ping() (*pyjson.Object, error) { return c.rpc("ping", pyjson.NewObject()) }
