package observability

import "github.com/nickheyer/icebath/pkg/tools"

var (
	legacyProviderFamilies = Set("aggregator", "custom", "direct", "local", "unknown")
	legacyModelLocalities  = Set("local", "remote", "unknown")
	legacyModelOutcomes    = Set("cancelled", "failed", "success")
	legacyModelFamilies    = Set("claude", "deepseek", "gemini", "gemma", "glm", "gpt", "grok", "kimi", "llama",
		"minimax", "mimo", "mistral", "nemotron", "nova", "o1", "o3", "o4", "qwen", "step", "trinity", "unknown")
)

// dims is one metric's closed field to vocabulary contract
type dims map[string]allowedValues

// Merges dimension contracts left to right, later keys winning
func mergeDims(parts ...dims) dims {
	out := dims{}
	for _, p := range parts {
		for k, v := range p {
			out[k] = v
		}
	}
	return out
}

var counterDimensionValues = map[string]dims{
	ClientActiveMetric: {},
	LegacyModelCallMetric: {
		"call_role": Set("primary"), "locality": legacyModelLocalities,
		"model_family": legacyModelFamilies, "outcome": legacyModelOutcomes,
		"provider_family": legacyProviderFamilies,
	},
	TaskStartedMetric: {
		"entrypoint": TaskEntrypoints, "execution_surface": ExecutionSurfaces, "platform": GatewayPlatforms,
	},
	TaskFinishedMetric: {
		"duration_bucket": DurationBuckets, "end_reason": TaskEndReasons,
		"entrypoint": TaskEntrypoints, "execution_surface": ExecutionSurfaces,
		"failure_class":           TaskFailureClasses,
		"model_call_count_bucket": CountBuckets, "outcome": TaskOutcomes,
		"platform":           GatewayPlatforms,
		"retry_count_bucket": CountBuckets, "termination": TaskTerminations,
		"tool_call_count_bucket": CountBuckets,
	},
	ModelRouteMetric: {
		"call_role": ModelCallRoles, "error_class": ModelErrorClasses, "outcome": ModelOutcomes,
		"ttft_bucket": TTFTBuckets,
	},
	ToolCallMetric: {
		"approval_outcome": ToolApprovalOutcomes, "latency_bucket": ToolLatencyBuckets,
		"outcome": ToolOutcomes, "retry_count_bucket": ToolRetryBuckets, "tool_category": ToolCategories,
	},
	ToolLatencyMetric: {
		"latency_bucket": ToolLatencyBuckets, "retry_count_bucket": ToolRetryBuckets, "tool_category": ToolCategories,
	},
	TaskDurationMetric: {
		"duration_bucket": DurationBuckets, "execution_surface": ExecutionSurfaces,
		"outcome": TaskOutcomes, "retry_count_bucket": CountBuckets,
	},
	ToolUsageMetric: {"error_class": ToolErrorClasses, "outcome": ToolOutcomes, "tool_name": ToolNames},
	ToolApprovalMetric: {
		"attribution": ToolApprovalAttributions, "outcome": ToolApprovalOutcomes.Minus("not_required"),
	},
	SkillLifecycleMetric: {"action": SkillLifecycleActions, "provenance": SkillProvenances},
	SkillLoadMetric: {
		"post_patch_state": SkillPostPatchStates, "provenance": SkillProvenances,
		"reuse_state": SkillReuseStates, "skill_name": SkillNames, "use_count_bucket": CountBuckets,
	},
	InstallSnapshotMetric: mergeDims(dims{
		"cron_job_count_bucket": SizeBuckets, "display_language": DisplayLanguages,
		"install_age_bucket": InstallAgeBuckets, "mcp_server_count_bucket": SizeBuckets,
		"memory_provider": MemoryProviders, "messaging_platform_count_bucket": SizeBuckets,
		"plugin_count_bucket": SizeBuckets, "profile_count_bucket": SizeBuckets,
		"skill_count_bucket": SizeBuckets, "terminal_backend": TerminalBackends,
	}, installV4SnapshotDimensions, PreparedCompactionSnapshotDimensions),
	SessionMetric: {
		"active_duration_bucket": SessionDurationBuckets, "entrypoint": TaskEntrypoints,
		"execution_surface": ExecutionSurfaces, "failed_turn_count_bucket": CountBuckets,
		"last_outcome": TaskOutcomes, "platform": GatewayPlatforms, "turn_count_bucket": SizeBuckets,
		"message_count_bucket": LongSizeBuckets, "model_call_count_bucket": LongSizeBuckets,
		"tool_call_count_bucket": LongSizeBuckets,
	},
	MilestoneMetric:      {"install_age_bucket": InstallAgeBuckets, "milestone": Milestones},
	SetupCompletedMetric: {"surface": SetupSurfaces},
	ModelTokensMetric:    {"aux_task": AuxTasks, "call_role": ModelCallRoles, "token_type": TokenTypes},
	CompressionMetric: {
		"context_fill_bucket": ContextFillBuckets, "failure_class": CompressionFailureClasses,
		"outcome": CompressionOutcomes, "trigger": CompressionTriggers,
	},
	ModelSwitchMetric:  {"execution_surface": ExecutionSurfaces},
	FallbackMetric:     {"error_class": ModelErrorClasses},
	SlashCommandMetric: {"command": SlashCommands, "execution_surface": ExecutionSurfaces},
	ExtensionInstallMetric: {
		"kind": ExtensionKinds, "name": ExtensionNames, "outcome": ExtensionOutcomes, "source": ExtensionSources,
		"failure_class": ExtensionFailureClasses, "registry": ExtensionRegistries,
	},
	MemoryOpMetric: {
		"failure_class": MemoryOpFailureClasses, "op": MemoryOps, "origin": MemoryOpOrigins,
		"outcome": MemoryOpOutcomes, "provider": MemoryProviders,
	},
	CuratorRunMetric: {
		"archived_bucket": CountBuckets, "created_bucket": CountBuckets, "merged_bucket": CountBuckets,
		"outcome": CuratorOutcomes, "patched_bucket": CountBuckets, "trigger": CuratorTriggers,
	},
	DelegationRunMetric: {
		"depth": DelegationDepths, "mode": DelegationModes, "outcome": DelegationOutcomes,
		"subagent_count_bucket": CountBuckets,
	},
	ExecutionBackendMetric: {
		"backend": ExecutionBackends, "error_class": ToolErrorClasses, "kind": ExecutionKinds,
		"outcome": ExecutionOutcomes,
	},
	PlatformHealthMetric: {
		"error_class": PlatformErrorClasses, "event": PlatformHealthEvents, "platform": AdapterPlatforms,
	},
	PlatformDeliveryMetric: {
		"failure_class": DeliveryFailureClasses, "outcome": DeliveryOutcomes, "platform": AdapterPlatforms,
	},
	ReplyLatencyMetric: {"first_response_bucket": ReplyLatencyBuckets, "platform": AdapterPlatforms},
	CronRunMetric: {
		"delivery_kind": CronDeliveryKinds, "duration_bucket": DurationBuckets, "outcome": CronRunOutcomes,
	},
	ModelToolQualityMetric: {"call_role": ModelCallRoles, "issue": ToolCallIssues},
	ModelFrictionMetric:    {"signal": FrictionSignals},
	ContextPeakMetric: {
		"limit_hit": LimitHitValues, "peak_fill_bucket": ContextFillBuckets, "window_bucket": ContextWindowBuckets,
	},
	StartupLatencyMetric: {"latency_bucket": StartupLatencyBuckets, "surface": StartupSurfaces},
	UpdateRunMetric: {
		"apply_mode": UpdateApplyModes, "duration_bucket": UpdateDurationBuckets,
		"failed_stage": UpdateFailedStages, "from_version_age_bucket": VersionAgeBuckets,
		"kind": UpdateKinds, "outcome": UpdateRunOutcomes, "failure_class": UpdateFailureClasses,
	},
	UpdateStageMetric: {
		"duration_bucket": UpdateDurationBuckets, "outcome": UpdateStageOutcomes, "stage": UpdateStages,
	},
	ProcessExitMetric: {"crash_class": CrashClasses, "exit_kind": ProcessExitKinds, "process_kind": ProcessKinds},
	FileEditMetric: {
		"match_strategy": FileEditStrategies, "mode": FileEditModes, "outcome": FileEditOutcomes,
		"tool": FileEditTools,
	},
	LoopGuardMetric: {"detector": LoopGuardDetectors, "signal": LoopGuardSignals},
	ToolRecoveryMetric: {
		"next_outcome": ToolRecoveryOutcomes, "next_tool": ToolRecoveryNextTools, "tool": ToolNames,
	},
	TerminalOutcomeMetric: {
		"backend": TerminalBackends, "command_kind": TerminalCommandKinds, "outcome": TerminalOutcomes,
	},
	ModelReplyIssueMetric: {"issue": ModelReplyIssues},
	TaskCostMetric: {
		"api_calls_bucket": TurnActivityBuckets, "outcome": TaskCostOutcomes,
		"tokens_bucket": TurnTokenBuckets, "tool_calls_bucket": TurnActivityBuckets,
	},
	WastedTokensMetric: {"reason": WasteReasons, "tokens_bucket": TurnTokenBuckets},
	ToolOutputTruncationMetric: {
		"original_size_bucket": ToolOutputSizeBuckets, "tool": ToolNames, "truncated": YesNo,
	},
	ToolOverheadMetric: {
		"enabled_tool_count_bucket": SizeBuckets, "execution_surface": ExecutionSurfaces,
		"tool_schema_tokens_bucket": ToolSchemaTokenBuckets,
	},
	ToolEnabledUnusedMetric: {"toolset": ToolsetNames, "used": YesNo},
	CacheBreakMetric:        {"cause": CacheBreakCauses},
	EngagementDayMetric: {
		"active_minutes_bucket": ActiveMinutesBuckets, "active_profile_count_bucket": SizeBuckets,
		"surfaces_used_count": SurfacesUsedBuckets, BusyPeakField: CountBuckets,
	},
	EngagementSurfaceMetric: {"active_minutes_bucket": ActiveMinutesBuckets, "surface": EngagementSurfaces},
	ModelSwitchAfterMetric:  {"turns_before_switch_bucket": TurnsBeforeSwitchBuckets},
	DesktopFeatureUseMetric: {"area": DesktopFeatureAreas},
	DesktopFrictionMetric:   {"detail": DesktopFrictionDetailValues, "kind": DesktopFrictionKinds},
	DesktopOnboardingMetric: {"event": DesktopOnboardingEvents, "step": DesktopOnboardingSteps},
	DesktopModeUseMetric: {
		"active_minutes_bucket": DesktopActiveMinutesBuckets, "bot_count_bucket": SizeBuckets,
		"messages_sent_bucket": SizeBuckets, "mode": DesktopModes,
	},
	DesktopActionUseMetric: {"action": DesktopActionIDs, "count_bucket": SizeBuckets, "via": DesktopActionVias},
	DesktopDislikeMetric: {
		"direction": DesktopDislikeDirections, "signal": DesktopDislikeSignals, "target": DesktopDislikeTargetValues,
	},
	ToolUnavailableMetric: {"tool_name": setOfKeys(tools.BuiltinToolNames).Union(RetiredToolNames)},
	ProviderSetupMetric: {
		"event": ProviderSetupEvents, "failure_class": ProviderSetupFailureClasses, "surface": ProviderSetupSurfaces,
	},
	FeatureAdoptionMetric: {"days_since_install_bucket": DaysSinceInstallBuckets, "feature": Features},
	FeatureDisabledMetric: {
		"event": FeatureDisabledEvents, "kind": FeatureDisabledKinds, "surface": FeatureDisabledSurfaces,
	},
	InstallRunMetric: {
		"duration_bucket": UpdateDurationBuckets, "failed_stage": InstallRunFailedStages,
		"failure_class": InstallRunFailureClasses, "installer": InstallRunInstallers, "outcome": InstallRunOutcomes,
	},
	ToolSearchMetric: {"hits_bucket": CountBuckets, "outcome": ToolSearchOutcomes, "scope": ToolSearchScopes},
	ToolBatchMetric:  {"connector_share": ConnectorShares, "size_bucket": CountBuckets},
	ConnectorChainMetric: {
		"described_first": YesNo, "outcome": ToolOutcomes, "searches_before_bucket": CountBuckets,
	},
	PreparedCompactionMetric: mergeDims(PreparedCompactionDimensions, dims{"failure_class": CompressionFailureClasses}),
	BusyArrivalMetric: {
		"disposition": BusyArrivalDispositions, "platform": AdapterPlatforms, "wait_bucket": BusyWaitBuckets,
	},
}

