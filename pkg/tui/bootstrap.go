package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
)

// Launch variables that must not leak into a gateway owner the bootstrap starts
var tuiBootstrapScrubbedEnv = []string{"ICEBATH_MODEL", "ICEBATH_INFERENCE_MODEL", "ICEBATH_TUI_PROVIDER",
	"ICEBATH_INFERENCE_PROVIDER", "ICEBATH_TUI_TOOLSETS", "ICEBATH_TUI_SKILLS",
	"ICEBATH_CWD", "TERMINAL_CWD", "ICEBATH_YOLO", "ICEBATH_ACCEPT_HOOKS",
	"ICEBATH_TUI_CHECKPOINTS", "ICEBATH_TUI_PASS_SESSION_ID", "ICEBATH_KANBAN_BOARD",
	"ICEBATH_TUI_CLIENT_ENV"}

// Verdict line the TUI shows when no owner can be attached, each detail line kept a verdict
func tuiBootstrapFailureMessage(receipt gateway.ObservedEndpoint) string {
	reason := receipt.ReasonCode
	if reason == "" {
		reason = "not ready"
	}
	head := fmt.Sprintf("gateway %s: %s", receipt.State, reason)
	if receipt.Detail == "" {
		return head
	}
	if receipt.ReasonCode == "runtime_exited" {
		return head + "\n" + receipt.Detail
	}
	out := []string{head}
	for _, line := range strings.Split(strings.ReplaceAll(receipt.Detail, "\r\n", "\n"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, fmt.Sprintf("gateway %s: %s", receipt.State, line))
		}
	}
	return strings.Join(out, "\n")
}

// Answer of the control socket to a session ticket request
type ticketAnswer struct {
	OK     bool `json:"ok"`
	ID     int  `json:"id"`
	Result struct {
		InstanceID string  `json:"instance_id"`
		ProfileID  string  `json:"profile_id"`
		Ticket     *string `json:"ticket"`
	} `json:"result"`
}

// Ensures or discovers the gateway owner and mints the private interactive grant the terminal UI attaches with
func GatewayBootstrap(ctx context.Context, start, recover bool) (localGrant, error) {
	abs, err := filepath.Abs(home.Dir(ctx))
	if err != nil {
		return localGrant{}, err
	}
	h, err := canonicalHome(abs)
	if err != nil {
		return localGrant{}, err
	}
	for _, key := range tuiBootstrapScrubbedEnv {
		_ = os.Unsetenv(key)
	}
	var receipt gateway.ObservedEndpoint
	if start {
		if receipt, err = gateway.EnsureGatewayRuntime(ctx, h, 30, true); err != nil {
			return localGrant{}, err
		}
	} else {
		receipt = gateway.DiscoverGatewayEndpoint(ctx, h, 5*time.Second)
	}
	if !start && recover && !gateway.Attachable(receipt) {
		if receipt, err = gateway.EnsureGatewayRuntime(ctx, h, 30, true); err != nil {
			return localGrant{}, err
		}
	}
	if !gateway.Attachable(receipt) {
		return localGrant{}, errors.New(tuiBootstrapFailureMessage(receipt))
	}
	endpoint := receipt.Endpoint
	request, err := json.Marshal(map[string]any{"protocol": 1, "id": 1, "verb": "session-ticket",
		"params": map[string]any{"profile_id": endpoint.ProfileID, "instance_id": endpoint.InstanceID, "purpose": "interactive"}})
	if err != nil {
		return localGrant{}, err
	}
	raw, err := gateway.TUIBootstrapControlQuery(gateway.ControlHomeFor(h, endpoint), append(request, '\n'), 5*time.Second)
	if err != nil {
		return localGrant{}, err
	}
	var answer ticketAnswer
	if err := json.Unmarshal(raw, &answer); err != nil {
		return localGrant{}, err
	}
	granted := answer.OK && answer.ID == 1 && answer.Result.Ticket != nil &&
		answer.Result.InstanceID == endpoint.InstanceID && answer.Result.ProfileID == endpoint.ProfileID
	if !granted {
		return localGrant{}, errors.New("gateway private bootstrap rejected")
	}
	url, protocols := gateway.GatewayWSTarget(endpoint, *answer.Result.Ticket)
	return localGrant{URL: url, Protocols: protocols, ProfileID: endpoint.ProfileID, InstanceID: endpoint.InstanceID}, nil
}
