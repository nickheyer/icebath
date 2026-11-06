package skills

import (
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Namespace of one icebath bundles command line, the action empty when no subcommand was given
type BundlesArgs struct {
	Action      string
	Name        string
	Skill       []string
	Description string
	Instruction string
	Force       bool
}

// Bundles command runner over a scope, printing to Out and reading skill names through LineInput
type BundlesCLI struct {
	Scope     *Scope
	Out       io.Writer
	LineInput func(prompt string) (string, error)
}

// Runner over the scope writing to out and reading from the default terminal
func NewBundlesCLI(s *Scope, out io.Writer) *BundlesCLI {
	return &BundlesCLI{Scope: s, Out: out, LineInput: func(prompt string) (string, error) { return cliui.Default().LineInput(prompt) }}
}

// Fresh rich console over the command output
func (b *BundlesCLI) console() *pyrich.Console { return pyrich.NewConsole(b.Out) }

// Prints the message and returns exit status one
func bundlesFail(c *pyrich.Console, message string) int {
	c.Print(message)
	return 1
}

// Python exception class and str of a bundle CRUD error
func bundleErrorClass(err error) (string, string) {
	var be *BundleError
	if errors.As(err, &be) {
		return be.Class, be.Msg
	}
	return pypath.OSErrorClass(err), pypath.OSErrorText(err)
}

// Bundle info value as its Python str, empty when falsy
func bundleField(info map[string]any, key string) string {
	if v := info[key]; pyrepr.Truthy(v) {
		return pyrepr.Str(v)
	}
	return ""
}

// Prints the installed bundles table or the empty directory hint
func (b *BundlesCLI) cmdList() (int, error) {
	c := b.console()
	bundles, err := b.Scope.ListBundles()
	if err != nil {
		return 0, err
	}
	if len(bundles) == 0 {
		c.Print("[dim]No bundles installed yet. Create one with:\n  icebath bundles create <name> --skill skill1 --skill skill2[/]\nBundles directory: [bold]" + b.Scope.BundlesDir() + "[/]")
		return 0, nil
	}
	t := &pyrich.Table{Title: fmt.Sprintf("Skill Bundles (%d)", len(bundles)), Columns: []pyrich.Column{
		{Header: "Command", Style: "bold cyan"}, {Header: "Name", Style: "bold"}, {Header: "Skills", Justify: "right"}, {Header: "Description"}}}
	for _, info := range bundles {
		members, _ := info["skills"].([]any)
		t.AddRow("/"+pyrepr.Str(info["slug"]), pyrepr.Str(info["name"]), strconv.Itoa(len(members)), bundleField(info, "description"))
	}
	c.PrintTable(t)
	c.Print("\n[dim]Bundles directory: " + b.Scope.BundlesDir() + "[/]")
	return 0, nil
}

// Prints one bundle's command, file, skills and instruction, status one when it is unknown
func (b *BundlesCLI) cmdShow(a BundlesArgs) (int, error) {
	c := b.console()
	info, ok, err := b.Scope.GetBundle(a.Name)
	if err != nil {
		return 0, err
	}
	if !ok {
		return bundlesFail(c, "[bold red]Bundle "+pyrepr.StrRepr(a.Name)+" not found.[/]"), nil
	}
	members, _ := info["skills"].([]any)
	c.Print("[bold cyan]/" + pyrepr.Str(info["slug"]) + "[/]  [bold]" + pyrepr.Str(info["name"]) + "[/]")
	if desc := bundleField(info, "description"); desc != "" {
		c.Print("  " + desc)
	}
	c.Print("  [dim]File: " + pyrepr.Str(info["path"]) + "[/]")
	c.Print(fmt.Sprintf("  [bold]Skills (%d):[/]", len(members)))
	for _, s := range members {
		c.Print("    - " + pyrepr.Str(s))
	}
	if instruction := bundleField(info, "instruction"); instruction != "" {
		c.Print("  [bold]Instruction:[/]\n    " + instruction)
	}
	return 0, nil
}

// Saves a bundle from the flag skills or skill names read one per line, status one on refusal
func (b *BundlesCLI) cmdCreate(a BundlesArgs) (int, error) {
	c := b.console()
	skills := append([]string(nil), a.Skill...)
	if len(skills) == 0 {
		c.Print("[dim]No skills passed via --skill. Enter one skill name per line.\nSubmit an empty line to finish.[/]")
		for {
			line, err := b.LineInput("skill> ")
			if errors.Is(err, cliui.ErrEOF) || errors.Is(err, cliui.ErrInterrupt) {
				return bundlesFail(c, "\n[yellow]Cancelled.[/]"), nil
			}
			if err != nil {
				return 0, err
			}
			if line = pystr.Strip(line); line == "" {
				break
			}
			skills = append(skills, line)
		}
	}
	if len(skills) == 0 {
		return bundlesFail(c, "[bold red]A bundle must reference at least one skill.[/]"), nil
	}
	path, err := b.Scope.SaveBundle(a.Name, skills, a.Description, a.Instruction, a.Force)
	if err != nil {
		switch class, msg := bundleErrorClass(err); class {
		case "FileExistsError":
			return bundlesFail(c, "[bold red]"+msg+"[/]\n[dim]Pass --force to overwrite.[/]"), nil
		case "ValueError":
			return bundlesFail(c, "[bold red]"+msg+"[/]"), nil
		}
		return 0, err
	}
	c.Print("[bold green]Created bundle:[/] " + path)
	info, ok, err := b.Scope.GetBundle(a.Name)
	if err != nil {
		return 0, err
	}
	if ok {
		members, _ := info["skills"].([]any)
		c.Print(fmt.Sprintf("  Invoke with: [bold cyan]/%s[/]  (loads %d skills)", pyrepr.Str(info["slug"]), len(members)))
	}
	return 0, nil
}

// Deletes a bundle by name, status one when no bundle file exists
func (b *BundlesCLI) cmdDelete(a BundlesArgs) (int, error) {
	c := b.console()
	path, err := b.Scope.DeleteBundle(a.Name)
	if err != nil {
		if class, msg := bundleErrorClass(err); class == "FileNotFoundError" {
			return bundlesFail(c, "[bold red]"+msg+"[/]"), nil
		}
		return 0, err
	}
	c.Print("[bold green]Deleted bundle:[/] " + path)
	return 0, nil
}

// Rescans the bundles dir and prints the added and removed bundles with the new total
func (b *BundlesCLI) cmdReload() (int, error) {
	c := b.console()
	diff, err := b.Scope.ReloadBundles()
	if err != nil {
		return 0, err
	}
	added, _ := diff["added"].([]map[string]any)
	removed, _ := diff["removed"].([]map[string]any)
	if len(added) > 0 {
		c.Print(fmt.Sprintf("[bold green]Added (%d):[/]", len(added)))
		for _, entry := range added {
			c.Print("  + " + pyrepr.Str(entry["name"]) + " — " + pyrepr.Str(entry["description"]))
		}
	}
	if len(removed) > 0 {
		c.Print(fmt.Sprintf("[bold red]Removed (%d):[/]", len(removed)))
		for _, entry := range removed {
			c.Print("  - " + pyrepr.Str(entry["name"]))
		}
	}
	if len(added) == 0 && len(removed) == 0 {
		c.Print(fmt.Sprintf("[dim]No changes. %s bundle(s) loaded.[/]", pyrepr.Str(diff["total"])))
	} else {
		c.Print(fmt.Sprintf("[dim]Total bundles now: %s[/]", pyrepr.Str(diff["total"])))
	}
	return 0, nil
}

// Dispatches one bundles command line, listing when no subcommand handler applies
func (b *BundlesCLI) BundlesCommand(a BundlesArgs) (int, error) {
	switch a.Action {
	case "show":
		return b.cmdShow(a)
	case "create":
		return b.cmdCreate(a)
	case "delete":
		return b.cmdDelete(a)
	case "reload":
		return b.cmdReload()
	}
	return b.cmdList()
}
