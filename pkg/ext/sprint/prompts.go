package sprint

import (
	"fmt"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/prompt"
)

// Harness section every member shares, the tools it may call, the tool that ends its stage and further rules
func team(tools []string, must string, extra ...string) *prompt.Harness {
	return &prompt.Harness{Reader: "the person watching the arena", Speaker: "the sprint", Gatekeeper: "the person", Tools: tools, SharedTree: true, Must: must, Extra: extra}
}

// Base prompt of a role, sent in place of the built in one, naming the tools the role is offered
func roleSystem(role Role, t Team) string {
	offered, denied := roleTools(role)
	if offered == nil {
		offered = prompt.Without(prompt.Builtins(), denied)
	}
	switch role {
	case RoleDirector:
		return prompt.System{
			Identity: []string{"You are the director of a sprint team.", "You turn the request into a brief, then report the result.", "The architect plans, builders do all the work, the reviewer checks it.", "You do not review or write code."},
			Rules: []string{
				"Confirm that what the request names exists with Glob or Grep, in at most five calls. Leave reading the code to the team.",
				"The brief states what to deliver, how to check it and what to leave alone. Findings, diagnoses and designs are the team's work, keep them out.",
				"Choices settled with the person or on their behalf go in the notes of " + ToolBrief + ".",
			},
			Harness: team(offered, "", "The brief stage ends only when you call "+ToolBrief+". A turn that ends without it earns a reminder, nothing more."),
			Writing: &prompt.Writing{Reader: "the person who asked for the sprint"},
			Closing: []string{"Call " + ToolBrief + " once with the brief. The report at the end is written from the handoffs and verdicts alone, without tool calls."},
		}.String()
	case RoleArchitect:
		return prompt.System{
			Identity: []string{"You are the architect of a sprint team.", "You cut the brief into tickets and call " + ToolPlan + ".", "That is the whole job. Builders do everything else, including investigating, reviewing, measuring, testing and fixing."},
			Rules: []string{
				"Outline the tree first, then Grep for the symbols the brief names. Read a file only when those leave a ticket boundary unclear, several at once through file_paths.",
				"Do not work out the changes. When the brief asks to find problems, cut tickets by area, each telling its builder to find and fix them in the files it owns.",
				"A ticket states what to deliver and lists every file its builder will touch. No two tickets list the same file, and a file in no ticket is off limits to everyone. Add a dependency only when a ticket cannot start without another.",
			},
			Harness: team(offered, ToolPlan),
			Closing: []string{"Call " + ToolPlan + " within 15 tool calls."},
		}.String()
	case RoleBuilder:
		return prompt.System{
			Identity: []string{"You are a builder on a sprint team.", "You build one ticket, as the ticket and the brief say."},
			Rules: []string{
				"Orient with Outline and Grep, Read the files you own once, then change them with Edit, or Write for a new file.",
				"Build with Shell and run only the tests covering the packages the ticket touches, do not re-run tests. The commands and what they showed go in the tested field of " + ToolHandoff + ".",
				"Call " + ToolHandoff + " when the ticket is done, when you are blocked, or with a question only the reviewer can settle. The verdict arrives as your next message.",
			},
			Harness:  team(offered, ToolHandoff),
			Delivery: &prompt.Delivery{Asked: "the reviewer through " + ToolHandoff, Extra: []string{"Change only the files the ticket owns. A build broken by a file you do not own goes in the handoff, not under your hands."}},
		}.String()
	}
	rework := "Rework is not allowed, so accept or fail every handoff."
	if t.MaxRework > 0 {
		rework = fmt.Sprintf("A ticket sent back %d times is accepted or failed on its next handoff.", t.MaxRework)
	}
	return prompt.System{
		Identity: []string{"You are the reviewer of a sprint team.", "You judge each handoff against its ticket and the brief.", "You do not write code."},
		Rules: []string{
			"Done: Read the files the handoff names, not the git diff of the tree, which holds the work of other builders. Build with Shell and run the tests covering them. Accept when the ticket is met and the checks pass. Otherwise send rework naming the file, the gap and what done looks like. Fail only when it cannot be built as planned.",
			"Blocked or question: decide and answer with guidance. Fail only when the block cannot be cleared.",
			rework,
		},
		Harness: team(offered, ToolVerdict),
		Closing: []string{"Call " + ToolVerdict + " once for the ticket named."},
	}.String()
}

