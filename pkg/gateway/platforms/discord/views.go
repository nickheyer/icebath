package discord

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// discord.py Color presets the component views paint their embeds with
const (
	colorGreen    = 0x2ecc71
	colorRed      = 0xe74c3c
	colorBlue     = 0x3498db
	colorPurple   = 0x9b59b6
	colorGreyple  = 0x99aab5
	colorGold     = 0xf1c40f
	colorOrange   = 0xe67e22
	colorDarkGrey = 0x607d8b
)

// Discord component caps in UTF-16 units and the select, row and child limits of one view
const (
	discordSelectFieldLimit    = 100
	discordSelectHintLimit     = 150
	discordSelectMaxOptions    = 25
	discordSelectMaxRows       = 5
	discordModelSelectCapacity = (discordSelectMaxRows - 2) * discordSelectMaxOptions
	discordButtonLabelLimit    = 80
	discordEmbedTitleLimit     = 256
	discordEllipsis            = "…"
	discordViewMaxChildren     = 25
	discordActionRowWidth      = 5
)

// Component payload in the exact shape discord.py serialises, accepted wherever discordgo takes a component
type wireComponent map[string]any

func (c wireComponent) Type() discordgo.ComponentType {
	t, _ := c["type"].(int)
	return discordgo.ComponentType(t)
}

func (c wireComponent) MarshalJSON() ([]byte, error) { return json.Marshal(map[string]any(c)) }

// Select menu option with an optional description
type selectOption struct {
	label, value, description string
}

// Button or select of a view with its wire fields, rendered row and click callback
type viewItem struct {
	kind     discordgo.ComponentType
	customID string
	label    string
	style    discordgo.ButtonStyle
	emoji    string
	disabled bool
	hint     string
	options  []selectOption
	row      int
	callback func(ctx context.Context, i *discordgo.Interaction) error
}

// Row width the item takes, a select fills a whole action row
func (it *viewItem) width() int {
	if it.kind == discordgo.SelectMenuComponent {
		return discordActionRowWidth
	}
	return 1
}

// Item payload as discord.py writes it, empty optional fields left out
func (it *viewItem) payload() map[string]any {
	if it.kind == discordgo.SelectMenuComponent {
		p := map[string]any{"type": int(discordgo.SelectMenuComponent), "custom_id": it.customID, "min_values": 1, "max_values": 1, "disabled": it.disabled}
		if it.hint != "" {
			p["placeholder"] = it.hint
		}
		if len(it.options) > 0 {
			opts := make([]map[string]any, 0, len(it.options))
			for _, o := range it.options {
				op := map[string]any{"label": o.label, "value": o.value, "default": false}
				if o.description != "" {
					op["description"] = o.description
				}
				opts = append(opts, op)
			}
			p["options"] = opts
		}
		return p
	}
	p := map[string]any{"type": int(discordgo.ButtonComponent), "style": int(it.style), "disabled": it.disabled}
	if it.label != "" {
		p["label"] = it.label
	}
	if it.customID != "" {
		p["custom_id"] = it.customID
	}
	if it.emoji != "" {
		p["emoji"] = map[string]any{"name": it.emoji}
	}
	return p
}

// Item repr the view error log names, discord.py style
func (it *viewItem) repr() string {
	if it.kind == discordgo.SelectMenuComponent {
		return fmt.Sprintf("<Select placeholder=%s min_values=1 max_values=1 disabled=%s>", pyrepr.Repr(it.hint), pyrepr.Repr(it.disabled))
	}
	return fmt.Sprintf("<Button style=%d disabled=%s label=%s>", int(it.style), pyrepr.Repr(it.disabled), pyrepr.Repr(it.label))
}

// Thirty two hex digits, the custom id discord.py gives an item declared without one
func randomCustomID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Hooks a concrete view overrides, its click authorisation and its timeout handler
type viewHooks interface {
	checkAuth(ctx context.Context, i *discordgo.Interaction) bool
	onTimeout(ctx context.Context)
}

