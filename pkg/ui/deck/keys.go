package deck

import "charm.land/bubbles/v2/key"

// Bindings the deck handles before forwarding keys to the focused card
type KeyMap struct {
	Next     key.Binding
	Prev     key.Binding
	ToMain   key.Binding
	ToStack  key.Binding
	Enter    key.Binding
	Back     key.Binding
	Grow     key.Binding
	Shrink   key.Binding
	MoveDown key.Binding
	MoveUp   key.Binding
	Close    key.Binding
	Jump     key.Binding
	Help     key.Binding
	Quit     key.Binding
}

// Vim and arrow bindings with tab cycling, digits to jump, enter to dive in, and esc to back out
func DefaultKeyMap() KeyMap {
	return KeyMap{
		Next:     key.NewBinding(key.WithKeys("tab", "j", "down"), key.WithHelp("tab", "next")),
		Prev:     key.NewBinding(key.WithKeys("shift+tab", "k", "up"), key.WithHelp("shift+tab", "prev")),
		ToMain:   key.NewBinding(key.WithKeys("h", "left"), key.WithHelp("h", "main")),
		ToStack:  key.NewBinding(key.WithKeys("l", "right"), key.WithHelp("l", "stack")),
		Enter:    key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "dive")),
		Back:     key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back")),
		Grow:     key.NewBinding(key.WithKeys("L", "shift+l", "ctrl+right"), key.WithHelp("L", "grow")),
		Shrink:   key.NewBinding(key.WithKeys("H", "shift+h", "ctrl+left"), key.WithHelp("H", "shrink")),
		MoveDown: key.NewBinding(key.WithKeys("J", "shift+j", "shift+down"), key.WithHelp("J", "move down")),
		MoveUp:   key.NewBinding(key.WithKeys("K", "shift+k", "shift+up"), key.WithHelp("K", "move up")),
		Close:    key.NewBinding(key.WithKeys("x", "delete", "backspace"), key.WithHelp("x", "close")),
		Jump:     key.NewBinding(key.WithKeys("1", "2", "3", "4", "5", "6", "7", "8", "9"), key.WithHelp("1-9", "jump")),
		Help:     key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "hints")),
		Quit:     key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
	}
}
