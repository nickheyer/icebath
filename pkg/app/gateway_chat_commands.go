package app

import (
	"fmt"
	"math/big"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
)

// Command listing every command the shared gateway serves
const gatewayCommandList = "/help"

// Refusal naming the command list for a command the shared gateway does not serve yet
func gatewayRefusal(name string) string {
	return fmt.Sprintf("/%s is not available on the shared gateway yet. Commands it serves: %s", name, gatewayCommandList)
}

// Whether a decoded JSON value is a Python int and not a bool
func gatewayIsInt(v any) bool {
	switch v.(type) {
	case int64, int, *big.Int:
		return true
	}
	return false
}

// Human authored live user rows of a snapshot oldest first, never archived and never a handoff carrier
func gatewayUserTurns(snapshot map[string]any) []map[string]any {
	var turns []map[string]any
	messages, _ := gatewayGetOr(snapshot, "messages", []any{}).([]any)
	for _, raw := range messages {
		row, _ := raw.(map[string]any)
		kind := row["display_kind"]
		if gatewayIsInt(row["row_id"]) && (kind == nil || kind == "" || kind == "steer") &&
			!anyutil.IsTrue(row["archived"]) && agent.IsUserOriginatedTurn(row) {
			turns = append(turns, row)
		}
	}
	return turns
}

// Rewind payload naming the Nth newest user turn of the snapshot, a retry refusing a turn it cannot resend
func gatewayRewindTo(turnsBack int64, retry bool) mutationDerive {
	return func(snapshot map[string]any) (map[string]any, error) {
		turns := gatewayUserTurns(snapshot)
		if int64(len(turns)) < turnsBack {
			return nil, nil
		}
		target := turns[int64(len(turns))-turnsBack]
		if retry {
			view := agent.UserOriginatedTurnView(target)
			if _, err := agent.RetryableUserText(view["content"]); err != nil {
				return nil, &gateway.GatewayClientError{Msg: "/retry: " + err.Error()}
			}
		}
		return map[string]any{"target_message_id": requireKey(target, "row_id")}, nil
	}
}
