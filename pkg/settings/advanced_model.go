package settings

import "time"

// How long Fast mode stays on after it switches on by itself
var AgentFastAutoSeconds = Register(&Duration{Meta: Meta{Key: "agent.fast_auto_seconds", Label: "Fast mode window", Short: "How long Fast mode stays on after it switches on by itself.", Group: GroupModel, Order: 1000, Advanced: true}, Default: time.Minute})

// Reasoning effort for particular models, by model name
var AgentReasoningOverrides = Register(&Map{Meta: Meta{Key: "agent.reasoning_overrides", Label: "Reasoning by model", Short: "Reasoning effort for particular models, by model name.", Help: "Models not listed use Reasoning effort.", Group: GroupModel, Order: 1010, Advanced: true}})

// How wordy answers should be, for models that support it
var AgentTextVerbosity = Register(&Enum{Meta: Meta{Key: "agent.text_verbosity", Label: "Answer length", Short: "How wordy answers should be, for models that support it.", Group: GroupModel, Order: 1020, Advanced: true}, Default: "", Values: []Choice{{Value: "low", Label: "Low", Meaning: "Short answers."}, {Value: "medium", Label: "Medium", Meaning: "Balanced answers."}, {Value: "high", Label: "High", Meaning: "Detailed answers."}}, Null: "the model's default"})

// API key for smart command approval
var AuxiliaryApprovalApiKey = Register(&String{Meta: Meta{Key: "auxiliary.approval.api_key", Label: "Approval key", Short: "API key for smart command approval.", Help: "Empty uses your normal login.", Group: GroupModel, Order: 1030, Advanced: true}, Masked: true, Default: ""})

// Custom endpoint address for smart command approval
var AuxiliaryApprovalBaseUrl = Register(&String{Meta: Meta{Key: "auxiliary.approval.base_url", Label: "Approval URL", Short: "Custom endpoint address for smart command approval.", Group: GroupModel, Order: 1040, Advanced: true}, Default: ""})

// Extra request fields for smart command approval
var AuxiliaryApprovalExtraBody = Register(&Map{Meta: Meta{Key: "auxiliary.approval.extra_body", Label: "Approval fields", Short: "Extra request fields for smart command approval.", Group: GroupModel, Order: 1050, Advanced: true}})

// Model for smart command approval
var AuxiliaryApprovalModel = Register(&String{Meta: Meta{Key: "auxiliary.approval.model", Label: "Approval model", Short: "Model for smart command approval.", Help: "Empty picks one for you.", Group: GroupModel, Order: 1060, Advanced: true}, Default: ""})

// Provider for smart command approval
var AuxiliaryApprovalProvider = Register(&String{Meta: Meta{Key: "auxiliary.approval.provider", Label: "Approval provider", Short: "Provider for smart command approval.", Help: "Auto picks one for you.", Group: GroupModel, Order: 1070, Advanced: true}, Default: "auto"})

// Thinking effort for smart command approval
var AuxiliaryApprovalReasoningEffort = Register(&Enum{Meta: Meta{Key: "auxiliary.approval.reasoning_effort", Label: "Approval thinking", Short: "Thinking effort for smart command approval.", Group: GroupModel, Order: 1080, Advanced: true}, Values: reasoningChoices, Null: "the model's default"})

// Time limit for smart command approval
var AuxiliaryApprovalTimeout = Register(&Duration{Meta: Meta{Key: "auxiliary.approval.timeout", Label: "Approval timeout", Short: "Time limit for smart command approval.", Group: GroupModel, Order: 1090, Advanced: true}, Default: 30 * time.Second})

// API key for background reviews
var AuxiliaryBackgroundReviewApiKey = Register(&String{Meta: Meta{Key: "auxiliary.background_review.api_key", Label: "Auto review key", Short: "API key for background reviews.", Help: "Empty uses your normal login.", Group: GroupModel, Order: 1100, Advanced: true}, Masked: true, Default: ""})

// Custom endpoint address for background reviews
var AuxiliaryBackgroundReviewBaseUrl = Register(&String{Meta: Meta{Key: "auxiliary.background_review.base_url", Label: "Auto review URL", Short: "Custom endpoint address for background reviews.", Group: GroupModel, Order: 1110, Advanced: true}, Default: ""})

// Let the model review sessions in the background automatically
var AuxiliaryBackgroundReviewEnabled = Register(&Bool{Meta: Meta{Key: "auxiliary.background_review.enabled", Label: "Auto review", Short: "Let the model review sessions in the background automatically.", Group: GroupModel, Order: 1120, Advanced: true}, Default: true})

// Extra request fields for background reviews
var AuxiliaryBackgroundReviewExtraBody = Register(&Map{Meta: Meta{Key: "auxiliary.background_review.extra_body", Label: "Auto review fields", Short: "Extra request fields for background reviews.", Group: GroupModel, Order: 1130, Advanced: true}})

// Model for background reviews
var AuxiliaryBackgroundReviewModel = Register(&String{Meta: Meta{Key: "auxiliary.background_review.model", Label: "Auto review model", Short: "Model for background reviews.", Help: "Empty picks one for you.", Group: GroupModel, Order: 1140, Advanced: true}, Default: ""})

// Provider for background reviews
var AuxiliaryBackgroundReviewProvider = Register(&String{Meta: Meta{Key: "auxiliary.background_review.provider", Label: "Auto review provider", Short: "Provider for background reviews.", Help: "Auto picks one for you.", Group: GroupModel, Order: 1150, Advanced: true}, Default: "auto"})

// Thinking effort for background reviews
var AuxiliaryBackgroundReviewReasoningEffort = Register(&Enum{Meta: Meta{Key: "auxiliary.background_review.reasoning_effort", Label: "Auto review thinking", Short: "Thinking effort for background reviews.", Group: GroupModel, Order: 1160, Advanced: true}, Values: reasoningChoices, Null: "the model's default"})

// Time limit for background reviews
var AuxiliaryBackgroundReviewTimeout = Register(&Duration{Meta: Meta{Key: "auxiliary.background_review.timeout", Label: "Auto review timeout", Short: "Time limit for background reviews.", Group: GroupModel, Order: 1170, Advanced: true}, Default: 2 * time.Minute})