// First message of the director, the request to settle and whether the person can be asked
func directorOpening(request string, interactive bool) string {
	ask := "Nobody can be asked, settle open choices yourself and put them in the notes."
	if interactive {
		ask = "Ask the person with " + ToolAsk + " only when a choice would change the work."
	}
	return prompt.New().Para("Request:").Raw(request).Para(ask + " Then call " + ToolBrief + ".").String()
}

// Brief as the team reads it
func briefDoc(b Brief) *prompt.Doc {
	return prompt.New().Heading(1, "Brief").Raw(b.Objective).Titled("Acceptance", b.Acceptance...).Titled("Constraints", b.Constraints...).Titled("Leave alone", b.Untouched...).Para("Notes", strings.TrimSpace(b.Notes))
}

func briefText(b Brief) string { return briefDoc(b).String() }

// First message of the architect, the brief and the ticket limit
func architectOpening(b Brief, maxTickets, builders int) string {
	return briefDoc(b).Paraf("Cut this into at most %d tickets for %d builders working in parallel, then call %s.", maxTickets, builders, ToolPlan).String()
}

// Plan title, summary and approach as a member reads them
func planHead(p Plan) *prompt.Doc {
	return prompt.New().Heading(1, "Plan: "+p.Title).Raw(p.Summary).Raw(p.Approach)
}

// Plan as the team reads it, with every ticket
func planText(p Plan, tickets []Ticket) string {
	d := planHead(p).Titled("Risks", p.Risks...).Heading(2, "Tickets")
	for _, t := range tickets {
		d.Para(prompt.H(3, t.ID+" "+t.Title), strings.TrimSpace(t.Description), prompt.KV("Files", strings.Join(t.Files, ", ")), prompt.KV("Depends on", strings.Join(t.DependsOn, ", ")), prompt.KV("Size", t.Size))
	}
	return d.String()
}

// Tickets already done with the first line of their last handoff
func builtDoc(tickets []Ticket) *prompt.Doc {
	var items []string
	for _, t := range tickets {
		if t.Status == TicketDone {
			items = append(items, t.ID+" "+t.Title+": "+firstLine(t.Report))
		}
	}
	return prompt.New().Titled(prompt.H(2, "Already built"), items...)
}

// Ticket as its builder reads it, with the closing instruction
func ticketDoc(t Ticket) *prompt.Doc {
	return prompt.New().
		Para(prompt.H(1, "Your ticket "+t.ID+" "+t.Title), strings.TrimSpace(t.Description), prompt.KV("Files you own", strings.Join(t.Files, ", ")), prompt.KV("Built before yours", strings.Join(t.DependsOn, ", "))).
		Titled("Notes from the reviewer", t.Notes...).
		Para("Build it, run its tests, then call " + ToolHandoff + " with what you ran.")
}

// First message of a builder on a ticket, the brief, the plan, what is already built and its ticket
func builderOpening(b Brief, p Plan, tickets []Ticket, t Ticket) string {
	return briefDoc(b).Add(planHead(p)).Add(builtDoc(tickets)).Add(ticketDoc(t)).String()
}

// Message a builder gets when its next ticket comes in the same session
func builderNext(t Ticket, tickets []Ticket) string {
	return builtDoc(tickets).Add(ticketDoc(t)).String()
}

// Message the reviewer gets with a handoff to judge, the brief and the plan ahead of the first
func reviewOpening(b Brief, p Plan, t Ticket, h Handoff, from string, first bool) string {
	d := prompt.New()
	if first {
		d.Add(briefDoc(b)).Add(planHead(p))
	}
	back := ""
	if t.Round > 0 {
		back = fmt.Sprintf("Sent back %d time(s) before.", t.Round)
	}
	d.Para(prompt.H(1, "Handoff on "+t.ID+" "+t.Title+" from "+from), prompt.KV("Ticket", strings.TrimSpace(t.Description)), prompt.KV("Files owned", strings.Join(t.Files, ", ")), back)
	d.Para(prompt.KV("Status", string(h.Status)), strings.TrimSpace(h.Summary), prompt.KV("Files changed", strings.Join(h.Files, ", ")), prompt.KV("Tested", strings.TrimSpace(h.Tested)))
	if h.Status == HandoffDone {
		return d.Para("Call " + ToolVerdict + " with accepted, rework or failed.").String()
	}
	return d.Para("Call " + ToolVerdict + " with guidance or failed.").String()
}

