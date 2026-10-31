package sprint

import (
	"fmt"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/harness"
)

// One line on each role for the kind picker and the agent tool
func roleDescription(role string) string {
	switch role {
	case RolePM:
		return "sprint project manager, settles the objective with the person and hands it to the planner"
	case RolePlanner:
		return "sprint planner, sizes the story and cuts it into tickets"
	case RoleLead:
		return "sprint lead engineer, briefs the engineers, directs them as they report and reviews the whole"
	case RoleEngineer:
		return "sprint engineer, builds one ticket at a time and reports to the lead"
	}
	return ""
}

// Base instructions of a role, replacing the built in prompt
func roleSystem(role string) string {
	switch role {
	case RolePM:
		return pmSystem
	case RolePlanner:
		return plannerSystem
	case RoleLead:
		return leadSystem
	case RoleEngineer:
		return engineerSystem
	}
	return ""
}

const pmSystem = `You are the project manager of a sprint team working in the user's repository. A person brought a request. Your one job is to turn it into an objective the team can build without coming back with questions.

Work like this:
- Read the request. Look at the repository with the file tools when that answers a question better than asking.
- Ask the person about anything that would change what gets built: scope, behavior at the edges, what done looks like, what must not change. Ask with ` + ToolAsk + `: every question offers two to four concrete options the person picks from, and they may type their own answer instead. Put related questions in one call, up to four. Do not ask what you can find out yourself, and do not ask in plain text.
- When an answer comes back as no answer, decide yourself and record the choice in the notes.
- When the objective is clear, call ` + ToolResolve + ` once with the objective, the acceptance criteria and the constraints. That hands the work to the planner. Do not call it while anything that matters is still open.
- After the sprint ends you will be told what the team built. Report it to the person in a few lines, what was done, what was left, and anything they must check.

You do not write code and you do not plan the work. Keep every message short.`

const plannerSystem = `You are the planner of a sprint team working in the user's repository. You receive an objective the project manager settled with the person. Your job is to size it and cut it into tickets engineers can take in parallel.

Work like this:
- Study the repository with the file tools until you know where the change lands and what it touches.
- The person is not available to you. Where the objective leaves a choice open, settle it from the objective and the code, and state the choice in the ticket it affects.
- Write one story: a title, a summary of the change as a whole, and a size.
- Cut it into tickets. Each ticket is one coherent change one engineer can finish alone, naming the files it owns. Two tickets must not edit the same file; when they would, merge them or make one depend on the other. Keep the count as low as the work allows.
- Name dependencies only where one ticket cannot start before another is done.
- Call ` + ToolPlan + ` once with the story and the tickets. That is your whole output. Do not write code.`

const leadSystem = `You are the lead engineer of a sprint team working in the user's repository. Engineers build the tickets of a story; you oversee them. You read, build and test, and you decide. You do not edit files: when something must change, an engineer changes it on your direction.

Work like this:
- Before the build you get the story and the tickets. Study the repository where they land and write a briefing for the engineers: how the pieces fit, the interfaces between tickets, the order that matters, the mistakes to avoid. End your turn with the briefing as your answer.
- During the build engineers report to you with ` + ToolReport + `. Each report reaches you as a message naming the ticket. Look at the change in the working tree, build and run the tests when the report says done, then answer with ` + ToolDirect + `: accepted when the ticket meets the objective and its report holds up, rework with exactly what is wrong and what done looks like, guidance when the engineer is blocked or asks a question, failed when the ticket cannot be finished. Answer every report before you end your turn.
- After the build you review the whole: build the project, run its tests, check that the tickets fit together and meet the acceptance criteria. Then call ` + ToolReview + ` with a summary and the tickets to send back, if any. When nothing goes back, the summary is what the person reads, so say what was built, how it was verified and what they should know.`

const engineerSystem = `You are an engineer on a sprint team working in the user's repository. You take one ticket at a time from a story other engineers are also building, each in their own files, and you report to the lead.

Work like this:
- Read the story, the briefing of the lead and your ticket. Study the files it names and what they touch before changing anything.
- Build exactly the ticket, in the files it owns. Do not change files that belong to another ticket; when you must, report it as a question instead.
- Run the build and the tests that cover your change before you report.
- Report with ` + ToolReport + `: status question when a choice is not yours to make, blocked when you cannot go on, done when the ticket is built and verified, with what you changed file by file and how you verified it. The call returns the direction of the lead: follow it. Rework means change what the notes say and report done again.
- Your ticket is over when the lead answers accepted or failed. Then end your turn with one line. Never end your turn before that.`

