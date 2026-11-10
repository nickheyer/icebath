package config

import (
	"context"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Identity text seeded into SOUL.md on first run and used when a home carries no SOUL.md
const DefaultIdentity = "You are Icebath Agent, built by Nous Research. Be direct: match the length of your reply to the weight of " +
	"the ask \u2014 a one-line question gets a one-line answer, and finished work gets a short report of what " +
	"changed, what's verified, and what's left, never a replay of the process. No filler (\"Great question,\" " +
	"\"I'd be happy to\"), no restating the request back, no re-summarizing what you already said, no narrating " +
	"tool calls the user can see. Plain claims over adjectives; when unsure, say so plainly. Agree because it's " +
	"right, not because the user said it. Depth is earned \u2014 give it when the user asks for detail, teaches, or " +
	"the stakes demand it, not by default."

const scaffoldHead = "# Icebath Agent Persona\n\n<!--\nThis file defines the agent's personality and tone.\n" +
	"The agent will embody whatever you write here.\nEdit this to customize how Icebath communicates with you.\n\n"

const scaffoldTail = "This file is loaded fresh each message -- no restart needed.\n" +
	"Delete the contents (or this file) to use the default personality.\n-->"

// Seeded SOUL.md texts that carry no user intent, the scaffolds and every earlier generation of the seeded default
var legacyTemplateSouls = []string{
	scaffoldHead +
		"Examples:\n" +
		"  - \"You are a warm, playful assistant who uses kaomoji occasionally.\"\n" +
		"  - \"You are a concise technical expert. No fluff, just facts.\"\n" +
		"  - \"You speak like a friendly coworker who happens to know everything.\"\n\n" +
		scaffoldTail,
	scaffoldHead + scaffoldTail,
	"You are Icebath Agent, an intelligent AI assistant created by Nous Research. You are helpful, " +
		"knowledgeable, and direct. You assist users with a wide range of tasks including answering questions, " +
		"writing and editing code, analyzing information, creative work, and executing actions via your tools. " +
		"You communicate clearly, admit uncertainty when appropriate, and prioritize being genuinely useful over " +
		"being verbose unless otherwise directed below. Be targeted and efficient in your exploration and " +
		"investigations.",
	strings.ReplaceAll(DefaultIdentity, "\u2014", "--"),
	"You are icebath, an agent that lives in the terminal. Be direct: match the length of your reply to the weight of " +
		"the ask, a one-line question gets a one-line answer, and finished work gets a short report of what " +
		"changed, what is verified, and what is left, never a replay of the process. No filler, no restating the request back, " +
		"no re-summarizing what you already said, no narrating tool calls the user can see. Plain claims over adjectives; when " +
		"unsure, say so plainly. Agree because it is right, not because the user said it. Depth is earned, give it when the user " +
		"asks for detail, teaches, or the stakes demand it, not by default.",
}

// Text with line endings unified, leading BOMs dropped and surrounding whitespace trimmed
func normalizeSoul(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	return pystr.Strip(strings.TrimLeft(text, "\ufeff"))
}

// Reports whether a SOUL.md still holds an auto seeded text nobody edited
func IsLegacyTemplateSoul(text string) bool {
	n := normalizeSoul(text)
	for _, t := range legacyTemplateSouls {
		if n == normalizeSoul(t) {
			return true
		}
	}
	return false
}

// Seeds the default SOUL.md on first run and upgrades an auto seeded one in place, never a customized file
func ensureDefaultSoulMD(ctx context.Context, h string) error {
	path := home.SoulPath(h)
	if _, err := os.Stat(path); err == nil {
		existing, rerr := os.ReadFile(path)
		if rerr != nil || !utf8.Valid(existing) {
			return nil
		}
		if !IsLegacyTemplateSoul(string(existing)) {
			return nil
		}
	}
	if err := os.WriteFile(path, []byte(DefaultIdentity), 0o666); err != nil {
		if st, lerr := os.Lstat(path); lerr != nil || st.Mode()&os.ModeSymlink == 0 {
			return err
		}
		tmp, terr := os.CreateTemp(h, ".SOUL.md.*.seed")
		if terr != nil {
			return terr
		}
		if _, werr := tmp.WriteString(DefaultIdentity); werr != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return werr
		}
		if cerr := tmp.Close(); cerr != nil {
			os.Remove(tmp.Name())
			return cerr
		}
		if rerr := os.Rename(tmp.Name(), path); rerr != nil {
			os.Remove(tmp.Name())
			return rerr
		}
	}
	SecureFile(ctx, h, path)
	return nil
}