// API key for conversation summaries
var AuxiliaryCompressionApiKey = Register(&String{Meta: Meta{Key: "auxiliary.compression.api_key", Label: "Compression key", Short: "API key for conversation summaries.", Help: "Empty uses your normal login.", Group: GroupModel, Order: 1180, Advanced: true}, Masked: true, Default: ""})

// Custom endpoint address for conversation summaries
var AuxiliaryCompressionBaseUrl = Register(&String{Meta: Meta{Key: "auxiliary.compression.base_url", Label: "Compression URL", Short: "Custom endpoint address for conversation summaries.", Group: GroupModel, Order: 1190, Advanced: true}, Default: ""})

// Extra request fields for conversation summaries
var AuxiliaryCompressionExtraBody = Register(&Map{Meta: Meta{Key: "auxiliary.compression.extra_body", Label: "Compression fields", Short: "Extra request fields for conversation summaries.", Group: GroupModel, Order: 1200, Advanced: true}})

// Model for conversation summaries
var AuxiliaryCompressionModel = Register(&String{Meta: Meta{Key: "auxiliary.compression.model", Label: "Compression model", Short: "Model for conversation summaries.", Help: "Empty picks one for you.", Group: GroupModel, Order: 1210, Advanced: true}, Default: ""})

// Give up a summary stream that stops sending data after this long
var AuxiliaryCompressionNoProgressTimeout = Register(&Duration{Meta: Meta{Key: "auxiliary.compression.no_progress_timeout", Label: "Stall limit", Short: "Give up a summary stream that stops sending data after this long.", Group: GroupModel, Order: 1220, Advanced: true}, Null: "built-in 60 seconds"})

// Provider for conversation summaries
var AuxiliaryCompressionProvider = Register(&String{Meta: Meta{Key: "auxiliary.compression.provider", Label: "Compression provider", Short: "Provider for conversation summaries.", Help: "Auto picks one for you.", Group: GroupModel, Order: 1230, Advanced: true}, Default: "auto"})

// Thinking effort for conversation summaries
var AuxiliaryCompressionReasoningEffort = Register(&Enum{Meta: Meta{Key: "auxiliary.compression.reasoning_effort", Label: "Compression thinking", Short: "Thinking effort for conversation summaries.", Group: GroupModel, Order: 1240, Advanced: true}, Values: reasoningChoices, Null: "the model's default"})

// Time limit for conversation summaries
var AuxiliaryCompressionTimeout = Register(&Duration{Meta: Meta{Key: "auxiliary.compression.timeout", Label: "Compression timeout", Short: "Time limit for conversation summaries.", Help: "Never less than the built-in minimum.", Group: GroupModel, Order: 1250, Advanced: true}, Default: 2 * time.Minute})

// API key for skill upkeep
var AuxiliaryCuratorApiKey = Register(&String{Meta: Meta{Key: "auxiliary.curator.api_key", Label: "Curator key", Short: "API key for skill upkeep.", Help: "Empty uses your normal login.", Group: GroupModel, Order: 1260, Advanced: true}, Masked: true, Default: ""})

// Custom endpoint address for skill upkeep
var AuxiliaryCuratorBaseUrl = Register(&String{Meta: Meta{Key: "auxiliary.curator.base_url", Label: "Curator URL", Short: "Custom endpoint address for skill upkeep.", Group: GroupModel, Order: 1270, Advanced: true}, Default: ""})

// Extra request fields for skill upkeep
var AuxiliaryCuratorExtraBody = Register(&Map{Meta: Meta{Key: "auxiliary.curator.extra_body", Label: "Curator fields", Short: "Extra request fields for skill upkeep.", Group: GroupModel, Order: 1280, Advanced: true}})

// Model for skill upkeep
var AuxiliaryCuratorModel = Register(&String{Meta: Meta{Key: "auxiliary.curator.model", Label: "Curator model", Short: "Model for skill upkeep.", Help: "Empty picks one for you.", Group: GroupModel, Order: 1290, Advanced: true}, Default: ""})

// Provider for skill upkeep
var AuxiliaryCuratorProvider = Register(&String{Meta: Meta{Key: "auxiliary.curator.provider", Label: "Curator provider", Short: "Provider for skill upkeep.", Help: "Auto picks one for you.", Group: GroupModel, Order: 1300, Advanced: true}, Default: "auto"})

// Thinking effort for skill upkeep
var AuxiliaryCuratorReasoningEffort = Register(&Enum{Meta: Meta{Key: "auxiliary.curator.reasoning_effort", Label: "Curator thinking", Short: "Thinking effort for skill upkeep.", Group: GroupModel, Order: 1310, Advanced: true}, Values: reasoningChoices, Null: "the model's default"})

// Time limit for skill upkeep
var AuxiliaryCuratorTimeout = Register(&Duration{Meta: Meta{Key: "auxiliary.curator.timeout", Label: "Curator timeout", Short: "Time limit for skill upkeep.", Group: GroupModel, Order: 1320, Advanced: true}, Default: 10 * time.Minute})

// Only use free models for the OpenRouter fallback
var AuxiliaryFreeOnly = Register(&Bool{Meta: Meta{Key: "auxiliary.free_only", Label: "Free models only", Short: "Only use free models for the OpenRouter fallback.", Group: GroupModel, Order: 1330, Advanced: true}, Default: false})

// API key for goal checks
var AuxiliaryGoalJudgeApiKey = Register(&String{Meta: Meta{Key: "auxiliary.goal_judge.api_key", Label: "Goal judge key", Short: "API key for goal checks.", Help: "Empty uses your normal login.", Group: GroupModel, Order: 1340, Advanced: true}, Masked: true, Default: ""})

// Custom endpoint address for goal checks
var AuxiliaryGoalJudgeBaseUrl = Register(&String{Meta: Meta{Key: "auxiliary.goal_judge.base_url", Label: "Goal judge URL", Short: "Custom endpoint address for goal checks.", Group: GroupModel, Order: 1350, Advanced: true}, Default: ""})

// Extra request fields for goal checks
var AuxiliaryGoalJudgeExtraBody = Register(&Map{Meta: Meta{Key: "auxiliary.goal_judge.extra_body", Label: "Goal judge fields", Short: "Extra request fields for goal checks.", Group: GroupModel, Order: 1360, Advanced: true}})

