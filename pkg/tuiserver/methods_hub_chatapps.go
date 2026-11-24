package tuiserver

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/webhooksubs"
)

// Peer names the peer command takes, checked here too so a refusal comes back as text instead of a line on the service's stderr
var chatAppsPeerName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

var chatAppsRegistry = &handlerRegistry{}

func init() {
	scopedRPC(chatAppsRegistry, tuigateway.ChatAppsList.Name(), 5070, rpcShape{}, chatAppsList)
	scopedRPC(chatAppsRegistry, tuigateway.ChatAppsPairing.Name(), 5071, rpcShape{}, chatAppsPairing)
	scopedRPC(chatAppsRegistry, tuigateway.ChatAppsWebhook.Name(), 5072, rpcShape{}, chatAppsWebhook)
	scopedRPC(chatAppsRegistry, tuigateway.ChatAppsPeer.Name(), 5073, rpcShape{}, chatAppsPeer)
	scopedRPC(chatAppsRegistry, tuigateway.ChatAppsSend.Name(), 5074, rpcShape{}, chatAppsSend)
	addLongHandlers(tuigateway.ChatAppsList.Name(), tuigateway.ChatAppsWebhook.Name(), tuigateway.ChatAppsSend.Name())
	chatAppsRegistry.install()
}

// Lines a command printed, the blank lines around them dropped
func chatAppsLines(out ...*bytes.Buffer) []string {
	var lines []string
	for _, b := range out {
		for _, line := range strings.Split(b.String(), "\n") {
			if line = strings.TrimRight(line, " \r"); strings.TrimSpace(line) != "" {
				lines = append(lines, strings.TrimSpace(line))
			}
		}
	}
	return lines
}

// Answer of a chat apps change: its lines, or the lines as the failure when the command ended with a refusal
func chatAppsAnswer(rid any, code int, lines []string) map[string]any {
	if code != 0 {
		text := strings.Join(lines, " ")
		if text == "" {
			text = fmt.Sprintf("the command ended with code %d", code)
		}
		return rpcErr(rid, 4070, text, nil)
	}
	return ok(rid, tuigateway.Plain(tuigateway.ChatAppsResult{Lines: lines}))
}

// Chat app commands of the gateway command line, wired by the binary so this package does not import it
var ChatAppCommands struct {
	// Runs icebath webhook with the fields of the webhook form
	Webhook func(ctx context.Context, w io.Writer, p tuigateway.ChatAppsWebhookParams) (int, error)
	// Runs icebath send with its parsed words
	Send func(ctx context.Context, h string, ns map[string]any, w, errw io.Writer) (int, error)
}

// Text of a stored value, empty for nil
func chatAppsText(v any) string {
	if v == nil {
		return ""
	}
	return pyrepr.Str(v)
}

