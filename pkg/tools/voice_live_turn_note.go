package tools

import "github.com/nickheyer/icebath/pkg/internal/pystr"

// VoiceLiveTurnNote is prepended to the model input of a turn delegated from the live voice layer
const VoiceLiveTurnNote = "[Note: this message is a delegation from a live spoken conversation. The text is a voice " +
	"transcript (it may contain mis-hearings, hesitations and later corrections; use the latest " +
	"intent). Your reply will be spoken aloud by a voice model that paraphrases it: answer in plain " +
	"conversational sentences, keep it short (a few sentences unless the user asked for detail), no " +
	"markdown, no lists, no code blocks, no URLs read out character by character. Do the work with " +
	"your tools as usual; only the final facts need to be spoken. Do not claim an action succeeded " +
	"before it actually did.]"

// Live voice turn note followed by the recent spoken exchange the client sent, the bare note without one
func VoiceLiveTurnNoteFor(context string) string {
	context = pystr.Strip(context)
	if context == "" {
		return VoiceLiveTurnNote
	}
	return VoiceLiveTurnNote + "\n[Recent spoken conversation, newest last:\n" + context + "]"
}
