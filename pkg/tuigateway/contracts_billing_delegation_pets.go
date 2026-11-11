package tuigateway

import (
	"github.com/nickheyer/icebath/pkg/internal/pydantic"
)

var BillingEnvelope = pydantic.NewModel("BillingEnvelope", ClosedConfig,
	pydantic.Req("ok", pydantic.Bool),
	pydantic.Def("error", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("message", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("portal_url", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("retry_after", pydantic.Nullable(pydantic.Union(pydantic.Int, pydantic.Float)), nil),
	pydantic.Def("payload", pydantic.Nullable(pydantic.Dict(pydantic.Any)), nil),
	pydantic.Def("actor", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("code", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("recovery", pydantic.Nullable(pydantic.Str), nil),
)

var UsageBarKind = pydantic.Enum("UsageBarKind", "plan", "plan", "topup", "topup")

var UsageBar = pydantic.NewModel("UsageBar", ClosedConfig,
	pydantic.Req("kind", UsageBarKind),
	pydantic.Req("remaining_display", pydantic.Str),
	pydantic.Req("total_display", pydantic.Str),
	pydantic.Req("spent_display", pydantic.Str),
	pydantic.Def("pct_used", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Req("fill_fraction", pydantic.Float),
)

var UsageModel = pydantic.NewModel("UsageModel", ClosedConfig,
	pydantic.Def("ok", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Req("available", pydantic.Bool),
	pydantic.Def("status", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("plan_name", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("renews_at", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("renews_display", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("subscription_remaining_display", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("topup_remaining_display", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("total_spendable_display", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("has_topup", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("plan_bar", pydantic.Nullable(UsageBar), nil),
	pydantic.Def("topup_bar", pydantic.Nullable(UsageBar), nil),
)

var _ = Method("usage.bars", ProfileParams, UsageModel, "Two-bar dollar usage view shared by /usage, /topup and /subscription; fail-open to unavailable.")

var BillingCardInfo = pydantic.NewModel("BillingCardInfo", ClosedConfig,
	pydantic.Req("brand", pydantic.Str),
	pydantic.Req("last4", pydantic.Str),
	pydantic.Req("masked", pydantic.Str),
	pydantic.Def("display", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("resolved_via", pydantic.Nullable(pydantic.Str), nil),
)

var PaymentMethodKind = pydantic.Enum("PaymentMethodKind", "card", "card", "link", "link", "unknown", "unknown")

var BillingPaymentMethod = pydantic.NewModel("BillingPaymentMethod", ClosedConfig,
	pydantic.Req("kind", PaymentMethodKind),
	pydantic.Def("brand", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("last4", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("wallet", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("email", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("raw_kind", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("resolved_via", pydantic.Nullable(pydantic.Str), nil),
)

var BillingMonthlyCap = pydantic.NewModel("BillingMonthlyCap", ClosedConfig,
	pydantic.Def("limit_usd", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Req("limit_display", pydantic.Str),
	pydantic.Def("spent_this_month_usd", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Req("spent_display", pydantic.Str),
	pydantic.Req("is_default_ceiling", pydantic.Bool),
)

var AutoReloadCardKind = pydantic.Enum("AutoReloadCardKind", "canonical", "canonical", "distinct", "distinct", "none", "none")

var BillingAutoReloadCard = pydantic.NewModel("BillingAutoReloadCard", ClosedConfig,
	pydantic.Req("kind", AutoReloadCardKind),
	pydantic.Def("payment_method_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("brand", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("last4", pydantic.Nullable(pydantic.Str), nil),
)

var BillingAutoReload = pydantic.NewModel("BillingAutoReload", ClosedConfig,
	pydantic.Req("enabled", pydantic.Bool),
	pydantic.Def("threshold_usd", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Req("threshold_display", pydantic.Str),
	pydantic.Def("reload_to_usd", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Req("reload_to_display", pydantic.Str),
	pydantic.Def("card", pydantic.Nullable(BillingAutoReloadCard), nil),
)

var BillingStateResult = pydantic.NewModel("BillingStateResult", ClosedConfig,
	pydantic.Req("ok", pydantic.Bool),
	pydantic.Req("logged_in", pydantic.Bool),
	pydantic.Def("free_tier_account", pydantic.Bool, false),
	pydantic.Def("free_tier_model", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("org_name", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("org_slug", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("role", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("is_admin", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("can_change_plan", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("can_charge", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("balance_usd", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("balance_display", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("cli_billing_enabled", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("charge_presets", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
	pydantic.Def("charge_presets_display", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
	pydantic.Def("min_usd", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("max_usd", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("card", pydantic.Nullable(BillingCardInfo), nil),
	pydantic.Def("payment_method", pydantic.Nullable(BillingPaymentMethod), nil),
	pydantic.Def("monthly_cap", pydantic.Nullable(BillingMonthlyCap), nil),
	pydantic.Def("auto_reload", pydantic.Nullable(BillingAutoReload), nil),
	pydantic.Def("keep_going_on_credits", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("portal_url", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("error", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("usage", pydantic.Nullable(UsageModel), nil),
	pydantic.Def("plan_usage", pydantic.Nullable(PlanUsage), nil),
)

var _ = Method("billing.state", ProfileParams, BillingStateResult, "Read-only billing view (no scope); the Nous free tier is answered locally without a portal call.")

var SubscriptionContext = pydantic.Enum("SubscriptionContext", "personal", "personal", "team", "team")

var CurrentSubscription = pydantic.NewModel("CurrentSubscription", ClosedConfig,
	pydantic.Def("tier_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("tier_name", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("monthly_credits", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("credits_remaining", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("cycle_ends_at", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("pending_downgrade_tier_name", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("pending_downgrade_at", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("pending_downgrade_display", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Req("cancel_at_period_end", pydantic.Bool),
	pydantic.Def("cancellation_effective_at", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("cancellation_effective_display", pydantic.Nullable(pydantic.Str), nil),
)

var SubscriptionTierOption = pydantic.NewModel("SubscriptionTierOption", ClosedConfig,
	pydantic.Req("tier_id", pydantic.Str),
	pydantic.Req("name", pydantic.Str),
	pydantic.Req("tier_order", pydantic.Int),
	pydantic.Req("dollars_per_month_display", pydantic.Str),
	pydantic.Def("monthly_credits", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Req("is_current", pydantic.Bool),
	pydantic.Req("is_enabled", pydantic.Bool),
)

var SubscriptionStateResult = pydantic.NewModel("SubscriptionStateResult", ClosedConfig,
	pydantic.Req("ok", pydantic.Bool),
	pydantic.Req("logged_in", pydantic.Bool),
	pydantic.Def("is_admin", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("can_change_plan", pydantic.Nullable(pydantic.Bool), nil),
	pydantic.Def("org_name", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("org_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("role", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("context", pydantic.Nullable(SubscriptionContext), nil),
	pydantic.Def("current", pydantic.Nullable(CurrentSubscription), nil),
	pydantic.Def("tiers", pydantic.Nullable(pydantic.List(SubscriptionTierOption)), nil),
	pydantic.Def("portal_url", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("error", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("usage", pydantic.Nullable(UsageModel), nil),
	pydantic.Def("plan_usage", pydantic.Nullable(PlanUsage), nil),
)

var _ = Method("subscription.state", ProfileParams, SubscriptionStateResult, "Current plan, tier catalog and usage for the picker; fail-open when logged out.")

var SubscriptionPreviewParams = pydantic.Extend(ProfileParams, "SubscriptionPreviewParams", ParamsConfig,
	pydantic.Def("subscription_type_id", pydantic.Nullable(pydantic.Str), nil),
)

var SubscriptionChangeEffect = pydantic.Enum("SubscriptionChangeEffect", "charge_now", "charge_now", "scheduled", "scheduled", "no_op", "no_op", "blocked", "blocked")

var SubscriptionPreviewResult = pydantic.Extend(BillingEnvelope, "SubscriptionPreviewResult", ClosedConfig,
	pydantic.Def("effect", pydantic.Nullable(SubscriptionChangeEffect), nil),
	pydantic.Def("reason", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("current_tier_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("current_tier_name", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("target_tier_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("target_tier_name", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("monthly_credits_delta", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("amount_due_now_cents", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("effective_at", pydantic.Nullable(pydantic.Str), nil),
)

var _ = Method("subscription.preview", SubscriptionPreviewParams, SubscriptionPreviewResult, "Chargeless quote of what a plan change would do (billing:manage).")

var SubscriptionChangeParams = pydantic.Extend(ProfileParams, "SubscriptionChangeParams", ParamsConfig,
	pydantic.Def("subscription_type_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("cancel", pydantic.Bool, false),
)

var BillingPendingChangeResult = pydantic.Extend(BillingEnvelope, "BillingPendingChangeResult", ClosedConfig)

var _ = Method("subscription.change", SubscriptionChangeParams, BillingPendingChangeResult, "Schedule a downgrade / same-price change or a period-end cancellation.")

var _ = Method("subscription.resume", ProfileParams, BillingPendingChangeResult, "Clear a scheduled downgrade / cancellation (re-enables recurring spend).")

var SubscriptionUpgradeParams = pydantic.Extend(ProfileParams, "SubscriptionUpgradeParams", ParamsConfig,
	pydantic.Def("subscription_type_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("idempotency_key", pydantic.Nullable(pydantic.Str), nil),
)

var SubscriptionUpgradeResult = pydantic.Extend(BillingEnvelope, "SubscriptionUpgradeResult", ClosedConfig,
	pydantic.Def("status", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("target_tier_name", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("recovery_url", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("reason", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("idempotency_key", pydantic.Nullable(pydantic.Str), nil),
)

var _ = Method("subscription.upgrade", SubscriptionUpgradeParams, SubscriptionUpgradeResult, "Prorate, charge and flip the plan (billing:manage, idempotent).")

var BillingChargeParams = pydantic.Extend(ProfileParams, "BillingChargeParams", ParamsConfig,
	pydantic.Def("amount_usd", pydantic.Nullable(pydantic.Union(pydantic.Float, pydantic.Str)), nil),
	pydantic.Def("idempotency_key", pydantic.Nullable(pydantic.Str), nil),
)

var BillingChargeResult = pydantic.Extend(BillingEnvelope, "BillingChargeResult", ClosedConfig,
	pydantic.Def("charge_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("idempotency_key", pydantic.Nullable(pydantic.Str), nil),
)

var _ = Method("billing.charge", BillingChargeParams, BillingChargeResult, "Start a one-off top-up charge (billing:manage, idempotent).")

var BillingChargeStatusParams = pydantic.Extend(ProfileParams, "BillingChargeStatusParams", ParamsConfig,
	pydantic.Def("charge_id", pydantic.Nullable(pydantic.Str), nil),
)

var BillingChargeStatusResult = pydantic.Extend(BillingEnvelope, "BillingChargeStatusResult", ClosedConfig,
	pydantic.Def("status", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("amount_usd", pydantic.Nullable(pydantic.Union(pydantic.Str, pydantic.Float)), nil),
	pydantic.Def("settled_at", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("reason", pydantic.Nullable(pydantic.Str), nil),
)

var _ = Method("billing.charge_status", BillingChargeStatusParams, BillingChargeStatusResult, "Poll one charge by id.")

var BillingAutoReloadParams = pydantic.Extend(ProfileParams, "BillingAutoReloadParams", ParamsConfig,
	pydantic.Def("enabled", pydantic.Bool, false),
	pydantic.Def("threshold", pydantic.Nullable(pydantic.Union(pydantic.Float, pydantic.Str)), nil),
	pydantic.Def("top_up_amount", pydantic.Nullable(pydantic.Union(pydantic.Float, pydantic.Str)), nil),
)

var BillingMutationResult = pydantic.Extend(BillingEnvelope, "BillingMutationResult", ClosedConfig)

var _ = Method("billing.auto_reload", BillingAutoReloadParams, BillingMutationResult, "Enable/disable auto top-up with its threshold and reload amount (billing:manage).")

var BillingKeepGoingOnCreditsParams = pydantic.Extend(ProfileParams, "BillingKeepGoingOnCreditsParams", ParamsConfig,
	pydantic.Def("enabled", pydantic.Bool, false),
)

var _ = Method("billing.keep_going_on_credits", BillingKeepGoingOnCreditsParams, BillingMutationResult, "Window plans: turn 'Keep going on credits' on/off (billing:manage).")

var BillingStepUpParams = pydantic.Extend(ProfileParams, "BillingStepUpParams", ParamsConfig,
	pydantic.Def("session_id", pydantic.Nullable(pydantic.Str), nil),
)

var BillingStepUpResult = pydantic.Extend(BillingEnvelope, "BillingStepUpResult", ClosedConfig,
	pydantic.Def("granted", pydantic.Nullable(pydantic.Bool), nil),
)

var _ = Method("billing.step_up", BillingStepUpParams, BillingStepUpResult, "Run the billing:manage device flow; the URL/code arrive via billing.step_up.verification.")

var ActiveSubagent = pydantic.NewModel("ActiveSubagent", OpenConfig,
	pydantic.Req("subagent_id", pydantic.Str),
	pydantic.Def("parent_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("depth", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("goal", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("delegation_id", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("model", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("started_at", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("status", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("tool_count", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("owner_agent_session_id", pydantic.Nullable(pydantic.Str), nil),
)

var DelegationStatusResult = pydantic.NewModel("DelegationStatusResult", ClosedConfig,
	pydantic.Req("active", pydantic.List(ActiveSubagent)),
	pydantic.Req("paused", pydantic.Bool),
	pydantic.Req("max_spawn_depth", pydantic.Int),
	pydantic.Req("max_concurrent_children", pydantic.Int),
)

var _ = Method("delegation.status", ProfileParams, DelegationStatusResult, "Running subagent tree plus the spawn pause flag and limits.")

var DelegationPauseParams = pydantic.Extend(ProfileParams, "DelegationPauseParams", ParamsConfig,
	pydantic.Def("paused", pydantic.Bool, true),
)

var DelegationPauseResult = pydantic.NewModel("DelegationPauseResult", ClosedConfig,
	pydantic.Req("paused", pydantic.Bool),
)

var _ = Method("delegation.pause", DelegationPauseParams, DelegationPauseResult, "Block/unblock NEW spawns globally (active children keep running); returns the new state.")

var SubagentSteerParams = pydantic.Extend(SessionParams, "SubagentSteerParams", ParamsConfig,
	pydantic.Req("subagent_id", pydantic.Str),
	pydantic.Req("text", pydantic.Str),
)

var SteerStatus = pydantic.Enum("SteerStatus", "queued", "queued", "rejected", "rejected")

var SubagentSteerResult = pydantic.NewModel("SubagentSteerResult", ClosedConfig,
	pydantic.Req("status", SteerStatus),
	pydantic.Req("subagent_id", pydantic.Str),
	pydantic.Req("text", pydantic.Str),
)

var _ = Method("subagent.steer", SubagentSteerParams, SubagentSteerResult, "Queue steering text into a live delegated child owned by this session.")

var HandoffRequestParams = pydantic.Extend(SessionParams, "HandoffRequestParams", ParamsConfig,
	pydantic.Req("platform", pydantic.Str),
)

var HandoffRequestResult = pydantic.NewModel("HandoffRequestResult", ClosedConfig,
	pydantic.Req("queued", pydantic.Bool),
	pydantic.Req("session_key", pydantic.Str),
	pydantic.Req("platform", pydantic.Str),
	pydantic.Req("home_name", pydantic.Str),
)

var _ = Method("handoff.request", HandoffRequestParams, HandoffRequestResult, "Queue a handoff to a messaging platform's home channel; the gateway watcher claims it.")

var HandoffStateResult = pydantic.NewModel("HandoffStateResult", ClosedConfig,
	pydantic.Req("state", pydantic.Str),
	pydantic.Req("platform", pydantic.Str),
	pydantic.Req("error", pydantic.Str),
)

var _ = Method("handoff.state", SessionParams, HandoffStateResult, "Poll the handoff row for this session.")

var HandoffFailParams = pydantic.Extend(SessionParams, "HandoffFailParams", ParamsConfig,
	pydantic.Def("error", pydantic.Nullable(pydantic.Str), nil),
)

var HandoffFailResult = pydantic.NewModel("HandoffFailResult", ClosedConfig,
	pydantic.Req("failed", pydantic.Bool),
	pydantic.Req("state", pydantic.Str),
)

var _ = Method("handoff.fail", HandoffFailParams, HandoffFailResult, "Fail a not-yet-claimed handoff (client poll timeout); CAS against the watcher.")

var ReactionAuthor = pydantic.Enum("ReactionAuthor", "user", "user", "agent", "agent")

var MessageReactParams = pydantic.Extend(SessionParams, "MessageReactParams", ParamsConfig,
	pydantic.Def("row_id", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("newest_role", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("emoji", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("author", pydantic.Nullable(ReactionAuthor), nil),
)

var MessageReactResult = pydantic.NewModel("MessageReactResult", ClosedConfig,
	pydantic.Req("row_id", pydantic.Int),
	pydantic.Req("reactions", pydantic.List(MessageReaction)),
)

var _ = Method("message.react", MessageReactParams, MessageReactResult, "Set/clear one author's emoji reaction on a message; returns the row's full reaction list.")

var PetCancelParams = pydantic.Extend(ProfileParams, "PetCancelParams", ParamsConfig,
	pydantic.Def("token", pydantic.Nullable(pydantic.Str), nil),
)

var PetCancelResult = pydantic.NewModel("PetCancelResult", ClosedConfig,
	pydantic.Req("ok", pydantic.Bool),
)

var _ = Method("pet.cancel", PetCancelParams, PetCancelResult, "Stop an in-flight pet generate/hatch by token (idempotent).")

var PetGenProvider = pydantic.NewModel("PetGenProvider", ClosedConfig,
	pydantic.Req("name", pydantic.Str),
	pydantic.Req("label", pydantic.Str),
	pydantic.Req("default", pydantic.Bool),
)

var PetGenerateStatusResult = pydantic.NewModel("PetGenerateStatusResult", ClosedConfig,
	pydantic.Req("available", pydantic.Bool),
	pydantic.Req("providers", pydantic.List(PetGenProvider)),
)

var _ = Method("pet.generate.status", ProfileParams, PetGenerateStatusResult, "Whether pet generation is possible (a reference-capable image backend) and which providers.")

var PetGenerateParams = pydantic.Extend(ProfileParams, "PetGenerateParams", ParamsConfig,
	pydantic.Def("prompt", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("referenceImage", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("count", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("style", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("provider", pydantic.Nullable(pydantic.Str), nil),
)

var PetDraft = pydantic.NewModel("PetDraft", ClosedConfig,
	pydantic.Req("index", pydantic.Int),
	pydantic.Req("dataUri", pydantic.Str),
)

var PetGenerateResult = pydantic.NewModel("PetGenerateResult", ClosedConfig,
	pydantic.Req("ok", pydantic.Bool),
	pydantic.Req("token", pydantic.Str),
	pydantic.Req("drafts", pydantic.List(PetDraft)),
)

var _ = Method("pet.generate", PetGenerateParams, PetGenerateResult, "Candidate base looks for a new pet (draft step); drafts also stream via pet.generate.progress.")

var PetHatchParams = pydantic.Extend(ProfileParams, "PetHatchParams", ParamsConfig,
	pydantic.Req("token", pydantic.Str),
	pydantic.Req("name", pydantic.Str),
	pydantic.Def("cancelToken", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("index", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("description", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("prompt", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("style", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("provider", pydantic.Nullable(pydantic.Str), nil),
)

var PetSpritePayload = pydantic.NewModel("PetSpritePayload", ClosedConfig,
	pydantic.Def("slug", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("displayName", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("mime", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("spritesheetBase64", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("spritesheetRevision", pydantic.Nullable(pydantic.Str), nil),
	pydantic.Def("frameW", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("frameH", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("framesPerState", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("framesByState", pydantic.Nullable(pydantic.Dict(pydantic.Int)), nil),
	pydantic.Def("framesByRow", pydantic.Nullable(pydantic.Dict(pydantic.Int)), nil),
	pydantic.Def("loopMs", pydantic.Nullable(pydantic.Int), nil),
	pydantic.Def("scale", pydantic.Nullable(pydantic.Float), nil),
	pydantic.Def("stateRows", pydantic.Nullable(pydantic.List(pydantic.Str)), nil),
)

var PetHatchResult = pydantic.NewModel("PetHatchResult", ClosedConfig,
	pydantic.Req("ok", pydantic.Bool),
	pydantic.Req("slug", pydantic.Str),
	pydantic.Req("displayName", pydantic.Str),
	pydantic.Fac("warnings", pydantic.List(pydantic.Any), pydantic.NewList),
	pydantic.Req("pet", PetSpritePayload),
)

var _ = Method("pet.hatch", PetHatchParams, PetHatchResult, "Turn a base draft into a full spritesheet pet; progress streams via pet.hatch.progress.")

var ProjectFactsParams = pydantic.Extend(ProfileParams, "ProjectFactsParams", ParamsConfig,
	pydantic.Def("cwd", pydantic.Nullable(pydantic.Str), nil),
)

var ProjectFacts = pydantic.NewModel("ProjectFacts", ClosedConfig,
	pydantic.Req("root", pydantic.Str),
	pydantic.Req("manifests", pydantic.List(pydantic.Str)),
	pydantic.Req("packageManagers", pydantic.List(pydantic.Str)),
	pydantic.Req("verifyCommands", pydantic.List(pydantic.Str)),
	pydantic.Req("contextFiles", pydantic.List(pydantic.Str)),
)

var ProjectFactsResult = pydantic.NewModel("ProjectFactsResult", ClosedConfig,
	pydantic.Def("facts", pydantic.Nullable(ProjectFacts), nil),
)

var _ = Method("project.facts", ProjectFactsParams, ProjectFactsResult, "Structured project facts for a cwd so UIs don't re-sniff the workspace.")