// Platforms of the profile with their state, pairing, webhooks and peers, and whether the background service starts with the computer
func chatAppsList(ctx context.Context, rid any, raw map[string]any) (map[string]any, error) {
	if _, err := tuigateway.ChatAppsList.Params(raw); err != nil {
		return rpcErr(rid, 4000, err.Error(), nil), nil
	}
	h := home.Dir(ctx)
	out := tuigateway.ChatAppsListResult{Platforms: []tuigateway.ChatAppPlatform{}, Pending: []tuigateway.ChatAppPairing{},
		Approved: []tuigateway.ChatAppUser{}, Webhooks: []tuigateway.ChatAppWebhook{}, Peers: []tuigateway.ChatAppPeer{}}
	cfg, err := gateway.LoadGatewayConfig(ctx)
	if err != nil {
		return nil, err
	}
	targets := chatAppsTargets(ctx)
	status := gateway.ReadRuntimeStatus("")
	var states *pyjson.Object
	if status != nil {
		states, _ = pyjson.AsObject(status.GetOr("platforms", nil))
	}
	profile := home.ProfileNameFor(h)
	for name, pc := range cfg.Platforms {
		platform := string(name)
		if platform == "local" || pc == nil || !pc.Enabled {
			continue
		}
		row := tuigateway.ChatAppPlatform{Platform: platform, Enabled: pc.Enabled, Targets: targets[platform]}
		if hc := pc.HomeChannel; hc != nil {
			row.Home = chatAppsText(hc.Name)
			if row.Home == "" {
				row.Home = hc.ChatID
			}
		}
		if states != nil {
			entry, found := pyjson.AsObject(states.GetOr(platform, nil))
			if !found && profile != "" {
				entry, found = pyjson.AsObject(states.GetOr(profile+":"+platform, nil))
			}
			if found {
				row.State, row.Error = chatAppsText(entry.GetOr("state", nil)), chatAppsText(entry.GetOr("error_message", nil))
			}
		}
		out.Platforms = append(out.Platforms, row)
	}
	sort.Slice(out.Platforms, func(i, j int) bool { return out.Platforms[i].Platform < out.Platforms[j].Platform })
	store, err := gateway.NewPairingStore(ctx, "")
	if err != nil {
		return nil, err
	}
	pending, err := store.ListPending(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, p := range pending {
		out.Pending = append(out.Pending, tuigateway.ChatAppPairing{Platform: p.Platform, RequestID: p.RequestID, UserID: chatAppsText(p.UserID),
			UserName: chatAppsText(p.UserName), Minutes: int64(p.AgeMinutes)})
	}
	approved, err := store.ListApproved("")
	if err != nil {
		return nil, err
	}
	for _, a := range approved {
		out.Approved = append(out.Approved, tuigateway.ChatAppUser{Platform: chatAppsText(a.GetOr("platform", nil)), UserID: chatAppsText(a.GetOr("user_id", nil)),
			UserName: chatAppsText(a.GetOr("user_name", nil))})
	}
	seen := map[string]bool{}
	for _, p := range append(append([]string{}, chatAppsPlatformNames(out.Platforms)...), chatAppsPendingPlatforms(out.Pending)...) {
		if seen[p] {
			continue
		}
		seen[p] = true
		if locked, err := store.IsLockedOut(p); err == nil && locked {
			out.LockedOut = append(out.LockedOut, p)
		} else if err != nil {
			return nil, err
		}
	}
	out.WebhooksEnabled = webhooksubs.WebhookEnabled(ctx, h)
	subs := webhooksubs.Load(h)
	for _, name := range subs.Keys {
		route, isObject := pyjson.AsObject(subs.Vals[name])
		if !isObject {
			continue
		}
		out.Webhooks = append(out.Webhooks, chatAppsWebhookRow(ctx, h, name, route))
	}
	peers, err := chatAppsPeers(ctx, h)
	if err != nil {
		return nil, err
	}
	out.Peers = peers
	if out.ServiceInstalled, err = gateway.IsServiceInstalled(ctx); err != nil {
		return nil, err
	}
	return ok(rid, tuigateway.Plain(out)), nil
}

// Names of the platforms of the rows
func chatAppsPlatformNames(rows []tuigateway.ChatAppPlatform) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Platform
	}
	return out
}

// Platforms the pending requests came from
func chatAppsPendingPlatforms(rows []tuigateway.ChatAppPairing) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Platform
	}
	return out
}

// Chats each platform found, as the targets the send command takes, from the same directory `icebath send --list` reads
func chatAppsTargets(ctx context.Context) map[string][]tuigateway.ChatAppTarget {
	out := map[string][]tuigateway.ChatAppTarget{}
	directory := gateway.LoadChannelDirectory(ctx)
	if directory == nil {
		return out
	}
	platforms, ok := pyjson.AsObject(directory.GetOr("platforms", nil))
	if !ok {
		return out
	}
	for _, name := range platforms.Keys {
		channels, _ := platforms.Vals[name].([]any)
		for _, raw := range channels {
			channel, ok := pyjson.AsObject(raw)
			if !ok {
				continue
			}
			label := chatAppsText(channel.GetOr("name", nil))
			id := chatAppsText(channel.GetOr("id", nil))
			if id == "" {
				id = label
			}
			if label == "" {
				label = id
			}
			if id != "" {
				out[name] = append(out[name], tuigateway.ChatAppTarget{Target: name + ":" + id, Label: label})
			}
		}
	}
	return out
}