// Model for goal checks
var AuxiliaryGoalJudgeModel = Register(&String{Meta: Meta{Key: "auxiliary.goal_judge.model", Label: "Goal judge model", Short: "Model for goal checks.", Help: "Empty picks one for you.", Group: GroupModel, Order: 1370, Advanced: true}, Default: ""})

// Provider for goal checks
var AuxiliaryGoalJudgeProvider = Register(&String{Meta: Meta{Key: "auxiliary.goal_judge.provider", Label: "Goal judge provider", Short: "Provider for goal checks.", Help: "Auto picks one for you.", Group: GroupModel, Order: 1380, Advanced: true}, Default: "auto"})

// Thinking effort for goal checks
var AuxiliaryGoalJudgeReasoningEffort = Register(&Enum{Meta: Meta{Key: "auxiliary.goal_judge.reasoning_effort", Label: "Goal judge thinking", Short: "Thinking effort for goal checks.", Group: GroupModel, Order: 1390, Advanced: true}, Values: reasoningChoices, Null: "the model's default"})

// Time limit for goal checks
var AuxiliaryGoalJudgeTimeout = Register(&Duration{Meta: Meta{Key: "auxiliary.goal_judge.timeout", Label: "Goal judge timeout", Short: "Time limit for goal checks.", Group: GroupModel, Order: 1400, Advanced: true}, Default: time.Minute})

// API key for splitting kanban tasks
var AuxiliaryKanbanDecomposerApiKey = Register(&String{Meta: Meta{Key: "auxiliary.kanban_decomposer.api_key", Label: "Decomposer key", Short: "API key for splitting kanban tasks.", Help: "Empty uses your normal login.", Group: GroupModel, Order: 1410, Advanced: true}, Masked: true, Default: ""})

// Custom endpoint address for splitting kanban tasks
var AuxiliaryKanbanDecomposerBaseUrl = Register(&String{Meta: Meta{Key: "auxiliary.kanban_decomposer.base_url", Label: "Decomposer URL", Short: "Custom endpoint address for splitting kanban tasks.", Group: GroupModel, Order: 1420, Advanced: true}, Default: ""})

// Extra request fields for splitting kanban tasks
var AuxiliaryKanbanDecomposerExtraBody = Register(&Map{Meta: Meta{Key: "auxiliary.kanban_decomposer.extra_body", Label: "Decomposer fields", Short: "Extra request fields for splitting kanban tasks.", Group: GroupModel, Order: 1430, Advanced: true}})

// Model for splitting kanban tasks
var AuxiliaryKanbanDecomposerModel = Register(&String{Meta: Meta{Key: "auxiliary.kanban_decomposer.model", Label: "Decomposer model", Short: "Model for splitting kanban tasks.", Help: "Empty picks one for you.", Group: GroupModel, Order: 1440, Advanced: true}, Default: ""})

// Provider for splitting kanban tasks
var AuxiliaryKanbanDecomposerProvider = Register(&String{Meta: Meta{Key: "auxiliary.kanban_decomposer.provider", Label: "Decomposer provider", Short: "Provider for splitting kanban tasks.", Help: "Auto picks one for you.", Group: GroupModel, Order: 1450, Advanced: true}, Default: "auto"})

// Thinking effort for splitting kanban tasks
var AuxiliaryKanbanDecomposerReasoningEffort = Register(&Enum{Meta: Meta{Key: "auxiliary.kanban_decomposer.reasoning_effort", Label: "Decomposer thinking", Short: "Thinking effort for splitting kanban tasks.", Group: GroupModel, Order: 1460, Advanced: true}, Values: reasoningChoices, Null: "the model's default"})

// Time limit for splitting kanban tasks
var AuxiliaryKanbanDecomposerTimeout = Register(&Duration{Meta: Meta{Key: "auxiliary.kanban_decomposer.timeout", Label: "Decomposer timeout", Short: "Time limit for splitting kanban tasks.", Group: GroupModel, Order: 1470, Advanced: true}, Default: 3 * time.Minute})

// API key for MCP tool reasoning
var AuxiliaryMcpApiKey = Register(&String{Meta: Meta{Key: "auxiliary.mcp.api_key", Label: "MCP key", Short: "API key for MCP tool reasoning.", Help: "Empty uses your normal login.", Group: GroupModel, Order: 1480, Advanced: true}, Masked: true, Default: ""})

// Custom endpoint address for MCP tool reasoning
var AuxiliaryMcpBaseUrl = Register(&String{Meta: Meta{Key: "auxiliary.mcp.base_url", Label: "MCP URL", Short: "Custom endpoint address for MCP tool reasoning.", Group: GroupModel, Order: 1490, Advanced: true}, Default: ""})

// Extra request fields for MCP tool reasoning
var AuxiliaryMcpExtraBody = Register(&Map{Meta: Meta{Key: "auxiliary.mcp.extra_body", Label: "MCP fields", Short: "Extra request fields for MCP tool reasoning.", Group: GroupModel, Order: 1500, Advanced: true}})

// Model for MCP tool reasoning
var AuxiliaryMcpModel = Register(&String{Meta: Meta{Key: "auxiliary.mcp.model", Label: "MCP model", Short: "Model for MCP tool reasoning.", Help: "Empty picks one for you.", Group: GroupModel, Order: 1510, Advanced: true}, Default: ""})

// Provider for MCP tool reasoning
var AuxiliaryMcpProvider = Register(&String{Meta: Meta{Key: "auxiliary.mcp.provider", Label: "MCP provider", Short: "Provider for MCP tool reasoning.", Help: "Auto picks one for you.", Group: GroupModel, Order: 1520, Advanced: true}, Default: "auto"})

// Thinking effort for MCP tool reasoning
var AuxiliaryMcpReasoningEffort = Register(&Enum{Meta: Meta{Key: "auxiliary.mcp.reasoning_effort", Label: "MCP thinking", Short: "Thinking effort for MCP tool reasoning.", Group: GroupModel, Order: 1530, Advanced: true}, Values: reasoningChoices, Null: "the model's default"})