// First message of the project manager, the request and how the handoff works
func pmOpening(prompt string, interactive bool) string {
	var sb strings.Builder
	sb.WriteString("A person brought this request for the sprint team:\n\n")
	sb.WriteString(strings.TrimSpace(prompt))
	sb.WriteString("\n\n")
	if interactive {
		sb.WriteString("Ask them what you need to know with " + ToolAsk + ", then call " + ToolResolve + " when the objective is settled.")
	} else {
		sb.WriteString("Nobody is available to answer questions. Settle the objective from the request and the repository alone, make reasonable choices where it is silent and note them, then call " + ToolResolve + ".")
	}
	return sb.String()
}

// Reminder sent when a role ended its turn without the call the stage waits on
func nudge(toolName string) string {
	return "The sprint cannot go on until you call " + toolName + ". Call it now with what you have."
}

// Steer sent to a role whose stage ran past its limit
func wrapUp(toolName string, limit time.Duration) string {
	return fmt.Sprintf("Your time limit of %s has passed. Stop exploring, finish with what you know and call %s now.", span(limit), toolName)
}

// Steer sent to the lead whose briefing ran past its limit
func wrapUpBriefing(limit time.Duration) string {
	return fmt.Sprintf("Your time limit of %s has passed. Stop exploring and end your turn with the briefing now.", span(limit))
}

// Message that starts a role again after the person told a waiting sprint to go on
func goOn(toolName string) string {
	return "Go on from where you were and call " + toolName + " when you are ready."
}

// Steer sent to an engineer whose build round ran past its limit
func wrapUpReport(limit time.Duration) string {
	return fmt.Sprintf("The build limit of %s has passed. Finish what is in progress, leave the tree building, and report done with %s now.", span(limit), ToolReport)
}

// Steer sent to the lead once the build round ran past its limit
func wrapUpDirect(limit time.Duration) string {
	return fmt.Sprintf("The build limit of %s has passed. Engineers are reporting what they have. Answer every report with %s as accepted or failed, rework is closed for this round.", span(limit), ToolDirect)
}

