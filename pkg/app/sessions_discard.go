package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/gorilla/websocket"
	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/gateway"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
)

// Resumes a session by exact id first and then by title, the way chat --resume resolves a name
func discardResume(ctx context.Context, client *gateway.GatewayClient, name string) (map[string]any, error) {
	for _, key := range []string{"session_id", "title"} {
		snapshot, err := client.RPC(ctx, "session.resume", map[string]any{key: name})
		if err == nil {
			return snapshot, nil
		}
		var clientErr *gateway.GatewayClientError
		if !errors.As(err, &clientErr) || clientErr.Msg != "not_found" {
			return nil, err
		}
	}
	return nil, &gateway.GatewayClientError{Msg: "No session found matching '" + name + "'. Use 'icebath sessions list' to see available sessions."}
}

// Lists the lost turns and asks whether to discard them, end of input reading as no
func discardConfirm(w io.Writer, sessionID string, rows []map[string]any) (bool, error) {
	fmt.Fprintf(w, "Discard %d lost turn(s) on session %s? They are acknowledged, not replayed.\n", len(rows), sessionID)
	for _, row := range rows {
		fmt.Fprintf(w, "  %s\n", anyutil.String(row["admission_id"]))
	}
	line, err := console().Input("Proceed? [y/N] ")
	if errors.Is(err, cliui.ErrEOF) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

// Resolves the chosen unknown admissions of a session through its gateway with the generation each carries
func discard(ctx context.Context, ns map[string]any, w io.Writer) (int, error) {
	client, err := gateway.ConnectGateway(ctx)
	if err != nil {
		return 1, err
	}
	defer client.Close()
	snapshot, err := discardResume(ctx, client, nsText(ns, "session"))
	if err != nil {
		return 1, err
	}
	sessionID := snapshot["session_id"]
	if !anyutil.IsTrue(sessionID) {
		sessionID = snapshot["stored_session_id"]
	}
	var unknown []map[string]any
	pending, _ := snapshot["pending"].([]any)
	for _, item := range pending {
		if row, ok := item.(map[string]any); ok && row["status"] == "unknown" {
			unknown = append(unknown, row)
		}
	}
	chosen := unknown
	if wanted, _ := ns["admission"].([]string); len(wanted) > 0 {
		chosen = nil
		found := map[string]bool{}
		for _, row := range unknown {
			if id := anyutil.String(row["admission_id"]); slices.Contains(wanted, id) {
				chosen = append(chosen, row)
				found[id] = true
			}
		}
		var missing []string
		for _, id := range wanted {
			if !found[id] && !slices.Contains(missing, id) {
				missing = append(missing, id)
			}
		}
		if len(missing) > 0 {
			slices.Sort(missing)
			return 1, &gateway.GatewayClientError{Msg: "Not an unknown (lost) admission of this session: " + strings.Join(missing, ", ")}
		}
	}
	if len(chosen) == 0 {
		fmt.Fprintf(w, "Nothing to discard: session %s has no unknown admission.\n", anyutil.String(sessionID))
		return 0, nil
	}
	if !nsTruthy(ns, "yes") {
		if !console().StdinIsTTY() {
			fmt.Fprintln(stderrWriter(), "Refusing to discard without confirmation on a non-interactive stdin; pass --yes.")
			return 2, nil
		}
		confirmed, err := discardConfirm(w, anyutil.String(sessionID), chosen)
		if err != nil {
			return 1, err
		}
		if !confirmed {
			fmt.Fprintln(w, "Cancelled; nothing was discarded.")
			return 1, nil
		}
	}
	for _, row := range chosen {
		if _, err := client.RPCTimeout(ctx, "prompt.resolve_unknown", map[string]any{"session_id": sessionID, "admission_id": row["admission_id"],
			"execution_generation": row["execution_generation"]}, 0); err != nil {
			return 1, err
		}
		fmt.Fprintf(w, "Discarded %s\n", anyutil.String(row["admission_id"]))
	}
	return 0, nil
}

// Whether an error is a WebSocket protocol failure, a close frame or a refused handshake
func isWebSocketError(err error) bool {
	var closed *websocket.CloseError
	return errors.As(err, &closed) || errors.Is(err, websocket.ErrBadHandshake) || errors.Is(err, websocket.ErrCloseSent)
}

// Acknowledges the turns a session lost in a gateway crash, reporting a gateway failure on stderr
func cmdDiscard(ctx context.Context, h string, ns map[string]any, w io.Writer) (int, error) {
	code, err := discard(ctx, ns, w)
	if err == nil {
		return code, nil
	}
	var clientErr *gateway.GatewayClientError
	switch {
	case errors.As(err, &clientErr):
		fmt.Fprintln(stderrWriter(), "Error: "+clientErr.Msg)
	case errorutil.IsSystem(err), errors.Is(err, context.DeadlineExceeded), os.IsTimeout(err), isWebSocketError(err):
		fmt.Fprintln(stderrWriter(), "Error: Gateway connection/read failed")
	default:
		return 1, err
	}
	return 1, nil
}