// Time limit for MCP tool reasoning
var AuxiliaryMcpTimeout = Register(&Duration{Meta: Meta{Key: "auxiliary.mcp.timeout", Label: "MCP timeout", Short: "Time limit for MCP tool reasoning.", Group: GroupModel, Order: 1540, Advanced: true}, Default: 30 * time.Second})

// API key for memory search rewriting
var AuxiliaryMemoryQueryRewriteApiKey = Register(&String{Meta: Meta{Key: "auxiliary.memory_query_rewrite.api_key", Label: "Memory query key", Short: "API key for memory search rewriting.", Help: "Empty uses your normal login.", Group: GroupModel, Order: 1550, Advanced: true}, Masked: true, Default: ""})

// Custom endpoint address for memory search rewriting
var AuxiliaryMemoryQueryRewriteBaseUrl = Register(&String{Meta: Meta{Key: "auxiliary.memory_query_rewrite.base_url", Label: "Memory query URL", Short: "Custom endpoint address for memory search rewriting.", Group: GroupModel, Order: 1560, Advanced: true}, Default: ""})

// Extra request fields for memory search rewriting
var AuxiliaryMemoryQueryRewriteExtraBody = Register(&Map{Meta: Meta{Key: "auxiliary.memory_query_rewrite.extra_body", Label: "Memory query fields", Short: "Extra request fields for memory search rewriting.", Group: GroupModel, Order: 1570, Advanced: true}})

// Model for memory search rewriting
var AuxiliaryMemoryQueryRewriteModel = Register(&String{Meta: Meta{Key: "auxiliary.memory_query_rewrite.model", Label: "Memory query model", Short: "Model for memory search rewriting.", Help: "Empty picks one for you.", Group: GroupModel, Order: 1580, Advanced: true}, Default: ""})

// Provider for memory search rewriting
var AuxiliaryMemoryQueryRewriteProvider = Register(&String{Meta: Meta{Key: "auxiliary.memory_query_rewrite.provider", Label: "Memory query provider", Short: "Provider for memory search rewriting.", Help: "Auto picks one for you.", Group: GroupModel, Order: 1590, Advanced: true}, Default: "auto"})

// Time limit for memory search rewriting
var AuxiliaryMemoryQueryRewriteTimeout = Register(&Duration{Meta: Meta{Key: "auxiliary.memory_query_rewrite.timeout", Label: "Memory query timeout", Short: "Time limit for memory search rewriting.", Group: GroupModel, Order: 1600, Advanced: true}, Default: 8 * time.Second})

// API key for mixture final answers
var AuxiliaryMoaAggregatorApiKey = Register(&String{Meta: Meta{Key: "auxiliary.moa_aggregator.api_key", Label: "Aggregator key", Short: "API key for mixture final answers.", Help: "Empty uses your normal login.", Group: GroupModel, Order: 1610, Advanced: true}, Masked: true, Default: ""})

// Custom endpoint address for mixture final answers
var AuxiliaryMoaAggregatorBaseUrl = Register(&String{Meta: Meta{Key: "auxiliary.moa_aggregator.base_url", Label: "Aggregator URL", Short: "Custom endpoint address for mixture final answers.", Group: GroupModel, Order: 1620, Advanced: true}, Default: ""})

// Extra request fields for mixture final answers
var AuxiliaryMoaAggregatorExtraBody = Register(&Map{Meta: Meta{Key: "auxiliary.moa_aggregator.extra_body", Label: "Aggregator fields", Short: "Extra request fields for mixture final answers.", Group: GroupModel, Order: 1630, Advanced: true}})

// Model for mixture final answers
var AuxiliaryMoaAggregatorModel = Register(&String{Meta: Meta{Key: "auxiliary.moa_aggregator.model", Label: "Aggregator model", Short: "Model for mixture final answers.", Help: "Empty picks one for you.", Group: GroupModel, Order: 1640, Advanced: true}, Default: ""})

// Provider for mixture final answers
var AuxiliaryMoaAggregatorProvider = Register(&String{Meta: Meta{Key: "auxiliary.moa_aggregator.provider", Label: "Aggregator provider", Short: "Provider for mixture final answers.", Help: "Auto picks one for you.", Group: GroupModel, Order: 1650, Advanced: true}, Default: "auto"})

// Time limit for mixture final answers
var AuxiliaryMoaAggregatorTimeout = Register(&Duration{Meta: Meta{Key: "auxiliary.moa_aggregator.timeout", Label: "Aggregator timeout", Short: "Time limit for mixture final answers.", Group: GroupModel, Order: 1660, Advanced: true}, Default: 15 * time.Minute})

// API key for mixture reference answers
var AuxiliaryMoaReferenceApiKey = Register(&String{Meta: Meta{Key: "auxiliary.moa_reference.api_key", Label: "Reference key", Short: "API key for mixture reference answers.", Help: "Empty uses your normal login.", Group: GroupModel, Order: 1670, Advanced: true}, Masked: true, Default: ""})

// Custom endpoint address for mixture reference answers
var AuxiliaryMoaReferenceBaseUrl = Register(&String{Meta: Meta{Key: "auxiliary.moa_reference.base_url", Label: "Reference URL", Short: "Custom endpoint address for mixture reference answers.", Group: GroupModel, Order: 1680, Advanced: true}, Default: ""})

// Extra request fields for mixture reference answers
var AuxiliaryMoaReferenceExtraBody = Register(&Map{Meta: Meta{Key: "auxiliary.moa_reference.extra_body", Label: "Reference fields", Short: "Extra request fields for mixture reference answers.", Group: GroupModel, Order: 1690, Advanced: true}})

// Model for mixture reference answers
var AuxiliaryMoaReferenceModel = Register(&String{Meta: Meta{Key: "auxiliary.moa_reference.model", Label: "Reference model", Short: "Model for mixture reference answers.", Help: "Empty picks one for you.", Group: GroupModel, Order: 1700, Advanced: true}, Default: ""})

// Provider for mixture reference answers
var AuxiliaryMoaReferenceProvider = Register(&String{Meta: Meta{Key: "auxiliary.moa_reference.provider", Label: "Reference provider", Short: "Provider for mixture reference answers.", Help: "Auto picks one for you.", Group: GroupModel, Order: 1710, Advanced: true}, Default: "auto"})