// One webhook route as the screen shows it
func chatAppsWebhookRow(ctx context.Context, h, name string, route *pyjson.Object) tuigateway.ChatAppWebhook {
	join := func(v any) string {
		items, ok := v.([]any)
		if !ok {
			return chatAppsText(v)
		}
		parts := make([]string, 0, len(items))
		for _, item := range items {
			parts = append(parts, chatAppsText(item))
		}
		return strings.Join(parts, ", ")
	}
	return tuigateway.ChatAppWebhook{Name: name, Description: chatAppsText(route.GetOr("description", nil)), Events: join(route.GetOr("events", nil)),
		Prompt: chatAppsText(route.GetOr("prompt", nil)), Skills: join(route.GetOr("skills", nil)), Deliver: chatAppsText(route.GetOr("deliver", "log")),
		DeliverChat: chatAppsText(route.GetOr("deliver_chat_id", nil)), DeliverOnly: pyrepr.Truthy(route.GetOr("deliver_only", nil)),
		URL: webhooksubs.RouteURL(ctx, h, name, route), Created: chatAppsText(route.GetOr("created_at", nil))}
}

// Variable of the home's .env that holds a peer's key, as the peer command names it
func chatAppsPeerKeyEnv(name string) string {
	return "ICEBATH_PEER_" + strings.ReplaceAll(strings.ToUpper(name), "-", "_") + "_KEY"
}

// Peers of the config's bot_peers with whether their key is set
func chatAppsPeers(ctx context.Context, h string) ([]tuigateway.ChatAppPeer, error) {
	cfg, err := config.LoadContext(ctx, h, config.NewSecrets(h).Lookup())
	if err != nil {
		return nil, err
	}
	peers, _ := cfg["bot_peers"].(map[string]any)
	names := make([]string, 0, len(peers))
	for name := range peers {
		names = append(names, name)
	}
	sort.Strings(names)
	out := []tuigateway.ChatAppPeer{}
	for _, name := range names {
		entry, _ := peers[name].(map[string]any)
		env := chatAppsPeerKeyEnv(name)
		key, _, err := provider.GetSecret(ctx, env)
		if err != nil {
			return nil, err
		}
		out = append(out, tuigateway.ChatAppPeer{Name: name, URL: chatAppsText(entry["url"]), Note: chatAppsText(entry["note"]), KeySet: strings.TrimSpace(key) != "", KeyEnv: env})
	}
	return out, nil
}

// Approves a pairing request or code, revokes an approved person, or clears the pending requests of a platform
func chatAppsPairing(ctx context.Context, rid any, raw map[string]any) (map[string]any, error) {
	p, err := tuigateway.ChatAppsPairing.Params(raw)
	if err != nil {
		return rpcErr(rid, 4000, err.Error(), nil), nil
	}
	if strings.TrimSpace(p.Platform) == "" {
		return rpcErr(rid, 4063, "platform required", nil), nil
	}
	ns := map[string]any{"pairing_action": p.Action, "platform": p.Platform}
	switch p.Action {
	case "approve":
		ns["code"] = p.Code
	case "revoke":
		ns["user_id"] = p.UserID
	case "clear":
		store, err := gateway.NewPairingStore(ctx, "")
		if err != nil {
			return nil, err
		}
		n, err := store.ClearPending(ctx, p.Platform)
		if err != nil {
			return nil, err
		}
		return ok(rid, tuigateway.Plain(tuigateway.ChatAppsResult{Lines: []string{fmt.Sprintf("Cleared %d pending request(s) of %s.", n, p.Platform)}})), nil
	default:
		return rpcErr(rid, 4000, "unknown pairing action "+p.Action, nil), nil
	}
	var buf bytes.Buffer
	code, err := gateway.PairingCommand(ctx, ns, &buf)
	if err != nil {
		return nil, err
	}
	return chatAppsAnswer(rid, code, chatAppsLines(&buf)), nil
}

