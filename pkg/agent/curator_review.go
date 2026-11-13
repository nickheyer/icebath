package agent

import (
	"context"

	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

func init() { memory.CuratorHooks.RunReview = runCuratorReview }

// Runs the curator review fork with nudges off, background review origin, fresh read marks and silenced output
func runCuratorReview(ctx context.Context, req memory.CuratorReviewRequest) (*memory.CuratorReviewResult, error) {
	o := AIAgentOptions{
		Model: req.Model, Provider: req.Provider, RequestOverrides: req.RequestOverrides,
		ReasoningConfig: req.ReasoningConfig, EnabledToolsets: req.EnabledToolsets, MaxIterations: new(req.MaxIterations),
		QuietMode: req.QuietMode, Platform: req.Platform, SkipContextFiles: req.SkipContextFiles, SkipMemory: req.SkipMemory,
		ACPCommand: req.ACPCommand, ACPArgs: req.ACPArgs, CredentialPool: req.CredentialPool,
	}
	if r := req.Route; r != nil {
		o.APIKey, o.BaseURL, o.APIMode = r.APIKey, r.BaseURL, string(r.Mode)
	}
	a, err := NewAIAgent(ctx, o)
	if err != nil {
		return nil, err
	}
	defer func() {
		defer func() { _ = recover() }()
		_ = a.Close()
	}()
	a.memoryNudgeInterval, a.skillNudgeInterval = req.MemoryNudgeInterval, req.SkillNudgeInterval
	a.memoryWriteOrigin = req.WriteOrigin
	func() {
		defer func() { _ = recover() }()
		skills.ResetBackgroundReviewReadMarks(a.env)
	}()
	result, err := a.RunConversation(tools.SilenceOutput(ctx), req.Prompt, RunConversationOptions{})
	if err != nil {
		return nil, err
	}
	live := a.liveSessionMessages()
	messages := make([]any, len(live))
	for i, m := range live {
		messages[i] = state.MessageToDict(m)
	}
	return &memory.CuratorReviewResult{Conversation: result, Messages: messages}, nil
}