// Time limit for mixture reference answers
var AuxiliaryMoaReferenceTimeout = Register(&Duration{Meta: Meta{Key: "auxiliary.moa_reference.timeout", Label: "Reference timeout", Short: "Time limit for mixture reference answers.", Group: GroupModel, Order: 1720, Advanced: true}, Default: 15 * time.Minute})

// API key for job item urgency scoring
var AuxiliaryMonitorApiKey = Register(&String{Meta: Meta{Key: "auxiliary.monitor.api_key", Label: "Monitor key", Short: "API key for job item urgency scoring.", Help: "Empty uses your normal login.", Group: GroupModel, Order: 1730, Advanced: true}, Masked: true, Default: ""})

// Custom endpoint address for job item urgency scoring
var AuxiliaryMonitorBaseUrl = Register(&String{Meta: Meta{Key: "auxiliary.monitor.base_url", Label: "Monitor URL", Short: "Custom endpoint address for job item urgency scoring.", Group: GroupModel, Order: 1740, Advanced: true}, Default: ""})

// Extra request fields for job item urgency scoring
var AuxiliaryMonitorExtraBody = Register(&Map{Meta: Meta{Key: "auxiliary.monitor.extra_body", Label: "Monitor fields", Short: "Extra request fields for job item urgency scoring.", Group: GroupModel, Order: 1750, Advanced: true}})

// Model for job item urgency scoring
var AuxiliaryMonitorModel = Register(&String{Meta: Meta{Key: "auxiliary.monitor.model", Label: "Monitor model", Short: "Model for job item urgency scoring.", Help: "Empty picks one for you.", Group: GroupModel, Order: 1760, Advanced: true}, Default: ""})

// Provider for job item urgency scoring
var AuxiliaryMonitorProvider = Register(&String{Meta: Meta{Key: "auxiliary.monitor.provider", Label: "Monitor provider", Short: "Provider for job item urgency scoring.", Help: "Auto picks one for you.", Group: GroupModel, Order: 1770, Advanced: true}, Default: "auto"})

// Thinking effort for job item urgency scoring
var AuxiliaryMonitorReasoningEffort = Register(&Enum{Meta: Meta{Key: "auxiliary.monitor.reasoning_effort", Label: "Monitor thinking", Short: "Thinking effort for job item urgency scoring.", Group: GroupModel, Order: 1780, Advanced: true}, Values: reasoningChoices, Null: "the model's default"})

// Time limit for job item urgency scoring
var AuxiliaryMonitorTimeout = Register(&Duration{Meta: Meta{Key: "auxiliary.monitor.timeout", Label: "Monitor timeout", Short: "Time limit for job item urgency scoring.", Group: GroupModel, Order: 1790, Advanced: true}, Default: time.Minute})

// Free OpenRouter model tried when no helper model is set up
var AuxiliaryOpenrouterModel = Register(&String{Meta: Meta{Key: "auxiliary.openrouter_model", Label: "Fallback free model", Short: "Free OpenRouter model tried when no helper model is set up.", Help: "Empty uses a built-in free model.", Group: GroupModel, Order: 1800, Advanced: true}, Default: ""})

// API key for profile descriptions
var AuxiliaryProfileDescriberApiKey = Register(&String{Meta: Meta{Key: "auxiliary.profile_describer.api_key", Label: "Profile text key", Short: "API key for profile descriptions.", Help: "Empty uses your normal login.", Group: GroupModel, Order: 1810, Advanced: true}, Masked: true, Default: ""})

// Custom endpoint address for profile descriptions
var AuxiliaryProfileDescriberBaseUrl = Register(&String{Meta: Meta{Key: "auxiliary.profile_describer.base_url", Label: "Profile text URL", Short: "Custom endpoint address for profile descriptions.", Group: GroupModel, Order: 1820, Advanced: true}, Default: ""})

// Extra request fields for profile descriptions
var AuxiliaryProfileDescriberExtraBody = Register(&Map{Meta: Meta{Key: "auxiliary.profile_describer.extra_body", Label: "Profile text fields", Short: "Extra request fields for profile descriptions.", Group: GroupModel, Order: 1830, Advanced: true}})

// Model for profile descriptions
var AuxiliaryProfileDescriberModel = Register(&String{Meta: Meta{Key: "auxiliary.profile_describer.model", Label: "Profile text model", Short: "Model for profile descriptions.", Help: "Empty picks one for you.", Group: GroupModel, Order: 1840, Advanced: true}, Default: ""})

// Provider for profile descriptions
var AuxiliaryProfileDescriberProvider = Register(&String{Meta: Meta{Key: "auxiliary.profile_describer.provider", Label: "Profile text provider", Short: "Provider for profile descriptions.", Help: "Auto picks one for you.", Group: GroupModel, Order: 1850, Advanced: true}, Default: "auto"})

// Thinking effort for profile descriptions
var AuxiliaryProfileDescriberReasoningEffort = Register(&Enum{Meta: Meta{Key: "auxiliary.profile_describer.reasoning_effort", Label: "Profile text thinking", Short: "Thinking effort for profile descriptions.", Group: GroupModel, Order: 1860, Advanced: true}, Values: reasoningChoices, Null: "the model's default"})

// Time limit for profile descriptions
var AuxiliaryProfileDescriberTimeout = Register(&Duration{Meta: Meta{Key: "auxiliary.profile_describer.timeout", Label: "Profile text timeout", Short: "Time limit for profile descriptions.", Group: GroupModel, Order: 1870, Advanced: true}, Default: time.Minute})

// API key for the review command
var AuxiliaryReviewApiKey = Register(&String{Meta: Meta{Key: "auxiliary.review.api_key", Label: "Review key", Short: "API key for the review command.", Help: "Empty uses your normal login.", Group: GroupModel, Order: 1880, Advanced: true}, Masked: true, Default: ""})

// Request format of the review model
var AuxiliaryReviewApiMode = Register(&String{Meta: Meta{Key: "auxiliary.review.api_mode", Label: "Review API style", Short: "Request format of the review model.", Help: "Empty picks it for you.", Group: GroupModel, Order: 1890, Advanced: true}, Default: ""})

