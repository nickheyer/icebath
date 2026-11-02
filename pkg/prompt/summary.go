package prompt

import (
	"fmt"
	"strings"
)

// Sections a handoff summary is written in
var SummarySections = []string{"Goal", "Progress", "Key facts", "Decisions", "Open items"}

// Assistant turn written after a summary so the kept tail still follows an assistant message
const Ack = "Understood. I have the summary of the earlier conversation and will continue from here."

// System prompt of a model writing the handoff summary of a transcript, extra guidance appended
func SummarySystem(extra string) string {
	return Compose(New().Para(
		"You write handoff summaries of agent sessions so the same agent can continue the work without the original transcript.",
		"Write the summary in these sections, each as a short heading followed by concise bullet points:",
		strings.Join(SummarySections, ", ")+".",
		"Keep every file path, host name, command, identifier and value that later work will need. State plainly what was verified and what was only attempted.",
	).String(), extra)
}

// System prompt of a model merging partial summaries into one, extra guidance appended
func SummaryCombineSystem(extra string) string {
	return Compose("You merge partial handoff summaries of one agent session into a single summary with the same sections: "+strings.Join(SummarySections, ", ")+". Keep every path, host, command, identifier and value. Drop nothing that later work will need.", extra)
}

// Message asking for the summary of a transcript, marked as one part of several when parts is above one
func SummaryRequest(transcript string, part, parts int) string {
	d := New()
	if parts > 1 {
		d.Tagged("transcript", transcript, Field{"part", fmt.Sprintf("%d of %d", part, parts)})
		return d.Para("Write the handoff summary of this part of the transcript.").String()
	}
	return d.Tagged("transcript", transcript).Para("Write the handoff summary of this transcript.").String()
}

// One partial summary labelled with its place, as the merge reads it
func SummaryPart(part, parts int, text string) string {
	return fmt.Sprintf("Part %d of %d:\n%s", part, parts, strings.TrimSpace(text))
}

// Summary as the session keeps it in place of the turns it replaced
func SummaryNote(text string) string {
	return "Summary of the earlier conversation:\n\n" + strings.TrimSpace(text)
}

// System prompt of a model answering a question from the content of one page
const PageSystem = "You answer questions about a web page using only its content below. Quote exact wording where it matters and say plainly when the page does not contain the answer."

// Message carrying a page and the question to answer from it
func PageRequest(page, question string) string {
	return New().Tagged("page", page).Para(KV("Prompt", question)).String()
}