var modelRouteMaxLengths = map[string]int{"model": ModelIdentifierMaxLength, "provider": ProviderIdentifierMaxLength}

// Both sides of a switch or fallback carry a provider identifier
var switchProviderLengths = map[string]int{"from_provider": ProviderIdentifierMaxLength, "to_provider": ProviderIdentifierMaxLength}

var identifierFields = map[string]map[string]int{
	ModelRouteMetric:       modelRouteMaxLengths,
	ModelTokensMetric:      modelRouteMaxLengths,
	SetupCompletedMetric:   {"provider": ProviderIdentifierMaxLength},
	ModelSwitchMetric:      switchProviderLengths,
	FallbackMetric:         switchProviderLengths,
	InstallSnapshotMetric:  {"main_provider": ProviderIdentifierMaxLength},
	ModelToolQualityMetric: modelRouteMaxLengths,
	ModelFrictionMetric:    modelRouteMaxLengths,
	ContextPeakMetric:      modelRouteMaxLengths,
	LoopGuardMetric:        modelRouteMaxLengths,
	ToolRecoveryMetric:     modelRouteMaxLengths,
	ModelReplyIssueMetric:  modelRouteMaxLengths,
	TaskCostMetric:         modelRouteMaxLengths,
	WastedTokensMetric:     modelRouteMaxLengths,
	CacheBreakMetric:       modelRouteMaxLengths,
	EngagementDayMetric: {
		"primary_model": ModelIdentifierMaxLength, "primary_provider": ProviderIdentifierMaxLength,
	},
	ModelSwitchAfterMetric: modelRouteMaxLengths,
	DesktopDislikeMetric:   {"setting": DesktopSettingKeyMaxLength},
	ToolUnavailableMetric:  modelRouteMaxLengths,
	ProviderSetupMetric:    {"provider": ProviderIdentifierMaxLength},
	FeatureDisabledMetric:  {"name": FeatureDisabledNameMaxLength},
	ToolBatchMetric:        modelRouteMaxLengths,
}