// Custom endpoint address for the review command
var AuxiliaryReviewBaseUrl = Register(&String{Meta: Meta{Key: "auxiliary.review.base_url", Label: "Review URL", Short: "Custom endpoint address for the review command.", Group: GroupModel, Order: 1900, Advanced: true}, Default: ""})

// Model for the review command
var AuxiliaryReviewModel = Register(&String{Meta: Meta{Key: "auxiliary.review.model", Label: "Review model", Short: "Model for the review command.", Help: "Empty picks one for you.", Group: GroupModel, Order: 1910, Advanced: true}, Default: ""})

// Provider for the review command
var AuxiliaryReviewProvider = Register(&String{Meta: Meta{Key: "auxiliary.review.provider", Label: "Review provider", Short: "Provider for the review command.", Help: "Auto picks one for you.", Group: GroupModel, Order: 1920, Advanced: true}, Default: "auto"})

// Endpoint addresses that only work with streamed replies
var AuxiliaryStreamOnlyBaseUrls = Register(&List{Meta: Meta{Key: "auxiliary.stream_only_base_urls", Label: "Streaming-only hosts", Short: "Endpoint addresses that only work with streamed replies.", Help: "Any address containing one of these texts is requested in streaming mode.", Group: GroupModel, Order: 1930, Advanced: true}})

// API key for session titles
var AuxiliaryTitleGenerationApiKey = Register(&String{Meta: Meta{Key: "auxiliary.title_generation.api_key", Label: "Title key", Short: "API key for session titles.", Help: "Empty uses your normal login.", Group: GroupModel, Order: 1940, Advanced: true}, Masked: true, Default: ""})

// Custom endpoint address for session titles
var AuxiliaryTitleGenerationBaseUrl = Register(&String{Meta: Meta{Key: "auxiliary.title_generation.base_url", Label: "Title URL", Short: "Custom endpoint address for session titles.", Group: GroupModel, Order: 1950, Advanced: true}, Default: ""})

// Name new sessions automatically
var AuxiliaryTitleGenerationEnabled = Register(&Bool{Meta: Meta{Key: "auxiliary.title_generation.enabled", Label: "Auto titles", Short: "Name new sessions automatically.", Group: GroupModel, Order: 1960, Advanced: true}, Default: true})

// Extra request fields for session titles
var AuxiliaryTitleGenerationExtraBody = Register(&Map{Meta: Meta{Key: "auxiliary.title_generation.extra_body", Label: "Title fields", Short: "Extra request fields for session titles.", Group: GroupModel, Order: 1970, Advanced: true}})

// Language of session titles
var AuxiliaryTitleGenerationLanguage = Register(&String{Meta: Meta{Key: "auxiliary.title_generation.language", Label: "Title language", Short: "Language of session titles.", Help: "Empty matches the language of your message.", Group: GroupModel, Order: 1980, Advanced: true}, Default: ""})

// Model for session titles
var AuxiliaryTitleGenerationModel = Register(&String{Meta: Meta{Key: "auxiliary.title_generation.model", Label: "Title model", Short: "Model for session titles.", Help: "Empty picks one for you.", Group: GroupModel, Order: 1990, Advanced: true}, Default: ""})

// Replace the quick title with one written by the title model
var AuxiliaryTitleGenerationModelUpgradeEnabled = Register(&Bool{Meta: Meta{Key: "auxiliary.title_generation.model_upgrade_enabled", Label: "Better titles", Short: "Replace the quick title with one written by the title model.", Group: GroupModel, Order: 2000, Advanced: true}, Default: true})

// Pick a quicker model for titles when one is available
var AuxiliaryTitleGenerationPreferFastModel = Register(&Bool{Meta: Meta{Key: "auxiliary.title_generation.prefer_fast_model", Label: "Prefer fast model", Short: "Pick a quicker model for titles when one is available.", Group: GroupModel, Order: 2010, Advanced: true}, Default: false})

// Provider for session titles
var AuxiliaryTitleGenerationProvider = Register(&String{Meta: Meta{Key: "auxiliary.title_generation.provider", Label: "Title provider", Short: "Provider for session titles.", Help: "Auto picks one for you.", Group: GroupModel, Order: 2020, Advanced: true}, Default: "auto"})

// Thinking effort for session titles
var AuxiliaryTitleGenerationReasoningEffort = Register(&Enum{Meta: Meta{Key: "auxiliary.title_generation.reasoning_effort", Label: "Title thinking", Short: "Thinking effort for session titles.", Group: GroupModel, Order: 2030, Advanced: true}, Values: reasoningChoices, Null: "the model's default"})

// Time limit for session titles
var AuxiliaryTitleGenerationTimeout = Register(&Duration{Meta: Meta{Key: "auxiliary.title_generation.timeout", Label: "Title timeout", Short: "Time limit for session titles.", Group: GroupModel, Order: 2040, Advanced: true}, Default: 30 * time.Second})

// Retries for helper calls after a brief network error
var AuxiliaryTransientRetries = Register(&Int{Meta: Meta{Key: "auxiliary.transient_retries", Label: "Helper retries", Short: "Retries for helper calls after a brief network error.", Help: "Between 0 and 6.", Group: GroupModel, Order: 2050, Advanced: true}, Default: 2})

// API key for kanban task specs
var AuxiliaryTriageSpecifierApiKey = Register(&String{Meta: Meta{Key: "auxiliary.triage_specifier.api_key", Label: "Triage key", Short: "API key for kanban task specs.", Help: "Empty uses your normal login.", Group: GroupModel, Order: 2060, Advanced: true}, Masked: true, Default: ""})

// Custom endpoint address for kanban task specs
var AuxiliaryTriageSpecifierBaseUrl = Register(&String{Meta: Meta{Key: "auxiliary.triage_specifier.base_url", Label: "Triage URL", Short: "Custom endpoint address for kanban task specs.", Group: GroupModel, Order: 2070, Advanced: true}, Default: ""})

// Extra request fields for kanban task specs
var AuxiliaryTriageSpecifierExtraBody = Register(&Map{Meta: Meta{Key: "auxiliary.triage_specifier.extra_body", Label: "Triage fields", Short: "Extra request fields for kanban task specs.", Group: GroupModel, Order: 2080, Advanced: true}})

