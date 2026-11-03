package prompt

import (
	"fmt"
	"strings"
	"time"
)

// What a system prompt says, rendered in this order from the parts set
type System struct {
	// Opening sentences, who the model is and what it does
	Identity []string
	// Rules put right after the identity
	Rules []string
	// Section on the surface the model acts through, nil leaving it out
	Harness *Harness
	// Section on carrying out work, nil leaving it out
	Delivery *Delivery
	// Section on writing for the reader, nil leaving it out
	Writing *Writing
	// Further sections in order
	Sections []Section
	// Rules put last
	Closing []string
}

// Titled part of a system prompt, paragraphs of text then rules
type Section struct {
	Title string
	Text  []string
	Rules []string
}

func (s System) String() string {
	d := New().Para(Sentences(s.Identity...)).Bullets(s.Rules...)
	if s.Harness != nil {
		d.Add(s.Harness.doc())
	}
	if s.Delivery != nil {
		d.Add(s.Delivery.doc())
	}
	if s.Writing != nil {
		d.Add(s.Writing.doc())
	}
	for _, sec := range s.Sections {
		d.Add(sec.doc())
	}
	return d.Bullets(s.Closing...).String()
}

func (s Section) doc() *Doc {
	d := New().Heading(1, s.Title)
	for _, p := range s.Text {
		d.Raw(p)
	}
	return d.Bullets(s.Rules...)
}

// What the model is told about the harness it acts through, each part left out while blank or false
type Harness struct {
	// Who sees the text written outside tool calls, such as the user
	Reader string
	// Who speaks through user messages, such as the user or the sprint
	Speaker string
	// Who chooses the permission gate every call passes, empty leaving the gate unmentioned
	Gatekeeper string
	// Tool names offered, the guide covering each one it knows in this order
	Tools []string
	// Checkout shared with others editing at the same time, so git leaves the tree and the index alone
	SharedTree bool
	// Tool whose call alone ends the stage, so a turn ending without it only earns a reminder
	Must string
	// Further rules
	Extra []string
}

func (h Harness) doc() *Doc {
	var rules []string
	if h.Reader != "" || h.Speaker != "" {
		rules = append(rules, Sentences(
			when(h.Reader != "", "Text you write outside a tool call is shown to "+h.Reader+" as markdown in a terminal."),
			"Tool results arrive as tool messages, never as user messages. After a tool result, continue the task.",
			when(h.Speaker != "", Cap(h.Speaker)+" speaks only through user messages.")))
	}
	if h.Gatekeeper != "" {
		rules = append(rules, "Every tool call passes a permission gate chosen by "+h.Gatekeeper+". A denied call means "+h.Gatekeeper+" declined it. Adjust the approach, never retry the same call.")
	}
	rules = append(rules, Guide(h.Tools, h.SharedTree)...)
	rules = append(rules, "Independent tool calls may be made together in one turn. Dependent calls wait for the earlier result.")
	if h.Reader != "" {
		rules = append(rules, "Reference code as path:line so "+h.Reader+" can open it.")
	}
	if h.SharedTree {
		rules = append(rules, "You work in one checkout beside others editing at the same time. Never run a git command that changes the tree or the index, and leave files outside your own work as they are.")
	}
	if h.Must != "" {
		rules = append(rules, "Your stage ends only when you call "+h.Must+". A turn that ends without it earns a reminder, nothing more.")
	}
	return New().Heading(1, "Harness").Bullets(append(rules, h.Extra...)...)
}

// One line guide of a tool by name, the file tools gathered into one sentence
type guide struct {
	name string
	use  string
}

// File tools in the order the guide names them, each use completing its name
var fileGuides = []guide{
	{"Read", "shows a file or several at once through file_paths"},
	{"Edit", "changes part of a file or several parts at once through edits"},
	{"Write", "replaces a whole file"},
	{"Glob", "finds files"},
	{"Grep", "searches contents"},
	{"Outline", "lists the definitions of a file or tree with line numbers"},
}

// Tools with a guide of their own, in the order the guide lists them
var otherGuides = []guide{
	{"Shell", "Use Shell for commands, builds and tests%s. Shell keeps its working directory between calls. A pipeline exits with the code of its last command. The exit code of every stage is listed whenever an earlier stage failed, so read that line before trusting a piped test run. A grep or diff that found nothing exits 1 without being flagged."},
	{"ShellOutput", "ShellOutput reads what a background job of Shell printed since the last call, with whether it still runs."},
	{"KillShell", "KillShell stops a background job of Shell."},
	{"WebFetch", "WebFetch reads a page, answering a question about it when one is given."},
	{"Record", "Record keeps a conclusion a person wants after the session, such as a bug found, a risk, a decision or an open question. Routine command results and progress belong in prose. Working notes, drafted code, and temporary files go in the scratch directory named in the environment."},
	{"Plan", "Plan holds the ordered steps of the work, on the attached mission or on this session. Call it before work that takes more than a few tool calls and whenever a step changes state, keeping one step active."},
	{"Agent", "Agent delegates a self contained task to a child with its own transcript, of a named kind when one fits. Give it every detail it needs, since it starts empty."},
	{"Skill", "Skill loads the instructions of a listed skill into the conversation. Load the skill that covers the task before doing the work another way."},
}

// Every tool name the guide knows, in guide order
func Builtins() []string {
	out := make([]string, 0, len(fileGuides)+len(otherGuides))
	for _, g := range fileGuides {
		out = append(out, g.name)
	}
	for _, g := range otherGuides {
		out = append(out, g.name)
	}
	return out
}