// SessionVolumeDimensions are the session row's long scale volume buckets
var SessionVolumeDimensions = map[string]ValueSet{
	"message_count_bucket": LongSizeBuckets, "model_call_count_bucket": LongSizeBuckets,
	"tool_call_count_bucket": LongSizeBuckets,
}

var (
	taskFinishedV2Only = Set("duration_bucket", "model_call_count_bucket", "retry_count_bucket", "tool_call_count_bucket")
	toolCallV2Only     = Set("latency_bucket", "retry_count_bucket")
	metricFields       = buildMetricFields()
	legacyMetricFields = buildLegacyMetricFields()
	// CounterMetrics are every current counter, the legacy model call count excluded
	CounterMetrics      = keysOf(metricFields).Minus(LegacyModelCallMetric)
	SumMetrics          = Set(ModelTokensMetric)
	skillMarkMetrics    = map[string]string{SkillLifecycleMark: SkillLifecycleMetric, SkillLoadMark: SkillLoadMetric}
	decisionMarkMetrics = map[string]string{
		SessionMark: SessionMetric, SetupCompletedMark: SetupCompletedMetric,
		CompressionMark: CompressionMetric, ModelSwitchMark: ModelSwitchMetric,
		FallbackMark: FallbackMetric, SlashCommandMark: SlashCommandMetric,
		ExtensionInstallMark: ExtensionInstallMetric,
		MemoryOpMark:         MemoryOpMetric, CuratorRunMark: CuratorRunMetric,
		DelegationRunMark: DelegationRunMetric, ExecutionBackendMark: ExecutionBackendMetric,
		PlatformHealthMark: PlatformHealthMetric, PlatformDeliveryMark: PlatformDeliveryMetric,
		ReplyLatencyMark: ReplyLatencyMetric, CronRunMark: CronRunMetric,
		ModelToolQualityMark: ModelToolQualityMetric, ModelFrictionMark: ModelFrictionMetric,
		ContextPeakMark:    ContextPeakMetric,
		StartupLatencyMark: StartupLatencyMetric,
		UpdateRunMark:      UpdateRunMetric, UpdateStageMark: UpdateStageMetric,
		ProcessExitMark: ProcessExitMetric,
		FileEditMark:    FileEditMetric, LoopGuardMark: LoopGuardMetric,
		ToolRecoveryMark: ToolRecoveryMetric, TerminalOutcomeMark: TerminalOutcomeMetric,
		ModelReplyIssueMark: ModelReplyIssueMetric,
		TaskCostMark:        TaskCostMetric, WastedTokensMark: WastedTokensMetric,
		ToolOutputTruncationMark: ToolOutputTruncationMetric, ToolOverheadMark: ToolOverheadMetric,
		ToolEnabledUnusedMark: ToolEnabledUnusedMetric, CacheBreakMark: CacheBreakMetric,
		ModelSwitchAfterMark:  ModelSwitchAfterMetric,
		DesktopFeatureUseMark: DesktopFeatureUseMetric, DesktopFrictionMark: DesktopFrictionMetric,
		DesktopOnboardingMark: DesktopOnboardingMetric, DesktopModeUseMark: DesktopModeUseMetric,
		DesktopActionUseMark: DesktopActionUseMetric, DesktopDislikeMark: DesktopDislikeMetric,
		ToolUnavailableMark: ToolUnavailableMetric, ProviderSetupMark: ProviderSetupMetric,
		FeatureDisabledMark: FeatureDisabledMetric,
		InstallRunMark:      InstallRunMetric,
		ToolSearchMark:      ToolSearchMetric, ToolBatchMark: ToolBatchMetric,
		ConnectorChainMark: ConnectorChainMetric, PreparedCompactionMark: PreparedCompactionMetric,
		BusyArrivalMetric: BusyArrivalMetric,
	}
)