// Model for kanban task specs
var AuxiliaryTriageSpecifierModel = Register(&String{Meta: Meta{Key: "auxiliary.triage_specifier.model", Label: "Triage model", Short: "Model for kanban task specs.", Help: "Empty picks one for you.", Group: GroupModel, Order: 2090, Advanced: true}, Default: ""})

// Provider for kanban task specs
var AuxiliaryTriageSpecifierProvider = Register(&String{Meta: Meta{Key: "auxiliary.triage_specifier.provider", Label: "Triage provider", Short: "Provider for kanban task specs.", Help: "Auto picks one for you.", Group: GroupModel, Order: 2100, Advanced: true}, Default: "auto"})

// Thinking effort for kanban task specs
var AuxiliaryTriageSpecifierReasoningEffort = Register(&Enum{Meta: Meta{Key: "auxiliary.triage_specifier.reasoning_effort", Label: "Triage thinking", Short: "Thinking effort for kanban task specs.", Group: GroupModel, Order: 2110, Advanced: true}, Values: reasoningChoices, Null: "the model's default"})

// Time limit for kanban task specs
var AuxiliaryTriageSpecifierTimeout = Register(&Duration{Meta: Meta{Key: "auxiliary.triage_specifier.timeout", Label: "Triage timeout", Short: "Time limit for kanban task specs.", Group: GroupModel, Order: 2120, Advanced: true}, Default: 2 * time.Minute})

// API key for image analysis
var AuxiliaryVisionApiKey = Register(&String{Meta: Meta{Key: "auxiliary.vision.api_key", Label: "Vision key", Short: "API key for image analysis.", Help: "Empty uses your normal login.", Group: GroupModel, Order: 2130, Advanced: true}, Masked: true, Default: ""})

// Custom endpoint address for image analysis
var AuxiliaryVisionBaseUrl = Register(&String{Meta: Meta{Key: "auxiliary.vision.base_url", Label: "Vision URL", Short: "Custom endpoint address for image analysis.", Group: GroupModel, Order: 2140, Advanced: true}, Default: ""})

// How long to wait when downloading an image or media file
var AuxiliaryVisionDownloadTimeout = Register(&Duration{Meta: Meta{Key: "auxiliary.vision.download_timeout", Label: "Download timeout", Short: "How long to wait when downloading an image or media file.", Group: GroupModel, Order: 2150, Advanced: true}, Default: 30 * time.Second})

// Extra request fields for image analysis
var AuxiliaryVisionExtraBody = Register(&Map{Meta: Meta{Key: "auxiliary.vision.extra_body", Label: "Vision fields", Short: "Extra request fields for image analysis.", Group: GroupModel, Order: 2160, Advanced: true}})

// Model for image analysis
var AuxiliaryVisionModel = Register(&String{Meta: Meta{Key: "auxiliary.vision.model", Label: "Vision model", Short: "Model for image analysis.", Help: "Empty picks one for you.", Group: GroupModel, Order: 2170, Advanced: true}, Default: ""})

// Provider for image analysis
var AuxiliaryVisionProvider = Register(&String{Meta: Meta{Key: "auxiliary.vision.provider", Label: "Vision provider", Short: "Provider for image analysis.", Help: "Auto picks one for you.", Group: GroupModel, Order: 2180, Advanced: true}, Default: "auto"})

// Thinking effort for image analysis
var AuxiliaryVisionReasoningEffort = Register(&Enum{Meta: Meta{Key: "auxiliary.vision.reasoning_effort", Label: "Vision thinking", Short: "Thinking effort for image analysis.", Group: GroupModel, Order: 2190, Advanced: true}, Values: reasoningChoices, Null: "the model's default"})

// Time limit for image analysis
var AuxiliaryVisionTimeout = Register(&Duration{Meta: Meta{Key: "auxiliary.vision.timeout", Label: "Vision timeout", Short: "Time limit for image analysis.", Group: GroupModel, Order: 2200, Advanced: true}, Default: 2 * time.Minute})

// ID of the Bedrock guardrail to apply
var BedrockGuardrailGuardrailIdentifier = Register(&String{Meta: Meta{Key: "bedrock.guardrail.guardrail_identifier", Label: "Guardrail ID", Short: "ID of the Bedrock guardrail to apply.", Help: "Both the ID and the version must be set for the guardrail to apply.", Group: GroupModel, Order: 2210, Advanced: true}, Default: ""})

// Version of the Bedrock guardrail to apply
var BedrockGuardrailGuardrailVersion = Register(&String{Meta: Meta{Key: "bedrock.guardrail.guardrail_version", Label: "Guardrail version", Short: "Version of the Bedrock guardrail to apply.", Group: GroupModel, Order: 2220, Advanced: true}, Default: ""})

// Whether guardrail checks block streamed replies (sync) or not
var BedrockGuardrailStreamProcessingMode = Register(&Enum{Meta: Meta{Key: "bedrock.guardrail.stream_processing_mode", Label: "Guardrail streaming", Short: "Whether guardrail checks block streamed replies (sync) or not.", Group: GroupModel, Order: 2230, Advanced: true}, Default: "async", Values: []Choice{{Value: "async", Label: "Async", Meaning: "Check without delaying replies."}, {Value: "sync", Label: "Sync", Meaning: "Check before text is shown."}}})

// Whether Bedrock reports why a guardrail acted
var BedrockGuardrailTrace = Register(&Enum{Meta: Meta{Key: "bedrock.guardrail.trace", Label: "Guardrail trace", Short: "Whether Bedrock reports why a guardrail acted.", Group: GroupModel, Order: 2240, Advanced: true}, Default: "disabled", Values: []Choice{{Value: "disabled", Label: "Off", Meaning: "No guardrail trace."}, {Value: "enabled", Label: "On", Meaning: "Include guardrail trace."}, {Value: "enabled_full", Label: "Full", Meaning: "Include full guardrail trace."}}})

// AWS region used for Bedrock requests
var BedrockRegion = Register(&String{Meta: Meta{Key: "bedrock.region", Label: "Bedrock region", Short: "AWS region used for Bedrock requests.", Help: "Blank takes the region from the endpoint address, else us-east-1.", Group: GroupModel, Order: 2250, Advanced: true}, Default: ""})

