package agent

// Copies the constructor params stored verbatim under the same name
func copyPassthroughParams(a *AIAgent, o AIAgentOptions) {
	a.model, a.modelNone, a.maxIterations, a.saveTrajectories, a.verboseLogging = o.Model, o.ModelNone, *o.MaxIterations, o.SaveTrajectories, o.VerboseLogging
	a.quietMode, a.toolProgressMode, a.ephemeralSystemPrompt, a.platform = o.QuietMode, *o.ToolProgressMode, o.EphemeralSystemPrompt, o.Platform
	a.skipContextFiles, a.loadSoulIdentity, a.passSessionID, a.logPrefixChars = o.SkipContextFiles, o.LoadSoulIdentity, o.PassSessionID, *o.LogPrefixChars
	a.providersAllowed, a.providersIgnored, a.providersOrder, a.providerSort = o.ProvidersAllowed, o.ProvidersIgnored, o.ProvidersOrder, o.ProviderSort
	a.providerRequireParameters, a.providerDataCollection, a.openrouterMinCodingScore = o.ProviderRequireParameters, o.ProviderDataCollection, o.OpenRouterMinCodingScore
	a.enabledToolsets, a.disabledToolsets = o.EnabledToolsets, o.DisabledToolsets
	a.maxTokens, a.reasoningConfig, a.serviceTier, a.sideAgent = o.MaxTokens, o.ReasoningConfig, o.ServiceTier, o.SideAgent
}

// Copies the gateway identity params, the gateway session key being the stable per chat key
func copyGatewayIdentityParams(a *AIAgent, o AIAgentOptions) {
	a.userID, a.userIDAlt, a.userName = o.UserID, o.UserIDAlt, o.UserName
	a.chatID, a.chatName, a.chatType = o.ChatID, o.ChatName, o.ChatType
	a.threadID, a.gatewaySessionKey = o.ThreadID, o.GatewaySessionKey
}

// Copies the platform callbacks stored under their parameter names
func copyCallbackParams(a *AIAgent, o AIAgentOptions) {
	a.toolProgressCallback, a.toolStartCallback, a.toolCompleteCallback = o.ToolProgressCallback, o.ToolStartCallback, o.ToolCompleteCallback
	a.toolResultMetadataCallback = o.ToolResultMetadataCallback
	a.thinkingCallback, a.reasoningCallback, a.clarifyCallback = o.ThinkingCallback, o.ReasoningCallback, o.ClarifyCallback
	a.readTerminalCallback, a.readPreviewCallback, a.drivePreviewCallback = o.ReadTerminalCallback, o.ReadPreviewCallback, o.DrivePreviewCallback
	a.readWindowBelowCallback, a.connectionCallback, a.tourCallback, a.themeCallback = o.ReadWindowBelowCallback, o.ConnectionCallback, o.TourCallback, o.ThemeCallback
	a.setupBeatCallback, a.stepCallback, a.streamDeltaCallback = o.SetupBeatCallback, o.StepCallback, o.StreamDeltaCallback
	a.interimAssistantCallback = o.InterimAssistantCallback
	a.statusCallback, a.noticeCallback, a.noticeClearCallback = o.StatusCallback, o.NoticeCallback, o.NoticeClearCallback
	a.eventCallback, a.reactionCallback, a.toolGenCallback = o.EventCallback, o.ReactionCallback, o.ToolGenCallback
}
