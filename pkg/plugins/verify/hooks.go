package verify

import (
	"context"

	"github.com/nickheyer/icebath/pkg/settings"
)

// How-to-verify advice appended to verification stop nudges
const CodingVerifyGuidanceText = "[Coding] Before you run tests/linters or call this done: if this is " +
	"creative UI/visual work, hold off on tests and linters until the user says " +
	"they like the result or you're about to commit. And before every commit, " +
	"clean your work: keep it KISS/DRY, match the surrounding code style, and be " +
	"elitist, shorthand, clever, concise, efficient, and elegant."

// Bound on consecutive pre_verify continue directives per turn from agent.max_verify_nudges of the profile of ctx, never below zero
func MaxVerifyNudges(ctx context.Context) int {
	return max(settings.AgentMaxVerifyNudges.Get(ctx), 0)
}

// Guidance appended to verification stop nudges, empty when agent.verify_guidance of the profile of ctx turns it off
func CodingVerifyGuidance(ctx context.Context) string {
	if !settings.AgentVerifyGuidance.Get(ctx) {
		return ""
	}
	return CodingVerifyGuidanceText
}
