package tools

// How a set of questions to the user ended
const (
	// The user answered or skipped every question
	Submitted = "submitted"
	// The user dismissed the questions, those answered before kept
	Cancelled = "cancelled"
	// No answer came in time, those answered before kept
	TimedOut = "timed_out"
	// No one could be asked, the notice saying why
	Undelivered = "undelivered"
)

// Question to the user, one of the set Env.Clarify asks at once
type Question struct {
	// Stable within the set, the key of its answer
	ID   string
	Text string
	// Choices to pick from as shown, the recommended one first; none for an open question, which takes typed text
	Choices []string
	// Whether any number of the choices may be picked
	MultiSelect bool
	// The answer is a secret, such as a password: masked as it is typed and never shown again
	Secret bool
}

// How the questions ended and the answers given
type Reply struct {
	// Answers by question id: the picked choices or the typed text, empty for a question the user skipped. A question with no entry was never
	// answered
	Answers map[string][]string
	Outcome string
	// Why the questions ended without being submitted, for the model
	Notice string
}