// Names with the dropped ones left out, in their order
func Without(names, dropped []string) []string {
	var out []string
	for _, n := range names {
		keep := true
		for _, d := range dropped {
			keep = keep && n != d
		}
		if keep {
			out = append(out, n)
		}
	}
	return out
}

// Guide rules for the tools named, in guide order, names the guide does not know left out
func Guide(tools []string, sharedTree bool) []string {
	has := func(name string) bool {
		for _, t := range tools {
			if t == name {
				return true
			}
		}
		return false
	}
	var out []string
	var files []string
	for _, g := range fileGuides {
		if has(g.name) {
			files = append(files, g.name+" "+g.use)
		}
	}
	if len(files) > 0 {
		out = append(out, "Prefer the dedicated file tools. "+strings.Join(files, ", ")+".")
	}
	for _, g := range otherGuides {
		if !has(g.name) {
			continue
		}
		use := g.use
		if g.name == "Shell" {
			git := " and git"
			if sharedTree {
				git = ""
			}
			use = fmt.Sprintf(use, git)
		}
		out = append(out, use)
	}
	return out
}

// How the model carries out work, who it may ask and who confirms what is hard to undo
type Delivery struct {
	// Who can be asked when readings of the task differ, empty when nobody can
	Asked string
	// Who confirms actions that are hard to reverse or reach outside the machine, empty when nobody can
	Confirmer string
	// Further sentences
	Extra []string
}

func (v Delivery) doc() *Doc {
	ask := "Make routine judgment calls yourself. Nobody can be asked, so settle open choices and say what you chose."
	if v.Asked != "" {
		ask = "Make routine judgment calls yourself and ask " + v.Asked + " only when different readings lead to materially different work."
	}
	confirm := ""
	if v.Confirmer != "" {
		confirm = "Confirm with " + v.Confirmer + " before actions that are hard to reverse or that reach outside the machine, such as deleting files, force pushing or sending messages, unless " + v.Confirmer + " already said to proceed."
	}
	text := Sentences(append([]string{
		"Do the work as asked, acting on the actual request rather than guesses about what lies behind it. The requested scope is the deliverable, so neither narrow nor widen it quietly.",
		ask,
		"When you have enough information to act, act. Finish the whole task and report completion only when it is fully done. If part of the task is blocked, finish every other part and say exactly what was left and why.",
		confirm,
		"Report outcomes faithfully. If tests fail, say so with the output. If a step was skipped, say that.",
	}, v.Extra...)...)
	return New().Heading(1, "Delivering work").Para(text)
}

// How the model writes for whoever reads its prose
type Writing struct {
	// Who reads the prose, such as the user
	Reader string
}

func (w Writing) doc() *Doc {
	rules := []string{
		"Lead with the answer or outcome. If something could not be verified, say so first.",
		"One idea per sentence, about twenty words, with a verb. No em dashes, no parentheticals.",
		"State facts and conclusions. Do not narrate your own reasoning and do not announce what you are about to do before every call.",
		"Keep code out of prose. Name a file, function or flag only when the reader has to go there. Commands, snippets and error text go in a fenced code block.",
		"Use a list for parallel items such as findings, steps or files. One or two sentences per item.",
		"No headers in a message under about five hundred words.",
		"Stop when the content stops. No closing offer, no restating what you did.",
	}
	return New().Heading(1, "Writing for "+w.Reader).Para(Cap(w.Reader) + " may not see your tool calls or their results. Only your prose reaches them, so it has to stand on its own.").Bullets(rules...)
}

// Facts about the machine and session a run is told in its environment block
type Facts struct {
	Dir      string
	Git      bool
	Platform string
	Shell    string
	Date     time.Time
	Model    string
	// Name of the plan tool the run may call, empty when it has none
	PlanTool string
	// A mission is attached, so the plan tool updates its plan
	Mission bool
	// Plan kept on the session, as the plan tool reports it
	Plan string
	// Directory created for the session to hold working notes and temporary files
	Scratch string
	// Name of the record tool the run may call, empty when it has none
	RecordTool string
}

// Environment block of a run, one line per fact set
func Environment(f Facts) string {
	lines := []string{
		KV("Working directory", f.Dir),
		fmt.Sprintf("Git repository: %t", f.Git),
		KV("Platform", f.Platform),
		KV("Shell", f.Shell),
	}
	if !f.Date.IsZero() {
		lines = append(lines, KV("Today's date", f.Date.Format("2006-01-02")))
	}
	lines = append(lines, KV("Model", f.Model))
	if f.PlanTool != "" && f.Mission {
		lines = append(lines, "A mission is attached, so "+f.PlanTool+" updates its plan")
	}
	if f.PlanTool != "" && f.Plan != "" {
		lines = append(lines, "Plan kept on this session, which "+f.PlanTool+" replaces:\n"+indent(f.Plan, "  "))
	}
	if f.Scratch != "" {
		lines = append(lines, "Scratch directory: "+f.Scratch+", created for this session, for working notes and temporary files that do not belong in the project")
	}
	if f.RecordTool != "" {
		lines = append(lines, f.RecordTool+" is available for findings worth keeping")
	}
	return New().Heading(1, "Environment").Bullets(lines...).String()
}

// Text when the condition holds, empty otherwise
func when(cond bool, text string) string {
	if cond {
		return text
	}
	return ""
}

// Every line of the text led by the prefix
func indent(text, prefix string) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}
