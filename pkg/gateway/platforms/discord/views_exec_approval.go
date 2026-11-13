package discord

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/bwmarrin/discordgo"

	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Gateway approval resolver the approval buttons call, the text approve flow's mechanism
var resolveGatewayApprovalFn = tools.ResolveGatewayApproval

// Footer catalog keys of the reasons a request settles with, others reading as cancelled
var execApprovalRetireFooters = map[string]string{
	"resolved": "platform.discord.approval.already_resolved",
	"timeout":  "platform.discord.prompt.expired_footer",
	"gone":     "platform.discord.approval.no_longer_pending",
}

// Allow once, allow session, always allow and deny buttons of a dangerous command card
type execApprovalView struct {
	hermesView
	sessionKey     string
	requestID      string
	pendingRetire  string
	backendSettled bool
	retireMu       sync.Mutex
	requireAdmin   bool
	adminUserIDs   strSet
	allowOnce      *viewItem
	allowSession   *viewItem
	allowAlways    *viewItem
	deny           *viewItem
}

// Exec approval view with the tiers the prompt allows and the optional admin gate
func (a *DiscordAdapter) newExecApprovalView(ctx context.Context, sessionKey string, users, roles strSet, requireAdmin bool, adminUserIDs map[string]struct{},
	allowPermanent, allowSession, smartDenied bool, requestID string) (*execApprovalView, error) {
	timeout, err := ReadDiscordPromptTimeout(ctx)
	if err != nil {
		return nil, err
	}
	admins := strSet{}
	for id := range adminUserIDs {
		if s := pystr.Strip(id); s != "" {
			admins[s] = struct{}{}
		}
	}
	v := &execApprovalView{sessionKey: sessionKey, requestID: requestID, requireAdmin: requireAdmin, adminUserIDs: admins}
	v.initView(a, "ExecApprovalView", v, users, roles, float64(timeout))
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, b := range []struct {
		dst    **viewItem
		label  string
		style  discordgo.ButtonStyle
		choice string
		color  int
		key    string
	}{
		{&v.allowOnce, "Allow Once", discordgo.SuccessButton, "once", colorGreen, "platform.discord.approval.resolved_once"},
		{&v.allowSession, "Allow Session", discordgo.SecondaryButton, "session", colorBlue, "platform.discord.approval.resolved_session"},
		{&v.allowAlways, "Always Allow", discordgo.PrimaryButton, "always", colorPurple, "platform.discord.approval.resolved_always"},
		{&v.deny, "Deny", discordgo.DangerButton, "deny", colorRed, "platform.discord.approval.resolved_deny"},
	} {
		choice, color, key := b.choice, b.color, b.key
		item, err := v.addButtonLocked(b.label, b.style, "", "", func(ctx context.Context, i *discordgo.Interaction) error {
			return v.resolve(ctx, i, choice, color, key)
		})
		if err != nil {
			return nil, err
		}
		*b.dst = item
	}
	v.localizeButtonsLocked(ctx, map[*viewItem]string{v.allowOnce: "gateway.exec_approval.action_once", v.allowSession: "gateway.exec_approval.action_session",
		v.allowAlways: "gateway.exec_approval.action_always", v.deny: "gateway.exec_approval.action_deny"})
	if smartDenied || !allowSession {
		v.removeItemLocked(v.allowSession)
		v.removeItemLocked(v.allowAlways)
	} else if !allowPermanent {
		v.removeItemLocked(v.allowAlways)
	}
	return v, nil
}

// Base admission plus, with the admin gate on, membership in the admins, failing closed with a warning when none exist
func (v *execApprovalView) checkAuth(ctx context.Context, i *discordgo.Interaction) bool {
	if !v.hermesView.checkAuth(ctx, i) {
		return false
	}
	if !v.requireAdmin {
		return true
	}
	uid := ""
	if u := interactionUser(i); u != nil {
		uid = u.ID
	}
	if uid != "" && v.adminUserIDs.has(uid) {
		return true
	}
	if len(v.adminUserIDs) == 0 {
		slog.Warn("[Discord] require_admin_for_exec_approval is enabled but no admins are configured (allow_admin_from is empty) — " +
			"exec approval buttons are disabled for everyone. Add admin user IDs under the discord platform's allow_admin_from, or disable the toggle.")
	}
	return false
}

// UI timeout retires the card without claiming the backend settled
func (v *execApprovalView) onTimeout(ctx context.Context) {
	v.retire(ctx, "timeout", false)
}

// Disables the card once its backend request is no longer pending, a UI timeout never overwriting a settlement
func (v *execApprovalView) retire(ctx context.Context, reason string, backendSettled bool) {
	v.stop()
	v.retireMu.Lock()
	defer v.retireMu.Unlock()
	v.mu.Lock()
	if v.backendSettled {
		v.mu.Unlock()
		return
	}
	v.backendSettled = backendSettled
	v.resolved = true
	v.disableAllLocked()
	v.mu.Unlock()
	key, ok := execApprovalRetireFooters[reason]
	if !ok {
		key = "platform.discord.slash_confirm.resolved_cancel"
	}
	v.expireEmbed(ctx, i18n.T(ctx, key, "", nil), false)
}

// Resolver outcome with a panic of the resolver turned into the error a failed lookup reports
func (v *execApprovalView) resolveApproval(choice string) (count int, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	return resolveGatewayApprovalFn(v.sessionKey, choice, false, "", v.requestID), nil
}

// Resolves the approval through the gateway queue first, then stamps the card for the clicker
func (v *execApprovalView) resolve(ctx context.Context, i *discordgo.Interaction, choice string, color int, labelKey string) error {
	resolvedMsg := i18n.T(ctx, "platform.discord.approval.already_resolved", "", nil)
	if ok, err := v.gate(ctx, i, &resolvedMsg, unauthorized(ctx)); !ok {
		return err
	}
	label := i18n.T(ctx, labelKey, "", nil)
	v.mu.Lock()
	v.resolved, v.backendSettled = true, true
	v.mu.Unlock()
	user := interactionDisplayName(i)
	count, err := v.resolveApproval(choice)
	if err != nil {
		slog.Error(fmt.Sprintf("Failed to resolve gateway approval from button: %s", err))
		v.mu.Lock()
		v.resolved, v.backendSettled = false, false
		v.mu.Unlock()
		return v.adapter.respondEphemeral(ctx, i, i18n.T(ctx, "platform.discord.approval.click_failed", "", nil))
	}
	slog.Info(fmt.Sprintf("Discord button resolved %d approval(s) for session %s (choice=%s, user=%s)", count, v.sessionKey, choice, user))
	footer := label
	if count == 0 {
		v.mu.Lock()
		v.backendSettled = false
		v.mu.Unlock()
		if v.requestID != "" {
			return v.adapter.respondEphemeral(ctx, i, i18n.T(ctx, "platform.discord.approval.no_longer_pending", "", nil))
		}
		color = colorDarkGrey
		footer = i18n.T(ctx, "platform.discord.approval.expired", "", nil)
	} else {
		footer = i18n.T(ctx, "platform.discord.approval.by_user", "", map[string]any{"label": label, "user": user})
	}
	if err := v.finalizeEmbed(ctx, i, color, footer); err != nil {
		if count != 0 {
			v.mu.Lock()
			v.backendSettled = false
			v.mu.Unlock()
			v.retire(ctx, "resolved", true)
		}
		return err
	}
	return nil
}
