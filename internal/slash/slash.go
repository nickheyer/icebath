package slash

import (
	"context"
	"slices"
	"strings"
)

type Command struct {
	Name    string
	Aliases []string
	Usage   string
	Help    string
	Run     func(ctx context.Context, call *Call) (string, error)
}

type Call struct {
	SessionID string
	Argument  string
}

type Registrant interface {
	Commands() []Command
}

type Registry struct {
	registrants []Registrant
}

func (r *Registry) Register(registrant Registrant) {
	r.registrants = append(r.registrants, registrant)
}

func (r *Registry) Commands() []Command {
	var commands []Command
	for _, registrant := range r.registrants {
		commands = append(commands, registrant.Commands()...)
	}
	return commands
}

func (r *Registry) Command(text string) (Command, string, bool) {
	word, argument, _ := strings.Cut(strings.TrimSpace(text), " ")
	name, ok := strings.CutPrefix(word, "/")
	if !ok || name == "" {
		return Command{}, "", false
	}
	for _, command := range r.Commands() {
		if command.Name == name || slices.Contains(command.Aliases, name) {
			return command, strings.TrimSpace(argument), true
		}
	}
	return Command{}, "", false
}

type Slash struct {
	Registry
}

func New() *Slash {
	return &Slash{}
}
