package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Poll interval of a side task's answer wait
const gatewaySidePoll = time.Second

// Gateway connection of the CLI while its session is open, nil off the gateway or once the connection is final
func gatewayConnectionFor(c *CLI) *gatewaycli.GatewayConnection {
	store := c.gatewayStore()
	if store == nil || !c.gatewaySessionOpen || store.Connection.Final() {
		return nil
	}
	return store.Connection
}

// Admits a side task on the owner, returning its task id and the wait for its answer
func gatewaySideStart(ctx context.Context, connection *gatewaycli.GatewayConnection, sessionID, verb, text string) (string, func() (string, error), error) {
	raw, epoch, err := connection.CallTagged(ctx, "prompt."+verb, map[string]any{"session_id": sessionID, "text": text}, gatewaycli.CallOptions{})
	if err != nil {
		return "", nil, err
	}
	result, _ := pyjson.Plain(raw).(map[string]any)
	if !pyrepr.Truthy(result["task_id"]) {
		return "", nil, fmt.Errorf("prompt.%s returned no task id", verb)
	}
	taskID := pyrepr.Str(result["task_id"])
	inbox := connection.SideWaiter(taskID)
	wait := func() (string, error) {
		defer connection.DropSideWaiter(taskID)
		for {
			waitCtx, cancel := context.WithTimeout(ctx, gatewaySidePoll)
			payload, err := inbox.Get(waitCtx)
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return "", ctx.Err()
				}
				if connection.Closed() || connection.Epoch() != epoch {
					return "", errors.New(gateway.GatewayDisconnected)
				}
				continue
			}
			answer := ""
			if dict, _ := pyjson.Plain(payload).(map[string]any); pyrepr.Truthy(dict["text"]) {
				answer = pyrepr.Str(dict["text"])
			}
			if rest, failed := strings.CutPrefix(answer, "error: "); failed {
				return "", errors.New(rest)
			}
			return answer, nil
		}
	}
	return taskID, wait, nil
}
