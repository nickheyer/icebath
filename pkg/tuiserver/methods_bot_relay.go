package tuiserver

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/botmode"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Relay handle a delivery's target profile must be
var relayHandleRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

// Admission of a relayed DM into a target profile's authority
var relayDeliver = gateway.AuthorityDelivery

// Sender fields a relaying client reports, never forwarded as given
var relaySenderFields = []string{"from_profile", "from_handle", "from_connection"}

var botRelayRegistry = &handlerRegistry{}

func init() {
	botRelayRegistry.method("bot_relay.roster.sync", botRelayRosterSync)
	botRelayRegistry.method("bot_relay.outbox.drain", botRelayOutboxDrain)
	botRelayRegistry.method("bot_relay.deliver", botRelayDeliver)
	botRelayRegistry.method("bot_relay.reply", botRelayReply)
	botRelayRegistry.install()
}

// Install root the relay mailbox of every profile shares
func relayRoot() string { return botmode.InstallRoot(home.Process()) }

// Readable trace of the remote roster rows for the change log
func relayRosterTrace(root string) string {
	rows := botmode.ReadRemoteRoster(root)
	parts := make([]string, len(rows))
	for i, row := range rows {
		parts[i] = fmt.Sprintf("%s/%s=%s", row.ConnectionID, row.Profile, pyrepr.Repr(row.Title))
	}
	return fmt.Sprintf("n=%d [", len(rows)) + strings.Join(parts, " ") + "]"
}

// Replaces this gateway's view of agents on other connections, answering how many rows it accepted
func botRelayRosterSync(ctx context.Context, rid any, params map[string]any) (response map[string]any) {
	defer relayFailure(rid, 5090, &response)
	root := relayRoot()
	before := relayRosterTrace(root)
	count, err := botmode.WriteRemoteRoster(ctx, root, params["agents"])
	if err != nil {
		return rpcErr(rid, 5090, err.Error(), nil)
	}
	if after := relayRosterTrace(root); after != before {
		logger().Info(fmt.Sprintf("bot_relay roster changed: %s -> %s", before, after))
	}
	return ok(rid, map[string]any{"count": count})
}

// Claims every pending cross connection envelope queued here
func botRelayOutboxDrain(ctx context.Context, rid any, _ map[string]any) (response map[string]any) {
	defer relayFailure(rid, 5091, &response)
	envelopes, err := botmode.ClaimPendingEnvelopes(ctx, relayRoot())
	if err != nil {
		return rpcErr(rid, 5091, err.Error(), nil)
	}
	out := make([]any, len(envelopes))
	for i, envelope := range envelopes {
		out[i] = envelope
	}
	return ok(rid, map[string]any{"envelopes": out})
}

// Answers a raised failure of a relay door with its error code and message
func relayFailure(rid any, code int, response *map[string]any) {
	failure := recover()
	if failure == nil {
		return
	}
	if configOutage(failure) {
		panic(failure)
	}
	*response = rpcErr(rid, code, failureText(failure), nil)
}

// Admits a relayed DM into the target profile's authority, the author derived from the caller's identity when logged in
func botRelayDeliver(ctx context.Context, rid any, params map[string]any) map[string]any {
	profile, isText := params["profile"].(string)
	if !gateway.ValidDeliveryID(params["id"]) || !isText || !relayHandleRe.MatchString(profile) {
		return rpcErr(rid, 4090, "invalid_params", map[string]any{"reason": "invalid_params"})
	}
	var author map[string]any
	var identity any
	if identified, carries := tuigateway.CurrentTransport(ctx).(interface{ AuthIdentity() map[string]any }); carries {
		if fields := identified.AuthIdentity(); fields != nil {
			identity = fields
		}
	}
	if gateway.IsAuthenticatedIdentity(identity) {
		principal, err := gateway.AuthenticatedPrincipalDigest(identity.(map[string]any))
		if err != nil {
			panic(err)
		}
		author = provider.RelayingPrincipalAuthor(principal)
	} else {
		author = provider.DeliveryTurnAuthor(params["from_profile"], params["from_handle"], params["from_connection"])
	}
	forwarded := map[string]any{}
	for key, value := range params {
		isSender := false
		for _, field := range relaySenderFields {
			isSender = isSender || key == field
		}
		if !isSender {
			forwarded[key] = value
		}
	}
	if author != nil {
		forwarded["author"] = author
	}
	root := relayRoot()
	roster := botmode.Roster(root)
	names := make([]string, len(roster))
	homes := map[string]string{}
	for i, entry := range roster {
		names[i] = entry.Name
		homes[entry.Name] = entry.Dir
	}
	resolved := profile
	if pystr.Lower(profile) == "icebath" {
		resolved = botmode.ProfileForHandle("icebath", names)
	}
	targetHome, known := homes[resolved]
	if !known {
		return rpcErr(rid, 4092, fmt.Sprintf("no profile '%s' on this gateway", profile), map[string]any{"reason": "unknown_profile"})
	}
	if message, isMessage := forwarded["message"].(string); isMessage {
		taken, err := botmode.LocalTakenForms(root)
		if err != nil {
			panic(err)
		}
		forwarded["message"] = botmode.QualifySenderStamp(message, params["from_handle"], params["from_connection"], botmode.ReadRemoteRoster(root), taken)
	}
	forwarded["profile"] = resolved
	result, err := relayAuthorityDelivery(ctx, targetHome, forwarded)
	if err != nil {
		logger().Debug(fmt.Sprintf("bot_relay.deliver to %s refused", resolved), "error", err)
		return rpcErr(rid, 5094, err.Error(), map[string]any{"reason": "runtime_unavailable"})
	}
	return ok(rid, result)
}

// Delivery into the target authority, a raised failure returned as the refusal it stands for
func relayAuthorityDelivery(ctx context.Context, targetHome string, params map[string]any) (result map[string]any, err error) {
	defer func() {
		if failure := recover(); failure != nil {
			if configOutage(failure) {
				panic(failure)
			}
			err = fmt.Errorf("%s", failureText(failure))
		}
	}()
	return relayDeliver(ctx, targetHome, params)
}

// Writes a relayed reply or error for an envelope so the sender side waiter picks it up
func botRelayReply(ctx context.Context, rid any, params map[string]any) (response map[string]any) {
	envelopeID := strings.TrimSpace(relayString(params["id"]))
	if envelopeID == "" {
		return rpcErr(rid, 4093, "id required", nil)
	}
	defer relayFailure(rid, 5095, &response)
	_, err := botmode.WriteReply(ctx, relayRoot(), envelopeID, relayString(params["reply"]), relayString(params["error"]), relayString(params["reason"]))
	if err != nil {
		var invalid *botmode.InvalidEnvelopeError
		if errors.As(err, &invalid) {
			return rpcErr(rid, 4094, err.Error(), nil)
		}
		return rpcErr(rid, 5095, err.Error(), nil)
	}
	return ok(rid, map[string]any{"ok": true})
}

// Python str of a value or empty string for a falsy one
func relayString(v any) string {
	if !truthy(v) {
		return ""
	}
	return pyrepr.Str(v)
}
