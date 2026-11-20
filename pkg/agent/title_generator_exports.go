package agent

import "context"

// Heading a stored prompt carries when it was built before the skill safety rule moved to compaction
const SkillSafetyHeading = skillSafetyHeading

// Model title from an opening message alone with the default options, empty when disabled or rejected
func GenerateTitle(ctx context.Context, userMessage string) string {
	return generateTitle(ctx, userMessage, generateTitleOptions{})
}