// Component set bound to one message with the discord.py View timeout, stop and dispatch rules
type uiView struct {
	name     string
	hooks    viewHooks
	mu       sync.Mutex
	timeout  time.Duration
	children []*viewItem
	weights  [discordSelectMaxRows]int
	finished bool
	expiry   time.Time
	timer    *time.Timer
	store    *viewStore
	cacheKey string
	ctx      context.Context
}

// Places an item in the first row with room, failing past 25 children or five full rows
func (v *uiView) addItemLocked(it *viewItem) error {
	if len(v.children) >= discordViewMaxChildren {
		return errors.New("maximum number of children exceeded")
	}
	for row, weight := range v.weights {
		if weight+it.width() <= discordActionRowWidth {
			v.weights[row] += it.width()
			it.row = row
			v.children = append(v.children, it)
			return nil
		}
	}
	return errors.New("could not find open space for item")
}

// Drops one item and frees its row space
func (v *uiView) removeItemLocked(it *viewItem) {
	for idx, c := range v.children {
		if c == it {
			v.children = append(v.children[:idx:idx], v.children[idx+1:]...)
			v.weights[it.row] -= it.width()
			return
		}
	}
}

// Empties the view and its row weights
func (v *uiView) clearItemsLocked() {
	v.children = nil
	v.weights = [discordSelectMaxRows]int{}
}

// Disables every button and select of the view
func (v *uiView) disableAllLocked() {
	for _, c := range v.children {
		c.disabled = true
	}
}

// Action rows of the children grouped by rendered row, the empty list for an empty view
func (v *uiView) componentsLocked() []discordgo.MessageComponent {
	items := append([]*viewItem(nil), v.children...)
	sort.SliceStable(items, func(x, y int) bool { return items[x].row < items[y].row })
	out := []discordgo.MessageComponent{}
	for start := 0; start < len(items); {
		end := start
		var row []map[string]any
		for end < len(items) && items[end].row == items[start].row {
			row = append(row, items[end].payload())
			end++
		}
		out = append(out, wireComponent{"type": int(discordgo.ActionsRowComponent), "components": row})
		start = end
	}
	return out
}

// Pushes the expiry a full timeout out, starting the timer on first use
func (v *uiView) armTimeoutLocked() {
	if v.timeout <= 0 {
		return
	}
	v.expiry = time.Now().Add(v.timeout)
	if v.timer == nil {
		v.timer = time.AfterFunc(v.timeout, v.timeoutTick)
	}
}

// Times the view out once its expiry passed, waiting again while clicks keep refreshing it
func (v *uiView) timeoutTick() {
	v.mu.Lock()
	if v.finished || v.timer == nil {
		v.mu.Unlock()
		return
	}
	if wait := time.Until(v.expiry); wait > 0 {
		v.timer.Reset(wait)
		v.mu.Unlock()
		return
	}
	v.finished, v.timer = true, nil
	store, key, ctx := v.store, v.cacheKey, v.ctx
	v.store = nil
	v.mu.Unlock()
	if store != nil {
		store.removeView(key, v)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	v.hooks.onTimeout(ctx)
}

// Finishes the view, cancelling its timeout and detaching it from the message
func (v *uiView) stop() {
	v.mu.Lock()
	v.finished = true
	if v.timer != nil {
		v.timer.Stop()
		v.timer = nil
	}
	store, key := v.store, v.cacheKey
	v.store = nil
	v.mu.Unlock()
	if store != nil {
		store.removeView(key, v)
	}
}

// Whether the view stopped or timed out
func (v *uiView) isFinished() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.finished
}

// View repr the error log names, discord.py style
func (v *uiView) repr() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return fmt.Sprintf("<%s timeout=%s children=%d>", v.name, pyrepr.Repr(v.timeout.Seconds()), len(v.children))
}