// Message a builder gets with the verdict on its handoff
func verdictText(d Decision) string {
	switch d.Verdict {
	case VerdictAccepted:
		return d.Ticket + " accepted, the ticket is complete. End your turn with one line."
	case VerdictRework:
		return prompt.New().Para(d.Ticket + " sent back:").Raw(d.Notes).Para("Fix it, check it, then call " + ToolHandoff + " with done again.").String()
	case VerdictGuidance:
		return prompt.New().Para("Reviewer on " + d.Ticket + ":").Raw(d.Notes).Para("Go on with the ticket and call " + ToolHandoff + " when it is done.").String()
	}
	return prompt.New().Para(d.Ticket + " failed: " + d.Notes).Para("Stop work on it and end your turn with one line.").String()
}

// Message the director gets to write the report, why the build stopped short ahead of it when it did
func reportOpening(b Brief, p Plan, tickets []Ticket, cause string) string {
	d := prompt.New()
	if cause != "" {
		d.Para("The sprint stopped short: " + cause)
	}
	d.Raw(briefText(b)).Para(prompt.KV("Plan", p.Title)).Heading(2, "Tickets")
	for _, t := range tickets {
		head := prompt.H(3, t.ID+" "+t.Title+" is "+string(t.Status))
		if t.Builder != "" {
			head += " by " + t.Builder
		}
		lines := []string{head, prompt.KV("Last handoff", strings.TrimSpace(t.Report))}
		for _, n := range t.Notes {
			lines = append(lines, "Reviewer: "+n)
		}
		d.Para(append(lines, prompt.KV("Ended", t.Err))...)
	}
	return d.Para("Write the report for the person in Markdown with these sections: Shipped, Not shipped, How to verify, Left to do. Check what shipped against the acceptance of the brief. Use only what the handoffs and verdicts say, without tool calls.").String()
}

// Steer that tells a member to wrap up after its time ran out
func wrapUp(toolName string, limit time.Duration) string {
	return fmt.Sprintf("Time is up after %s. Call %s now with what you have.", limit.Round(time.Minute), toolName)
}

// Steer that tells a member that ended its turn without calling its tool to call it
func reminder(toolName string) string {
	return "Call " + toolName + " now with what you have. Nothing moves until you do."
}

// Steer that tells the director its last turn held no report
func reportReminder() string {
	return "Your last turn held no report. Write it now in Markdown with the sections Shipped, Not shipped, How to verify, Left to do, from the handoffs and verdicts alone."
}

// Result the director reads once the brief is handed over
func briefTaken(b Brief) string {
	return prompt.New().Para("Brief handed to the team. End your turn with one line on what was settled.").Add(briefDoc(b)).String()
}

// Result the architect reads once the plan is handed over
func planTaken(in planInput) string {
	return prompt.New().Paraf("Plan %q with %d tickets handed to the team. End your turn with one line on the approach.", in.Title, len(in.Tickets)).Raw(planText(in.Plan, in.Tickets)).String()
}

// Result a builder reads once its handoff is taken
func handoffTaken() string {
	return "Handed off to the reviewer. End your turn now with one line on where the ticket stands, the verdict arrives as your next message."
}

// Result the reviewer reads once its verdict is recorded
func verdictTaken(d Decision) string {
	return "Verdict " + string(d.Verdict) + " recorded on " + d.Ticket + ". End your turn with one line on why."
}

// Answers as the director reads them, one block per question
func answersText(qs []harness.Question, answers []harness.Answer) string {
	d := prompt.New()
	for i, q := range qs {
		head := fmt.Sprintf("Q%d: %s", i+1, q.Text)
		if i >= len(answers) || !answers[i].Given() {
			d.Para(head, "No answer, decide it yourself and say what you chose.")
			continue
		}
		a := answers[i]
		d.Para(head, prompt.KV("Picked", strings.Join(a.Picked, ", ")), prompt.KV("In their own words", strings.TrimSpace(a.Text)))
	}
	return d.String()
}

// First line of text, trimmed
func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(line)
}