// Adds or replaces a webhook route, removes one, or posts a signed test payload to one, through the webhook command
func chatAppsWebhook(ctx context.Context, rid any, raw map[string]any) (map[string]any, error) {
	p, err := tuigateway.ChatAppsWebhook.Params(raw)
	if err != nil {
		return rpcErr(rid, 4000, err.Error(), nil), nil
	}
	if strings.TrimSpace(p.Name) == "" {
		return rpcErr(rid, 4063, "name required", nil), nil
	}
	switch p.Action {
	case "subscribe":
		if p.Deliver == "" {
			p.Deliver = "log"
		}
	case "remove", "test":
	default:
		return rpcErr(rid, 4000, "unknown webhook action "+p.Action, nil), nil
	}
	var buf bytes.Buffer
	code, err := ChatAppCommands.Webhook(ctx, &buf, p)
	if err != nil {
		return nil, err
	}
	return chatAppsAnswer(rid, code, chatAppsLines(&buf)), nil
}

// Adds or updates a peer or removes one, through the peer command
func chatAppsPeer(ctx context.Context, rid any, raw map[string]any) (map[string]any, error) {
	p, err := tuigateway.ChatAppsPeer.Params(raw)
	if err != nil {
		return rpcErr(rid, 4000, err.Error(), nil), nil
	}
	name := strings.ToLower(strings.TrimSpace(p.Name))
	ns := map[string]any{"peer_action": p.Action, "name": name}
	switch p.Action {
	case "add":
		if !chatAppsPeerName.MatchString(name) {
			return rpcErr(rid, 4070, "Peer names are lowercase letters, digits, - and _, at most 64.", nil), nil
		}
		lowered := strings.ToLower(strings.TrimSpace(p.URL))
		if !strings.HasPrefix(lowered, "http://") && !strings.HasPrefix(lowered, "https://") {
			return rpcErr(rid, 4070, "The address must start with http:// or https://, for example http://spark.lan:8377.", nil), nil
		}
		ns["url"], ns["key"], ns["note"] = strings.TrimSpace(p.URL), strings.TrimSpace(p.Key), strings.TrimSpace(p.Note)
	case "remove":
	default:
		return rpcErr(rid, 4000, "unknown peer action "+p.Action, nil), nil
	}
	var buf bytes.Buffer
	code, err := gateway.PeerCommand(ctx, home.Dir(ctx), ns, &buf)
	if err != nil {
		return nil, err
	}
	return chatAppsAnswer(rid, code, chatAppsLines(&buf)), nil
}

// Sends a message to a chat of a configured platform, through the send command
func chatAppsSend(ctx context.Context, rid any, raw map[string]any) (map[string]any, error) {
	p, err := tuigateway.ChatAppsSend.Params(raw)
	if err != nil {
		return rpcErr(rid, 4000, err.Error(), nil), nil
	}
	if strings.TrimSpace(p.Target) == "" || strings.TrimSpace(p.Message) == "" {
		return rpcErr(rid, 4063, "target and message required", nil), nil
	}
	ns := map[string]any{"to": strings.TrimSpace(p.Target), "message": p.Message}
	if p.Subject != "" {
		ns["subject"] = p.Subject
	}
	var out, errOut bytes.Buffer
	code, err := ChatAppCommands.Send(ctx, home.Dir(ctx), ns, &out, &errOut)
	if err != nil {
		return nil, err
	}
	return chatAppsAnswer(rid, code, chatAppsLines(&out, &errOut)), nil
}