// Live views keyed by the message they are attached to
type viewStore struct {
	mu    sync.Mutex
	views map[string]*uiView
}

// Attaches a live view to a message and restarts its timeout, nothing once the view finished
func (s *viewStore) storeView(ctx context.Context, v *uiView, messageID string) {
	v.mu.Lock()
	if v.finished {
		v.mu.Unlock()
		return
	}
	prevKey := v.cacheKey
	v.store, v.cacheKey, v.ctx = s, messageID, context.WithoutCancel(ctx)
	v.armTimeoutLocked()
	v.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.views == nil {
		s.views = map[string]*uiView{}
	}
	if prevKey != messageID && s.views[prevKey] == v {
		delete(s.views, prevKey)
	}
	s.views[messageID] = v
}

// Detaches a view from its message when it is still the one attached
func (s *viewStore) removeView(messageID string, v *uiView) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.views[messageID] == v {
		delete(s.views, messageID)
	}
}

// View attached to a message, nil when none is live
func (s *viewStore) lookup(messageID string) *uiView {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.views[messageID]
}

// Routes a component click to the item of the live view that owns its custom id, refreshing the timeout
func (a *DiscordAdapter) dispatchComponent(ctx context.Context, i *discordgo.Interaction) {
	if i.Type != discordgo.InteractionMessageComponent || i.Message == nil {
		return
	}
	data := i.MessageComponentData()
	v := a.views.lookup(i.Message.ID)
	if v == nil {
		return
	}
	v.mu.Lock()
	var item *viewItem
	if !v.finished {
		for _, c := range v.children {
			if c.customID == data.CustomID && c.kind == data.ComponentType {
				item = c
			}
		}
	}
	if item == nil {
		v.mu.Unlock()
		return
	}
	v.armTimeoutLocked()
	callback := item.callback
	v.mu.Unlock()
	if err := callback(ctx, i); err != nil {
		slog.Error(fmt.Sprintf("Ignoring exception in view %s for item %s: %s", v.repr(), item.repr(), err))
	}
}

// Shared plumbing of the hermes component views, allowlist auth, the single use flag and the sent message
type hermesView struct {
	uiView
	adapter        *DiscordAdapter
	allowedUserIDs strSet
	allowedRoleIDs strSet
	liveAuth       func(*discordgo.Interaction) *bool
	resolved       bool
	message        *discordgo.Message
}

// Fills the shared view state, the timeout in seconds as discord.py takes it
func (h *hermesView) initView(a *DiscordAdapter, name string, hooks viewHooks, users, roles strSet, timeoutSeconds float64) {
	if roles == nil {
		roles = strSet{}
	}
	h.adapter, h.name, h.hooks, h.allowedUserIDs, h.allowedRoleIDs = a, name, hooks, users, roles
	h.timeout = time.Duration(timeoutSeconds * float64(time.Second))
}

// Adds a button bound to a callback, a random custom id standing in for an unset one
func (h *hermesView) addButtonLocked(label string, style discordgo.ButtonStyle, emoji, customID string, cb func(context.Context, *discordgo.Interaction) error) (*viewItem, error) {
	if customID == "" {
		customID = randomCustomID()
	}
	it := &viewItem{kind: discordgo.ButtonComponent, label: label, style: style, emoji: emoji, customID: customID, callback: cb}
	return it, h.addItemLocked(it)
}

// Adds a single choice select bound to a callback, a random custom id standing in for an unset one
func (h *hermesView) addSelectLocked(hint string, options []selectOption, customID string, cb func(context.Context, *discordgo.Interaction) error) error {
	if customID == "" {
		customID = randomCustomID()
	}
	return h.addItemLocked(&viewItem{kind: discordgo.SelectMenuComponent, hint: hint, options: options, customID: customID, callback: cb})
}

