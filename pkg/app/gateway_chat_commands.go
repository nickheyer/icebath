package app

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Page listing where every classic command stands on the shared gateway
const gatewayParityDoc = "https://hermes-agent.nousresearch.com/docs/developer-guide/gateway-command-parity"

// Controls the view answers itself, an alias of one answered as its canonical name
var gatewayViewControls = map[string]bool{"quit": true, "stop": true, "approve": true, "branch": true, "model": true,
	"compress": true, "yolo": true, "help": true}

// Session commands the classic view maps onto owner verbs itself
var gatewayCommandHandlers = map[string]func(ctx context.Context, v *GatewayChatView, rest string) error{
	"title": gatewayTitle, "undo": gatewayUndo, "retry": gatewayRetry, "new": gatewayNew, "clear": gatewayNew,
	"usage": gatewayUsage, "tools": gatewayTools,
}

// Refusal naming the parity table for a command the shared gateway does not serve yet
func gatewayRefusal(name string) string {
	return fmt.Sprintf("/%s is not available on the shared gateway yet. Parity table: %s", name, gatewayParityDoc)
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
			!pyrepr.Truthy(row["archived"]) && agent.IsUserOriginatedTurn(row) {
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

// Applies and acknowledges one mutation of the view session
func gatewayMutate(ctx context.Context, v *GatewayChatView, operation string, payload any) (map[string]any, error) {
	result, err := v.mutations.Apply(ctx, v.rpc, v.sessionID, operation, payload, nil)
	if err != nil {
		return nil, err
	}
	v.mutations.Acknowledge(v.sessionID, operation, payload)
	return result, nil
}

// Rewound user turn text and the rewound message count, ok false when there was nothing to rewind
func gatewayRewind(ctx context.Context, v *GatewayChatView, turnsBack int64, retry bool) (string, any, bool, error) {
	payload := gatewayRewindTo(turnsBack, retry)
	result, err := v.mutations.Apply(ctx, v.rpc, v.sessionID, "rewind", payload, nil)
	if err != nil {
		return "", nil, false, err
	}
	if pyrepr.Equal(result["status"], "nothing") || !pyrepr.Truthy(result["rewound_count"]) {
		v.mutations.Acknowledge(v.sessionID, "rewind", payload)
		return "", nil, false, nil
	}
	snapshot, err := v.rpc(ctx, "session.resume", map[string]any{"session_id": v.sessionID}, 0)
	if err != nil {
		return "", nil, false, err
	}
	v.mutations.Acknowledge(v.sessionID, "rewind", payload)
	v.generation = requireKey(snapshot, "execution_generation")
	target, _ := result["target_message"].(map[string]any)
	content := target["content"]
	count := requireKey(result, "rewound_count")
	if retry {
		if text, err := agent.RetryableUserText(content); err == nil {
			return text, count, true, nil
		}
	}
	text, _ := content.(string)
	return text, count, true, nil
}

// Renames the session, a bare title asking the owner for its report
func gatewayTitle(ctx context.Context, v *GatewayChatView, rest string) error {
	if rest == "" {
		return gatewayForward(ctx, v, "title", "")
	}
	result, err := gatewayMutate(ctx, v, "rename", pyjson.ObjectOf("title", rest))
	if err != nil {
		return err
	}
	title := rest
	if pyrepr.Truthy(result["title"]) {
		title = pyrepr.Str(result["title"])
	}
	v.println(v.out, "Session title set: "+title)
	return nil
}

// Rewinds the last N user turns and puts the removed message in the composer
func gatewayUndo(ctx context.Context, v *GatewayChatView, rest string) error {
	turns := int64(1)
	if rest != "" {
		n, ok := new(big.Int), false
		if pystr.StrIsDigit(rest) {
			n, ok = pystr.IntText(rest)
			if !ok {
				return fmt.Errorf("ValueError: invalid literal for int() with base 10: %s", pyrepr.Repr(rest))
			}
		}
		if !ok || n.Sign() <= 0 {
			return &gateway.GatewayClientError{Msg: "Usage: /undo [N]"}
		}
		turns = math.MaxInt64
		if n.IsInt64() {
			turns = n.Int64()
		}
	}
	text, count, ok, err := gatewayRewind(ctx, v, turns, false)
	if err != nil {
		return err
	}
	if !ok {
		v.println(v.out, v.t(ctx, "cli.session.undo_no_user_message"))
		return nil
	}
	plural := "s"
	if turns == 1 {
		plural = ""
	}
	v.println(v.out, fmt.Sprintf("Undid %d turn%s (%s message(s)); the removed message is in the composer.", turns, plural, pyrepr.Str(count)))
	v.prefill = text
	return nil
}

// Rewinds the last user turn and submits its lossless resend text again
func gatewayRetry(ctx context.Context, v *GatewayChatView, rest string) error {
	text, _, ok, err := gatewayRewind(ctx, v, 1, true)
	if err != nil {
		return err
	}
	if !ok || pystr.Strip(text) == "" {
		v.println(v.out, v.t(ctx, "cli.session.retry_no_user_message"))
		return nil
	}
	v.println(v.out, fmt.Sprintf("\n%s\n● %s", strings.Repeat("─", 40), text))
	_, err = v.submit(ctx, text)
	return err
}

// Starts a fresh session with this one frozen launch request and attaches to it, a title renaming it after
func gatewayNew(ctx context.Context, v *GatewayChatView, rest string) error {
	info, err := v.rpc(ctx, "session.info", map[string]any{"session_id": v.sessionID}, 0)
	if err != nil {
		return err
	}
	described, err := v.rpc(ctx, "runtime.describe", map[string]any{}, 0)
	if err != nil {
		return err
	}
	contract, _ := described["session_create"].(map[string]any)
	parameters, _ := contract["parameters"].([]any)
	accepted := map[string]bool{}
	for _, name := range parameters {
		if s, ok := name.(string); ok {
			accepted[s] = true
		}
	}
	request := map[string]any{}
	launch, _ := info["launch_request"].(map[string]any)
	for key, value := range launch {
		if accepted[key] && key != "request_id" && key != "title" {
			request[key] = value
		}
	}
	if pyrepr.Truthy(info["cwd"]) && accepted["cwd"] {
		request["cwd"] = info["cwd"]
	}
	if v.launchAPIKey != "" && accepted["api_key"] {
		request["api_key"] = v.launchAPIKey
	}
	var acceptedNames []any
	for name := range accepted {
		acceptedNames = append(acceptedNames, name)
	}
	policy, err := gatewaycli.ClientEnvPolicy(ctx, acceptedNames)
	if err != nil {
		return err
	}
	for key, value := range policy {
		request[key] = value
	}
	request["request_id"] = uuidHex()
	snapshot, err := v.rpc(ctx, "session.create", request, 0)
	if err != nil {
		return err
	}
	if err := v.adopt(ctx, snapshot); err != nil {
		return err
	}
	v.println(v.out, v.t(ctx, "cli.session.new_session"))
	v.println(v.errOut, "Session: "+v.sessionID)
	if rest != "" {
		return gatewayTitle(ctx, v, rest)
	}
	return nil
}

// Python int() of a JSON number or numeric string, truncating floats
func gatewayPyInt(v any) (int64, error) {
	switch n := v.(type) {
	case nil:
		return 0, nil
	case bool:
		if n {
			return 1, nil
		}
		return 0, nil
	case int64:
		return n, nil
	case int:
		return int64(n), nil
	case float64:
		return int64(n), nil
	case *big.Int:
		if n.IsInt64() {
			return n.Int64(), nil
		}
	case string:
		if parsed, ok := pystr.IntText(pystr.Strip(n)); ok && parsed.IsInt64() {
			return parsed.Int64(), nil
		}
	}
	return 0, fmt.Errorf("ValueError: invalid literal for int() with base 10: %s", pyrepr.Repr(v))
}

// The owner session report plus the last committed turn of this view
func gatewayUsage(ctx context.Context, v *GatewayChatView, rest string) error {
	if rest != "" {
		return gatewayForward(ctx, v, "usage", rest)
	}
	if err := gatewayForward(ctx, v, "usage", ""); err != nil {
		return err
	}
	keys := v.completions.Keys()
	if len(keys) == 0 {
		return nil
	}
	receipt, err := v.rpc(ctx, "prompt.receipt", map[string]any{"session_id": v.sessionID, "admission_id": keys[len(keys)-1], "include_result": true}, 0)
	if err != nil {
		return err
	}
	result, isDict := gatewayText(receipt["result"], map[string]any{}).(map[string]any)
	if !isDict || len(result) == 0 {
		return nil
	}
	model := v.model
	if pyrepr.Truthy(result["model"]) {
		model = pyrepr.Str(result["model"])
	}
	lines := []string{"", "Last turn:", "  Model: " + model}
	for _, pair := range [][2]string{{"Input tokens", "input_tokens"}, {"Output tokens", "output_tokens"},
		{"Total tokens", "total_tokens"}, {"API calls", "api_calls"}} {
		n, err := gatewayPyInt(gatewayText(result[pair[1]], int64(0)))
		if err != nil {
			return err
		}
		lines = append(lines, fmt.Sprintf("  %s: %s", pair[0], pystr.GroupThousands(n)))
	}
	switch cost := result["estimated_cost_usd"].(type) {
	case int64, int, float64, bool, *big.Int:
		f, _ := pyNumAdd(cost, 0.0).(float64)
		lines = append(lines, fmt.Sprintf("  Cost: $%.4f", f))
	}
	v.println(v.out, strings.Join(lines, "\n"))
	return nil
}

// The toolsets frozen at launch and where changing them mid session stands
func gatewayTools(ctx context.Context, v *GatewayChatView, rest string) error {
	info, err := v.rpc(ctx, "session.info", map[string]any{"session_id": v.sessionID}, 0)
	if err != nil {
		return err
	}
	launch, _ := info["launch_request"].(map[string]any)
	if toolsets, ok := launch["toolsets"].([]any); ok && len(toolsets) > 0 {
		names := make([]string, len(toolsets))
		for i, name := range toolsets {
			names[i] = pyrepr.Str(name)
		}
		v.println(v.out, "Toolsets frozen at launch: "+strings.Join(names, ", "))
	} else {
		v.println(v.out, "This session runs the profile's CLI toolsets as they were at launch (`icebath tools list`).")
	}
	_, tail, _ := strings.Cut(gatewayRefusal("tools"), " ")
	v.println(v.out, "Changing toolsets mid-session (/tools enable|disable) "+tail)
	return nil
}

// Runs a registry command or skill on the owner, its refusal naming where the command stands
func gatewayForward(ctx context.Context, v *GatewayChatView, name, rest string) error {
	command := name
	if rest != "" {
		command += " " + rest
	}
	reply, err := v.rpc(ctx, "slash.exec", map[string]any{"session_id": v.sessionID, "command": command}, 0)
	if err != nil {
		var refused *gateway.GatewayRPCError
		if !errors.As(err, &refused) || refused.Msg != "unsupported_command" {
			return err
		}
		if commands.Resolve(name) != nil {
			v.println(v.out, gatewayRefusal(name))
		} else {
			v.println(v.out, fmt.Sprintf("Unknown command: /%s (use /help)", name))
		}
		return nil
	}
	if kind := reply["type"]; pyrepr.Equal(kind, "skill") || pyrepr.Equal(kind, "send") {
		if pyrepr.Truthy(reply["notice"]) {
			v.println(v.out, pyrepr.Str(reply["notice"]))
		}
		display := "/" + name
		if pyrepr.Truthy(reply["display"]) {
			display = pyrepr.Str(reply["display"])
		}
		v.println(v.out, fmt.Sprintf("\n%s\n● %s", strings.Repeat("─", 40), display))
		_, err := v.submit(ctx, pyrepr.Str(requireKey(reply, "message")))
		return err
	}
	output := "/" + name + ": no output"
	if pyrepr.Truthy(reply["output"]) {
		output = pyrepr.Str(reply["output"])
	}
	v.println(v.out, output)
	return nil
}

// Any slash command the view controls did not take, an alias of a view control answered as its canonical
func gatewayRunCommand(ctx context.Context, v *GatewayChatView, command, rest string) (bool, error) {
	name := strings.TrimLeft(command, "/")
	canonical := name
	if def := commands.Resolve(name); def != nil {
		canonical = def.Name
	}
	if gatewayViewControls[canonical] && canonical != name {
		return v.command(ctx, pystr.Rstrip("/"+canonical+" "+rest))
	}
	if handler, ok := gatewayCommandHandlers[canonical]; ok {
		return true, handler(ctx, v, pystr.Strip(rest))
	}
	return true, gatewayForward(ctx, v, canonical, pystr.Strip(rest))
}

// Help line of the classic gateway view, its controls and the reports the owner runs
func gatewayChatHelpText() string {
	var names []string
	for name := range gateway.SessionReadCommands {
		if name != "help" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	reports := make([]string, len(names))
	for i, name := range names {
		reports[i] = "/" + name
	}
	return "/stop, /approve <id> <choice>, /answer <id> <text>, /discard <admission_id> (turn lost during " +
		"restart), /yolo [on|off], /quit (detach). /branch [title], /model <model> [--provider name], " +
		"/compress [here [N] | <focus>] [--preview]. /title [name], /undo [N], /retry, /new [title] " +
		"(/reset), /usage, /tools.\nReports run by the gateway: " + strings.Join(reports, " ") + ".\nParity table: " + gatewayParityDoc
}
