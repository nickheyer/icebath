package slash

import "testing"

type fixed []Command

func (f fixed) Commands() []Command { return f }

func TestRegistryParsesCommands(t *testing.T) {
	host := New()
	nested := new(Registry)
	host.Register(nested)
	nested.Register(fixed{{Name: "new", Aliases: []string{"clear", "reset"}}, {Name: "title"}})

	cases := []struct {
		text, name, argument string
		ok                   bool
	}{
		{"/title  my session ", "title", "my session", true},
		{"  /clear", "new", "", true},
		{"/new", "new", "", true},
		{"/unknown thing", "", "", false},
		{"title", "", "", false},
		{"/", "", "", false},
		{"say /title", "", "", false},
	}
	for _, c := range cases {
		command, argument, ok := host.Command(c.text)
		if ok != c.ok || command.Name != c.name || argument != c.argument {
			t.Errorf("Command(%q) = %q, %q, %v, want %q, %q, %v", c.text, command.Name, argument, ok, c.name, c.argument, c.ok)
		}
	}
}
