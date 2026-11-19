package gateway

// Watermarks of each thread and member derived from terminal events of a room log
func DeriveMemberWatermarks(roomValue any, events []any, localProfiles []string) (map[[2]string]int64, error) {
	room, err := ValidateDiscussionRoom(roomValue, localProfiles)
	if err != nil {
		return nil, err
	}
	validated, err := discussionValidatedEvents(events, room)
	if err != nil {
		return nil, err
	}
	return discussionDeriveWatermarks(validated)
}