// Click authorisation against the view allowlists confirmed by the live gateway check
func (h *hermesView) checkAuth(ctx context.Context, i *discordgo.Interaction) bool {
	h.mu.Lock()
	live := h.liveAuth
	h.mu.Unlock()
	return h.adapter.componentCheckAuth(ctx, i, h.allowedUserIDs, h.allowedRoleIDs, live)
}

// Ephemerally refuses an answered or unauthorised click, true when the click may proceed
func (h *hermesView) gate(ctx context.Context, i *discordgo.Interaction, resolvedMsg *string, unauthMsg string) (bool, error) {
	h.mu.Lock()
	resolved := h.resolved
	h.mu.Unlock()
	if resolvedMsg != nil && resolved {
		return false, h.adapter.respondEphemeral(ctx, i, *resolvedMsg)
	}
	if !h.hooks.checkAuth(ctx, i) {
		return false, h.adapter.respondEphemeral(ctx, i, unauthMsg)
	}
	return true, nil
}

// Whether a click already answered the view
func (h *hermesView) isResolved() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.resolved
}

// First embed of a message, nil when it has none
func firstEmbed(msg *discordgo.Message) *discordgo.MessageEmbed {
	if msg == nil || len(msg.Embeds) == 0 {
		return nil
	}
	return msg.Embeds[0]
}

// Copy of an embed with a new colour and footer, nil for no embed
func stampedEmbed(embed *discordgo.MessageEmbed, color int, footer string) *discordgo.MessageEmbed {
	if embed == nil {
		return nil
	}
	out := *embed
	out.Color = color
	out.Footer = &discordgo.MessageEmbedFooter{Text: footer}
	return &out
}

// Greys the sent card and stamps a footer, its controls kept disabled or removed, failures only logged
func (h *hermesView) expireEmbed(ctx context.Context, footer string, keepControls bool) {
	h.mu.Lock()
	msg := h.message
	if msg == nil {
		h.mu.Unlock()
		return
	}
	embed := stampedEmbed(firstEmbed(msg), colorGreyple, footer)
	if embed != nil {
		msg.Embeds[0] = embed
	}
	comps := []discordgo.MessageComponent{}
	var view *uiView
	if keepControls {
		comps, view = h.componentsLocked(), &h.uiView
	}
	h.mu.Unlock()
	if err := h.adapter.editViewMessage(ctx, msg, embed, comps, view); err != nil {
		slog.Debug("[Discord] Could not stamp prompt card", "error", err)
	}
}

// Marks the view answered, stamps the clicked card, disables its controls and edits in place, stopping the view either way
func (h *hermesView) finalizeEmbed(ctx context.Context, i *discordgo.Interaction, color int, footer string) error {
	h.mu.Lock()
	h.resolved = true
	embed := stampedEmbed(firstEmbed(i.Message), color, footer)
	h.disableAllLocked()
	comps := h.componentsLocked()
	h.mu.Unlock()
	defer h.stop()
	return h.adapter.respondEditMessage(ctx, i, embed, comps, &h.uiView)
}

// Relabels buttons from the catalog in the active language within the 80 unit label cap
func (h *hermesView) localizeButtonsLocked(ctx context.Context, keys map[*viewItem]string) {
	for item, key := range keys {
		if item != nil {
			item.label = tDiscord(ctx, key, discordButtonLabelLimit, nil)
		}
	}
}

// Disables the controls and greys the card with the expiry footer unless a click already answered it
func (h *hermesView) onTimeout(ctx context.Context) {
	h.mu.Lock()
	if h.resolved {
		h.mu.Unlock()
		return
	}
	h.resolved = true
	h.disableAllLocked()
	h.mu.Unlock()
	h.expireEmbed(ctx, i18n.T(ctx, "platform.discord.prompt.expired_footer", "", nil), true)
}

// Rich embed with a title, description and colour, empty fields left out
func newEmbed(title, description string, color int) *discordgo.MessageEmbed {
	return &discordgo.MessageEmbed{Type: discordgo.EmbedTypeRich, Title: title, Description: description, Color: color}
}

