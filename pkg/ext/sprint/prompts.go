package sprint

import (
	"fmt"
	"strings"
	"time"
)

// One line on each role for the kind picker and the agent tool
func roleDescription(role string) string {
	switch role {
	case RolePM:
		return "sprint project manager, settles the objective with the person and hands it to the planner"
	case RolePlanner:
		return "sprint planner, sizes the story and cuts it into tickets"
	case RoleLead:
		return "sprint lead engineer, reviews and integrates what the engineers built"
	case RoleEngineer:
		return "sprint engineer, builds one ticket at a time"
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
- Ask the person about anything that would change what gets built: scope, behavior at the edges, what done looks like, what must not change. Ask the questions that matter, a few at a time, in plain words. Do not ask what you can find out yourself.
- When the objective is clear, call ` + ToolResolve + ` once with the objective, the acceptance criteria and the constraints. That hands the work to the planner. Do not call it while anything that matters is still open.
- After the sprint ends you will be told what the team built. Report it to the person in a few lines, what was done, what was left, and anything they must check.

You do not write code and you do not plan the work. Keep every message short.`

const plannerSystem = `You are the planner of a sprint team working in the user's repository. You receive an objective the project manager settled with the person. Your job is to size it and cut it into tickets engineers can take in parallel.

Work like this:
- Study the repository with the file tools until you know where the change lands and what it touches.
- Write one story: a title, a summary of the change as a whole, and a size.
- Cut it into tickets. Each ticket is one coherent change one engineer can finish alone, naming the files it owns. Two tickets must not edit the same file; when they would, merge them or make one depend on the other. Keep the count as low as the work allows.
- Name dependencies only where one ticket cannot start before another is done.
- Call ` + ToolPlan + ` once with the story and the tickets. That is your whole output. Do not write code.`

const leadSystem = `You are the lead engineer of a sprint team working in the user's repository. Engineers each built a ticket of a story and reported back. Your job is to make the whole hold together and decide what is done.

Work like this:
- Read the story, the tickets and every report. Look at the changes in the working tree, build the project and run its tests.
- Fix integration problems yourself when they are small: a missed import, a name that drifted, a test that needs the other ticket.
- For each ticket decide accepted or rework. Rework means the engineer must change it, and your note says exactly what is wrong and what done looks like.
- Call ` + ToolReview + ` once with a verdict for every ticket and a summary of the state of the work. When every ticket is accepted the summary is what the person reads, so say what was built, how it was verified and what they should know.`

const engineerSystem = `You are an engineer on a sprint team working in the user's repository. You take one ticket at a time from a story other engineers are also building, each in their own files.

Work like this:
- Read the story and your ticket. Study the files it names and what they touch before changing anything.
- Build exactly the ticket, in the files it owns. Do not change files that belong to another ticket; when you must, say so in your report instead.
- Run the build and the tests that cover your change before you report.
- End with a short report: what you changed, file by file, how you verified it, and anything the lead must know, such as an interface another ticket relies on.`

// First message of the project manager, the request and how the handoff works
func pmOpening(prompt string, interactive bool) string {
	var sb strings.Builder
	sb.WriteString("A person brought this request for the sprint team:\n\n")
	sb.WriteString(strings.TrimSpace(prompt))
	sb.WriteString("\n\n")
	if interactive {
		sb.WriteString("Ask them what you need to know, then call " + ToolResolve + " when the objective is settled.")
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

// Message that starts a role again after the person told a waiting sprint to go on
func goOn(toolName string) string {
	return "Go on from where you were and call " + toolName + " when you are ready."
}

// Steer sent to an engineer whose build round ran past its limit
func wrapUpReport(limit time.Duration) string {
	return fmt.Sprintf("The build limit of %s has passed. Finish what is in progress, leave the tree building, and end with your report now.", span(limit))
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

// Verdicts and summary of a review, as it reads in a transcript
func reviewText(in reviewInput) string {
	var sb strings.Builder
	sb.WriteString("## Verdicts\n")
	for _, v := range in.Verdicts {
		fmt.Fprintf(&sb, "- %s %s", v.Ticket, v.Status)
		if v.Notes != "" {
			sb.WriteString(": ")
			sb.WriteString(v.Notes)
		}
		sb.WriteString("\n")
	}
	sb.WriteString("\n## Summary\n")
	sb.WriteString(strings.TrimSpace(in.Summary))
	sb.WriteString("\n")
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
	return objectiveText(o) + fmt.Sprintf("\nSize this and cut it into at most %d tickets, then call %s.", maxTickets, ToolPlan)
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

// Message that hands a ticket to an engineer, with the reports of the tickets it depends on and any review notes
func engineerOpening(o Objective, s Story, all []Ticket, t Ticket, deps []Ticket) string {
	var sb strings.Builder
	sb.WriteString(objectiveText(o))
	sb.WriteString("\n")
	sb.WriteString(storyText(s, all))
	sb.WriteString("\n")
	sb.WriteString(ticketText(t))
	for _, d := range deps {
		fmt.Fprintf(&sb, "\n## Report on %s %s, which this ticket builds on\n%s\n", d.ID, d.Title, strings.TrimSpace(d.Report))
	}
	if len(t.Notes) > 0 {
		sb.WriteString("\n## Review notes from the lead\n")
		for _, n := range t.Notes {
			sb.WriteString("- ")
			sb.WriteString(n)
			sb.WriteString("\n")
		}
		sb.WriteString("\nAddress every note, then report again.\n")
	}
	sb.WriteString("\nBuild this ticket now and end with your report.")
	return sb.String()
}

// Message that hands the built tickets to the lead
func leadOpening(o Objective, s Story, tickets []Ticket, round int) string {
	var sb strings.Builder
	sb.WriteString(objectiveText(o))
	sb.WriteString("\n")
	sb.WriteString(storyText(s, tickets))
	if round > 0 {
		fmt.Fprintf(&sb, "\nThis is review round %d. Tickets sent back came back with new reports.\n", round+1)
	}
	sb.WriteString("\n## Reports\n")
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
	sb.WriteString("\nReview the work, fix what is small, and call " + ToolReview + " with a verdict for every ticket.")
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
