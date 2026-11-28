package app

import (
	tea "charm.land/bubbletea/v2"

	vaultv1 "github.com/nickheyer/icebath/pkg/gen/icebath/vault/v1"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Places the vault key can move to, as the move asks for them
const (
	toKeychain = "keychain"
	toFile     = "file"
)

// Opens the page of the vault key: where it is kept as a sentence, then moving it to the keychain or to a file, each after a question that
// says what happens to the key
func (p *page) keyPage() tea.Cmd {
	t := p.kit.Env.Theme
	var moves []string
	var view *cards.List
	view = cards.NewList(p.kit.Env, cards.ListSpec{
		Title:   "Vault key",
		Glyph:   t.Glyphs.Keys,
		Key:     Screen + ":" + KeyOf,
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro:   func() []string { return p.keyState() },
		Columns: []render.Column{{}},
		Rows: func() [][]string {
			moves = p.moves()
			rows := make([][]string, len(moves))
			for i, to := range moves {
				rows[i] = []string{moveTitle(to)}
			}
			return rows
		},
		Detail: func(i int) []string { return p.moveDetail(moves[i]) },
		Empty:  func() string { return tui.Waiting(p.loaded, p.err, "") },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(moves) {
				return nil
			}
			return []wm.Hint{{Action: "hub.use", Label: "move"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(l *cards.List, i int) tea.Cmd {
				to := moves[i]
				l.Confirm(p.moveQuestion(to), func() tea.Cmd { return p.moveKey(to) })
				return nil
			},
		},
	})
	view.ToggleDetail()
	p.watch(view)
	return p.hub.Push(view)
}

// Where the key can move from where it is: to the keychain from a file, to a file from the keychain or while the keychain still holds a
// copy, and either way before there is a key
func (p *page) moves() []string {
	key := p.state.GetKey()
	var moves []string
	if key.GetStorage() != "keychain" && (key.GetStorage() != "none" || key.GetConfigured() != "keychain") {
		moves = append(moves, toKeychain)
	}
	if key.GetStorage() == "keychain" || key.GetKeychainLeft() != "" || key.GetStorage() == "none" && key.GetConfigured() == "keychain" {
		moves = append(moves, toFile)
	}
	return moves
}

// Title of a move of the key
func moveTitle(to string) string {
	if to == toKeychain {
		return "Move it to the keychain"
	}
	return "Move it to a file"
}

// First line of the page of the vault key: where it is kept, and a copy the keychain still holds
func (p *page) keyState() []string {
	key := p.state.GetKey()
	t := p.kit.Env.Theme
	var lines []string
	switch {
	case key.GetStorage() == "file":
		lines = append(lines, "Kept in a file beside the vault.")
	case key.GetStorage() == "keychain":
		lines = append(lines, "Kept in the keychain of this computer.")
	case key.GetConfigured() == "keychain":
		lines = append(lines, "Made with the first item you save, in the keychain of this computer.")
	default:
		lines = append(lines, "Made with the first item you save, in a file beside the vault.")
	}
	if left := key.GetKeychainLeft(); left != "" {
		lines = append(lines, render.Warm("The keychain still holds a copy under "+left+". Moving it to a file again removes it once the keychain can be "+
			"reached.", t))
	}
	return lines
}

// What a move does and where the key is now, said under its row
func (p *page) moveDetail(to string) []string {
	key := p.state.GetKey()
	t := p.kit.Env.Theme
	var lines []string
	if to == toKeychain {
		lines = append(lines, "Backups of this profile then hold the vault without its key.")
	} else {
		lines = append(lines, "The key sits beside the vault, so the profile can move to another computer.")
	}
	if path := key.GetPath(); path != "" && key.GetStorage() == "file" {
		lines = append(lines, render.Dim("File: "+path, t))
	}
	if service := key.GetService(); service != "" {
		lines = append(lines, render.Dim("Keychain entry: "+service+", account "+key.GetAccount(), t))
	}
	if keychain := key.GetKeychain(); keychain != "" {
		lines = append(lines, render.Dim("Keychain of this computer: "+keychain, t))
	}
	return lines
}

// Question before a move of the key, saying what happens to it
func (p *page) moveQuestion(to string) cards.Question {
	if p.state.GetKey().GetStorage() == "none" {
		if to == toKeychain {
			return cards.Question{Text: "Make the vault key in the keychain of this computer? It is made there with the first item you save.", Verb: "Move"}
		}
		return cards.Question{Text: "Make the vault key in a file beside the vault? It is made there with the first item you save.", Verb: "Move"}
	}
	if to == toKeychain {
		return cards.Question{Text: "Move the vault key to the keychain of this computer? The key file is deleted once the copy in the keychain opens " +
			"the vault.", Verb: "Move"}
	}
	return cards.Question{Text: "Move the vault key to a file beside the vault? The keychain entry is deleted once the file opens the vault.", Verb: "Move"}
}

// Moves the vault key, saying where it is now
func (p *page) moveKey(to string) tea.Cmd {
	return p.change("Moving the vault key…", func() (string, error) {
		key, err := p.service.MoveKey(p.kit.Env.Ctx, &vaultv1.MoveKeyRequest{Profile: p.profile, To: to})
		switch {
		case err != nil:
			return "", err
		case key.GetStorage() == "none" && to == toKeychain:
			return "The vault key is made in the keychain of this computer with the first item you save.", nil
		case key.GetStorage() == "none":
			return "The vault key is made in a file beside the vault with the first item you save.", nil
		case key.GetKeychainLeft() != "":
			return "The vault key is in a file now, but the keychain still holds a copy. Move it to a file again once the keychain can be reached.", nil
		case to == toKeychain:
			return "The vault key is in the keychain of this computer now.", nil
		}
		return "The vault key is in a file beside the vault now.", nil
	})
}