// How to pick between several keys of one provider
var CredentialPoolStrategies = Register(&Map{Meta: Meta{Key: "credential_pool_strategies", Label: "Key rotation", Short: "How to pick between several keys of one provider.", Help: "Older form. The provider's login rotation setting wins when set.", Group: GroupModel, Order: 2260, Advanced: true}})

// Stay on the main model if its rate limit ends within this time
var FallbackMinSwitchResetSeconds = Register(&Duration{Meta: Meta{Key: "fallback.min_switch_reset_seconds", Label: "Stay-put window", Short: "Stay on the main model if its rate limit ends within this time.", Help: "0 turns this off, so any rate limit switches to the fallback.", Group: GroupModel, Order: 2270, Advanced: true}, Default: 0})

// Per-model limits such as context size, by provider
var ModelOverrides = Register(&Map{Meta: Meta{Key: "model_overrides", Label: "Model overrides", Short: "Per-model limits such as context size, by provider.", Group: GroupModel, Order: 2280, Advanced: true}})

// Lowest coding score for automatic OpenRouter model routing
var OpenrouterMinCodingScore = Register(&Float{Meta: Meta{Key: "openrouter.min_coding_score", Label: "Min coding score", Short: "Lowest coding score for automatic OpenRouter model routing.", Group: GroupModel, Order: 2290, Advanced: true}, Default: 0.65})

// Let OpenRouter reuse identical earlier answers
var OpenrouterResponseCache = Register(&Bool{Meta: Meta{Key: "openrouter.response_cache", Label: "Response cache", Short: "Let OpenRouter reuse identical earlier answers.", Group: GroupModel, Order: 2300, Advanced: true}, Default: true})

// How long OpenRouter keeps cached answers
var OpenrouterResponseCacheTtl = Register(&Int{Meta: Meta{Key: "openrouter.response_cache_ttl", Label: "Cache lifetime", Short: "How long OpenRouter keeps cached answers.", Group: GroupModel, Order: 2310, Advanced: true}, Default: 300})

// How long the provider keeps your prompt cached
var PromptCachingCacheTtl = Register(&String{Meta: Meta{Key: "prompt_caching.cache_ttl", Label: "Prompt cache time", Short: "How long the provider keeps your prompt cached.", Group: GroupModel, Order: 2320, Advanced: true}, Default: "5m"})

// Your own named model endpoints and their settings
var Providers = Register(&Map{Meta: Meta{Key: "providers", Label: "Providers", Short: "Your own named model endpoints and their settings.", Group: GroupModel, Order: 2330, Advanced: true}})

// Google Cloud project used for Vertex AI requests
var VertexProjectId = Register(&String{Meta: Meta{Key: "vertex.project_id", Label: "Vertex project", Short: "Google Cloud project used for Vertex AI requests.", Group: GroupModel, Order: 2340, Advanced: true}, Default: ""})

// Google Cloud region used for Vertex AI requests
var VertexRegion = Register(&String{Meta: Meta{Key: "vertex.region", Label: "Vertex region", Short: "Google Cloud region used for Vertex AI requests.", Group: GroupModel, Order: 2350, Advanced: true}, Default: "global"})

// Which upstream providers OpenRouter may route requests to, per model or for all
var ProviderRouting = Register(&Map{Meta: Meta{Key: "provider_routing", Label: "OpenRouter routing", Short: "Which upstream providers OpenRouter may send your requests to.", Help: "Keys such as only, ignore, order and sort, optionally under a model name.", Group: GroupModel, Order: 3000, Advanced: true, Apply: NextTurn}})

// Extra HTTP headers sent with every model request
var ModelDefaultHeaders = Register(&Map{Meta: Meta{Key: "model.default_headers", Label: "Extra headers", Short: "Extra HTTP headers sent with every request to the model.", Help: "Header names map to values. A header set here replaces the built-in one.", Group: GroupModel, Order: 3010, Advanced: true, Apply: NewSession}})

// How icebath signs in to Azure Foundry
var ModelAuthMode = Register(&Enum{Meta: Meta{Key: "model.auth_mode", Label: "Azure sign-in", Short: "How icebath signs in to Azure Foundry.", Group: GroupModel, Order: 3020, Advanced: true, Apply: NewSession}, Default: "api_key", Values: []Choice{{Value: "api_key", Label: "Key", Meaning: "An API key."}, {Value: "entra_id", Label: "Entra ID", Meaning: "Your Microsoft Entra sign-in."}}})

// Entra ID options of the Azure Foundry sign-in
var ModelEntra = Register(&Map{Meta: Meta{Key: "model.entra", Label: "Entra options", Short: "Options of the Microsoft Entra sign-in, such as the token scope.", Group: GroupModel, Order: 3030, Advanced: true, Apply: NewSession}})

// Context window of the main model, overriding what icebath detects
var ModelContextLength = Register(&Int{Meta: Meta{Key: "model.context_length", Label: "Context window", Short: "How many tokens the main model can hold, when icebath guesses wrong.", Help: "Unset uses the size the provider reports or the built-in table.", Group: GroupModel, Order: 3100, Advanced: true, Apply: NewSession}, Null: "the size icebath detects", Min: at(1), Unit: "tokens"})

// Model that describes videos for the agent
var AuxiliaryVideoModel = Register(&String{Meta: Meta{Key: "auxiliary.video.model", Label: "Video model", Short: "Model that watches videos for the agent.", Group: GroupModel, Order: 2210, Advanced: true, Apply: NextTurn}, Null: "the image analysis model"})

// Parallel image and video preparation steps
var AuxiliaryVisionMaxConcurrency = Register(&Int{Meta: Meta{Key: "auxiliary.vision.max_concurrency", Label: "Image workers", Short: "How many images are prepared at the same time.", Group: GroupModel, Order: 2220, Advanced: true, Apply: Restart}, Null: "one per processor core", Min: at(1)})

// Randomness of image and video descriptions
var AuxiliaryVisionTemperature = Register(&Float{Meta: Meta{Key: "auxiliary.vision.temperature", Label: "Vision temperature", Short: "How varied image and video descriptions may be.", Help: "Low values give steadier descriptions.", Group: GroupModel, Order: 2230, Advanced: true, Apply: NextTurn}, Default: 0.1, Min: at(0.0), Max: at(2.0)})