// Story with every ticket in full, as the plan reads in a transcript
func planText(s Story, tickets []Ticket) string {
	var sb strings.Builder
	sb.WriteString(storyText(s, tickets))
	for _, t := range tickets {
		sb.WriteString("\n")
		sb.WriteString(ticketText(t))
		if len(t.DependsOn) > 0 {
			sb.WriteString("Depends on: ")
			sb.WriteString(strings.Join(t.DependsOn, ", "))
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// Summary and tickets sent back of a review, as it reads in a transcript
func reviewText(in reviewInput) string {
	var sb strings.Builder
	if len(in.Rework) > 0 {
		sb.WriteString("## Sent back\n")
		for _, back := range in.Rework {
			fmt.Fprintf(&sb, "- %s: %s\n", back.Ticket, back.Notes)
		}
		sb.WriteString("\n")
	}
	sb.WriteString("## Summary\n")
	sb.WriteString(strings.TrimSpace(in.Summary))
	sb.WriteString("\n")
	return sb.String()
}

// Questions with the answers the person gave, as the asking role reads them
func answersText(qs []harness.Question, answers []harness.Answer) string {
	var sb strings.Builder
	sb.WriteString("Answers from the person:\n")
	for i, q := range qs {
		fmt.Fprintf(&sb, "%d. %s\n", i+1, q.Text)
		if i >= len(answers) || !answers[i].Given() {
			sb.WriteString("   No answer. Decide this yourself and record the choice in your notes.\n")
			continue
		}
		a := answers[i]
		for _, label := range a.Picked {
			sb.WriteString("   Picked: " + label)
			for _, c := range q.Choices {
				if c.Label == label && c.Description != "" {
					sb.WriteString(", " + c.Description)
				}
			}
			sb.WriteString("\n")
		}
		if t := strings.TrimSpace(a.Text); t != "" {
			sb.WriteString("   In their own words: " + t + "\n")
		}
	}
	return sb.String()
}

// Objective as every later role reads it
func objectiveText(o Objective) string {
	var sb strings.Builder
	sb.WriteString("# Objective\n\n")
	sb.WriteString(strings.TrimSpace(o.Objective))
	sb.WriteString("\n")
	if len(o.Acceptance) > 0 {
		sb.WriteString("\n## Acceptance\n")
		for _, a := range o.Acceptance {
			sb.WriteString("- ")
			sb.WriteString(a)
			sb.WriteString("\n")
		}
	}
	if len(o.Constraints) > 0 {
		sb.WriteString("\n## Constraints\n")
		for _, c := range o.Constraints {
			sb.WriteString("- ")
			sb.WriteString(c)
			sb.WriteString("\n")
		}
	}
	if o.Notes != "" {
		sb.WriteString("\n## Notes from the project manager\n")
		sb.WriteString(strings.TrimSpace(o.Notes))
		sb.WriteString("\n")
	}
	return sb.String()
}

// Opening message of the planner
func plannerOpening(o Objective, maxTickets int) string {
	return objectiveText(o) + fmt.Sprintf("\nSize this and cut it into at most %d tickets, then call %s. Settle every open choice yourself and state it in the ticket it affects.", maxTickets, ToolPlan)
}

// Story as the engineers and the lead read it
func storyText(s Story, tickets []Ticket) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# Story: %s\n\n%s\n", s.Title, strings.TrimSpace(s.Summary))
	if s.Size != "" {
		fmt.Fprintf(&sb, "\nSize: %s\n", s.Size)
	}
	sb.WriteString("\n## Tickets\n")
	for _, t := range tickets {
		fmt.Fprintf(&sb, "- %s %s", t.ID, t.Title)
		if len(t.Files) > 0 {
			fmt.Fprintf(&sb, " (%s)", strings.Join(t.Files, ", "))
		}
		if len(t.DependsOn) > 0 {
			fmt.Fprintf(&sb, " after %s", strings.Join(t.DependsOn, ", "))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// Full text of one ticket
func ticketText(t Ticket) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# Ticket %s: %s\n\n%s\n", t.ID, t.Title, strings.TrimSpace(t.Description))
	if len(t.Files) > 0 {
		sb.WriteString("\nFiles: ")
		sb.WriteString(strings.Join(t.Files, ", "))
		sb.WriteString("\n")
	}
	if t.Estimate != "" {
		sb.WriteString("Estimate: ")
		sb.WriteString(t.Estimate)
		sb.WriteString("\n")
	}
	return sb.String()
}

// Message that hands the plan to the lead before the build and asks for the briefing
func leadBriefOpening(o Objective, s Story, tickets []Ticket, engineers int) string {
	var sb strings.Builder
	sb.WriteString(objectiveText(o))
	sb.WriteString("\n")
	sb.WriteString(planText(s, tickets))
	fmt.Fprintf(&sb, "\n%d engineers start on these tickets once you have briefed them. Study the repository where the tickets land, then end your turn with the briefing: how the pieces fit, the interfaces between tickets, the order that matters and the mistakes to avoid. Write nothing but the briefing in your answer.", engineers)
	return sb.String()
}

// Message handing a ticket to an engineer with the briefing, the reports it builds on and the notes of the lead
func engineerOpening(o Objective, s Story, briefing string, all []Ticket, t Ticket, deps []Ticket) string {
	var sb strings.Builder
	sb.WriteString(objectiveText(o))
	sb.WriteString("\n")
	sb.WriteString(storyText(s, all))
	if strings.TrimSpace(briefing) != "" {
		sb.WriteString("\n## Briefing from the lead\n")
		sb.WriteString(strings.TrimSpace(briefing))
		sb.WriteString("\n")
	}
	sb.WriteString("\n")
	sb.WriteString(ticketText(t))
	for _, d := range deps {
		fmt.Fprintf(&sb, "\n## Report on %s %s, which this ticket builds on\n%s\n", d.ID, d.Title, strings.TrimSpace(d.Report))
	}
	if len(t.Notes) > 0 {
		sb.WriteString("\n## Notes from the lead\n")
		for _, n := range t.Notes {
			sb.WriteString("- ")
			sb.WriteString(n)
			sb.WriteString("\n")
		}
		sb.WriteString("\nAddress every note.\n")
	}
	fmt.Fprintf(&sb, "\nBuild this ticket now. Report to the lead with %s and follow the direction it returns.", ToolReport)
	return sb.String()
}

// Plan as a lead going on in a fresh session reads it ahead of the reports
func leadContext(snap Snapshot) string {
	var sb strings.Builder
	sb.WriteString("You are the lead of this sprint and go on in a fresh session. Here is the plan you oversee.\n\n")
	sb.WriteString(objectiveText(snap.Objective))
	sb.WriteString("\n")
	sb.WriteString(storyText(snap.Story, snap.Tickets))
	if strings.TrimSpace(snap.Briefing) != "" {
		sb.WriteString("\n## Your briefing to the engineers\n")
		sb.WriteString(strings.TrimSpace(snap.Briefing))
		sb.WriteString("\n")
	}
	return sb.String()
}

// Message that brings the reports of engineers to the lead
func reportsText(reports []*report) string {
	var sb strings.Builder
	for _, r := range reports {
		fmt.Fprintf(&sb, "## Report from %s on ticket %s, %s\n%s\n\n", r.Engineer, r.Ticket, r.Status, strings.TrimSpace(r.Text))
	}
	if len(reports) == 1 {
		fmt.Fprintf(&sb, "%s waits for your direction. Check the work, then answer with %s.", reports[0].Engineer, ToolDirect)
	} else {
		fmt.Fprintf(&sb, "These engineers wait for your direction. Check the work, then answer each report with %s.", ToolDirect)
	}
	return sb.String()
}

// Reminder sent to a lead that ended its turn with reports still waiting
func nudgeDirect(reports []*report) string {
	return fmt.Sprintf("Engineers still wait for your direction on %s. Answer each with %s before you end your turn.", strings.Join(reportIDs(reports), ", "), ToolDirect)
}

// Direction of the lead as the engineer reads it from its report call
func directionText(ticket string, d direction) string {
	switch d.Verdict {
	case VerdictAccepted:
		return fmt.Sprintf("The lead accepted ticket %s. Your work on it is complete, end your turn with one line.", ticket)
	case VerdictFailed:
		return fmt.Sprintf("The lead failed ticket %s: %s\nStop work on it and end your turn with one line.", ticket, d.Notes)
	case VerdictRework:
		return fmt.Sprintf("The lead sent ticket %s back for rework: %s\nChange what the notes say, verify it, then report done again with %s.", ticket, d.Notes, ToolReport)
	}
	return fmt.Sprintf("Guidance from the lead on ticket %s: %s\nGo on with the ticket and report done with %s when it is built and verified.", ticket, d.Notes, ToolReport)
}

// Message that asks the lead to review a build round as a whole
func reviewOpening(o Objective, s Story, tickets []Ticket, round int) string {
	var sb strings.Builder
	sb.WriteString(objectiveText(o))
	sb.WriteString("\n")
	sb.WriteString(storyText(s, tickets))
	fmt.Fprintf(&sb, "\nBuild round %d is over. Where every ticket stands:\n", round+1)
	for _, t := range tickets {
		fmt.Fprintf(&sb, "\n### %s %s, by %s, %s\n", t.ID, t.Title, t.Engineer, t.Status)
		if t.Err != "" {
			fmt.Fprintf(&sb, "The attempt failed: %s\n", t.Err)
		}
		if strings.TrimSpace(t.Report) != "" {
			sb.WriteString(strings.TrimSpace(t.Report))
			sb.WriteString("\n")
		}
	}
	sb.WriteString("\nReview the whole: build the project, run its tests and check the tickets fit together and meet the acceptance criteria. Then call " + ToolReview + " with the summary and the tickets to send back, if any.")
	return sb.String()
}

// Message that tells the project manager how the sprint ended, with the failure that ended it when one did
func pmClosing(summary string, accepted, unresolved []Ticket, failure error) string {
	var sb strings.Builder
	if failure != nil {
		sb.WriteString("The sprint stopped before it was done: ")
		sb.WriteString(failure.Error())
		sb.WriteString(". Report to the person what was done, what was left and what they can do next, in a few lines.\n")
	} else {
		sb.WriteString("The sprint has ended. Report the outcome to the person.\n")
	}
	if strings.TrimSpace(summary) != "" {
		sb.WriteString("\n## Lead summary\n")
		sb.WriteString(strings.TrimSpace(summary))
		sb.WriteString("\n")
	}
	if len(accepted) > 0 {
		sb.WriteString("\n## Accepted tickets\n")
		for _, t := range accepted {
			fmt.Fprintf(&sb, "- %s %s\n", t.ID, t.Title)
		}
	}
	if len(unresolved) > 0 {
		sb.WriteString("\n## Tickets not accepted\n")
		for _, t := range unresolved {
			fmt.Fprintf(&sb, "- %s %s, %s", t.ID, t.Title, t.Status)
			if t.Err != "" {
				sb.WriteString(": ")
				sb.WriteString(t.Err)
			} else if len(t.Notes) > 0 {
				sb.WriteString(": ")
				sb.WriteString(t.Notes[len(t.Notes)-1])
			}
			sb.WriteString("\n")
		}
	}
	return sb.String()
}