// Current field set of every metric, contract fields plus identifier fields, minus the v2 only task and tool fields
func buildMetricFields() map[string]ValueSet {
	out := map[string]ValueSet{}
	for name, contract := range counterDimensionValues {
		fields := ValueSet{}
		for f := range contract {
			fields[f] = struct{}{}
		}
		for f := range identifierFields[name] {
			fields[f] = struct{}{}
		}
		out[name] = fields
	}
	out[TaskFinishedMetric] = out[TaskFinishedMetric].Difference(taskFinishedV2Only)
	out[ToolCallMetric] = out[ToolCallMetric].Difference(toolCallV2Only)
	return out
}

// Older field sets each metric still accepts so stored rows keep packaging
func buildLegacyMetricFields() map[string][]ValueSet {
	m := metricFields
	v4Snapshot := keysOf(installV4SnapshotDimensions)
	prepared := keysOf(PreparedCompactionSnapshotDimensions)
	return map[string][]ValueSet{
		ModelRouteMetric:   {keysOf(modelRouteMaxLengths), m[ModelRouteMetric].Minus("ttft_bucket")},
		TaskStartedMetric:  {m[TaskStartedMetric].Minus("platform")},
		TaskFinishedMetric: {keysOf(counterDimensionValues[TaskFinishedMetric]).Minus("failure_class", "platform")},
		ToolCallMetric:     {keysOf(counterDimensionValues[ToolCallMetric])},
		CompressionMetric:  {m[CompressionMetric].Minus("failure_class")},
		MemoryOpMetric:     {m[MemoryOpMetric].Minus("failure_class")},
		SkillLoadMetric:    {m[SkillLoadMetric].Minus("skill_name")},
		InstallSnapshotMetric: {
			m[InstallSnapshotMetric].Minus("display_language", "install_age_bucket", "main_provider",
				"messaging_platform_count_bucket", "terminal_backend").Difference(v4Snapshot, prepared),
			m[InstallSnapshotMetric].Difference(v4Snapshot, prepared),
			m[InstallSnapshotMetric].Difference(prepared),
		},
		SessionMetric:          {m[SessionMetric].Difference(keysOf(SessionVolumeDimensions))},
		ExtensionInstallMetric: {m[ExtensionInstallMetric].Minus("failure_class", "registry")},
		UpdateRunMetric:        {m[UpdateRunMetric].Minus("failure_class")},
		EngagementDayMetric:    {m[EngagementDayMetric].Minus(BusyPeakField)},
	}
}