// Embed list of an edit, the empty list clearing the embeds when there is none
func embedList(embed *discordgo.MessageEmbed) []*discordgo.MessageEmbed {
	if embed == nil {
		return []*discordgo.MessageEmbed{}
	}
	return []*discordgo.MessageEmbed{embed}
}

// Answers a component click by editing its message in place, keeping a live view attached
func (a *DiscordAdapter) respondEditMessage(ctx context.Context, i *discordgo.Interaction, embed *discordgo.MessageEmbed, comps []discordgo.MessageComponent, view *uiView) error {
	client := a.liveClient()
	if client == nil {
		return errors.New("Not connected")
	}
	if comps == nil {
		comps = []discordgo.MessageComponent{}
	}
	data := map[string]any{"embeds": embedList(embed), "components": comps}
	if a.allowedMentions != nil {
		data["allowed_mentions"] = a.allowedMentions
	}
	payload := map[string]any{"type": int(discordgo.InteractionResponseUpdateMessage), "data": data}
	endpoint := discordgo.EndpointInteractionResponse(i.ID, i.Token)
	if _, err := client.RequestWithBucketID(http.MethodPost, endpoint, payload, endpoint, discordgo.WithContext(ctx)); err != nil {
		return dpyError(err)
	}
	if view != nil && i.Message != nil {
		a.views.storeView(ctx, view, i.Message.ID)
	}
	return nil
}

// Acknowledges a component click without changing its message
func (a *DiscordAdapter) respondDeferUpdate(ctx context.Context, i *discordgo.Interaction) error {
	client := a.liveClient()
	if client == nil {
		return errors.New("Not connected")
	}
	if err := client.InteractionRespond(i, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseDeferredMessageUpdate}, discordgo.WithContext(ctx)); err != nil {
		return dpyError(err)
	}
	return nil
}

// Posts a followup message after an answered interaction without waiting for its body
func (a *DiscordAdapter) followupSend(ctx context.Context, i *discordgo.Interaction, content string) error {
	client := a.liveClient()
	if client == nil {
		return errors.New("Not connected")
	}
	if _, err := client.FollowupMessageCreate(i, false, &discordgo.WebhookParams{Content: content, AllowedMentions: a.allowedMentions}, discordgo.WithContext(ctx)); err != nil {
		return dpyError(err)
	}
	return nil
}

// Replaces the embed of an answered interaction message and removes its controls
func (a *DiscordAdapter) editOriginalResponse(ctx context.Context, i *discordgo.Interaction, embed *discordgo.MessageEmbed) error {
	client := a.liveClient()
	if client == nil {
		return errors.New("Not connected")
	}
	embeds, comps := embedList(embed), []discordgo.MessageComponent{}
	if _, err := client.InteractionResponseEdit(i, &discordgo.WebhookEdit{Embeds: &embeds, Components: &comps, AllowedMentions: a.allowedMentions}, discordgo.WithContext(ctx)); err != nil {
		return dpyError(err)
	}
	return nil
}

// Edits the embed and controls of a sent prompt message, keeping a live view attached
func (a *DiscordAdapter) editViewMessage(ctx context.Context, msg *discordgo.Message, embed *discordgo.MessageEmbed, comps []discordgo.MessageComponent, view *uiView) error {
	client := a.liveClient()
	if client == nil {
		return errors.New("Not connected")
	}
	embeds := embedList(embed)
	if _, err := client.ChannelMessageEditComplex(&discordgo.MessageEdit{ID: msg.ID, Channel: msg.ChannelID, Embeds: &embeds, Components: &comps, AllowedMentions: a.allowedMentions}, discordgo.WithContext(ctx)); err != nil {
		return dpyError(err)
	}
	if view != nil {
		a.views.storeView(ctx, view, msg.ID)
	}
	return nil
}
