package agent

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/state"
)

// Per agent record of the skills index difference the model was told
type skillsIndexDeltaState struct {
	skillsIndexTold skills.SkillsIndexNotes
}

// Stages the untold skills index change note into the per turn note slot
func stageSkillsIndexNote(ctx context.Context, a *AIAgent, activeSystemPrompt string, messages []*state.Message) bool {
	history := make([]map[string]any, 0, len(messages))
	for _, m := range messages {
		if m != nil {
			history = append(history, state.MessageToDict(m))
		}
	}
	current := func() (prompt string, err error) {
		defer func() {
			if r := recover(); r != nil {
				prompt, err = "", fmt.Errorf("%v", r)
			}
		}()
		return skillsPrompt(ctx, a)
	}
	if !a.skillsIndexTold.Stage(a.provider, a.apiMode, a.sessionID, current, activeSystemPrompt, history) {
		return false
	}
	a.skillsIndexNoteText = a.skillsIndexTold.Consume()
	return true
}
