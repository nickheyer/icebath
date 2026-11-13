package googlechat

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/i18n"
)

var (
	fencedRe    = regexp.MustCompile("```(?:[^\n]*\n)?[\\s\\S]*?```")
	inlineRe    = regexp.MustCompile("`[^`]+`")
	headerRe    = regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`)
	boldItalRe  = regexp.MustCompile(`\*\*\*(.+?)\*\*\*`)
	boldRe      = regexp.MustCompile(`\*\*(.+?)\*\*`)
	linkRe      = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	invisibleRe = regexp.MustCompile("[​-‏⁠﻿︀-️\U000e0100-\U000e01ef]")
	spacesRe    = regexp.MustCompile(`  +`)
)

// Button function a clarify choice click invokes
const clarifyAction = "icebath_clarify"

// Choice value of the free text answer button
const clarifyOtherChoice = "__other__"

// Every match replaced by fn of its submatches
func replaceGroups(re *regexp.Regexp, s string, fn func(groups []string) string) string {
	var b strings.Builder
	last := 0
	for _, loc := range re.FindAllStringSubmatchIndex(s, -1) {
		groups := make([]string, len(loc)/2)
		for i := range groups {
			if loc[2*i] >= 0 {
				groups[i] = s[loc[2*i]:loc[2*i+1]]
			}
		}
		b.WriteString(s[last:loc[0]])
		b.WriteString(fn(groups))
		last = loc[1]
	}
	b.WriteString(s[last:])
	return b.String()
}

// Markdown in the Google Chat dialect with code protected and invisible codepoints stripped
func formatMessage(content string) string {
	if content == "" {
		return content
	}
	var values []string
	hold := func(value string) string {
		values = append(values, value)
		return fmt.Sprintf("\x00GC%d\x00", len(values)-1)
	}
	text := fencedRe.ReplaceAllStringFunc(content, hold)
	text = inlineRe.ReplaceAllStringFunc(text, hold)
	text = replaceGroups(headerRe, text, func(g []string) string { return hold("*" + strings.TrimSpace(g[1]) + "*") })
	text = replaceGroups(boldItalRe, text, func(g []string) string { return hold("*_" + g[1] + "_*") })
	text = replaceGroups(boldRe, text, func(g []string) string { return hold("*" + g[1] + "*") })
	text = replaceGroups(linkRe, text, func(g []string) string { return hold("<" + g[2] + "|" + g[1] + ">") })
	text = invisibleRe.ReplaceAllString(text, "")
	text = spacesRe.ReplaceAllString(text, " ")
	for i := len(values) - 1; i >= 0; i-- {
		text = strings.ReplaceAll(text, fmt.Sprintf("\x00GC%d\x00", i), values[i])
	}
	return text
}

// One entry of the cardsV2 message field
type cardV2 struct {
	CardID string `json:"cardId"`
	Card   card   `json:"card"`
}

type card struct {
	Header   *cardHeader   `json:"header,omitempty"`
	Sections []cardSection `json:"sections"`
}

type cardHeader struct {
	Title string `json:"title"`
}

type cardSection struct {
	Widgets []cardWidget `json:"widgets"`
}

type cardWidget struct {
	TextParagraph *textParagraph `json:"textParagraph,omitempty"`
	ButtonList    *buttonList    `json:"buttonList,omitempty"`
}

type textParagraph struct {
	Text string `json:"text"`
}

type buttonList struct {
	Buttons []cardButton `json:"buttons"`
}

type cardButton struct {
	Text    string        `json:"text"`
	OnClick buttonOnClick `json:"onClick"`
}

type buttonOnClick struct {
	Action buttonAction `json:"action"`
}

type buttonAction struct {
	Function   string            `json:"function"`
	Parameters []actionParameter `json:"parameters"`
}

type actionParameter struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Clarify button answering with the choice, the label cut to 80 characters
func clarifyButton(clarifyID, label, choice string) cardButton {
	if r := []rune(label); len(r) > 80 {
		label = string(r[:77]) + "..."
	}
	return cardButton{Text: label, OnClick: buttonOnClick{Action: buttonAction{Function: clarifyAction,
		Parameters: []actionParameter{{Key: "choice", Value: choice}, {Key: "clarify_id", Value: clarifyID}}}}}
}

// Question card with one button per choice and the free text answer button
func clarifyCard(ctx context.Context, clarifyID, question string, choices []string) cardV2 {
	var buttons []cardButton
	for _, choice := range choices {
		if text := strings.TrimSpace(choice); text != "" {
			buttons = append(buttons, clarifyButton(clarifyID, text, text))
		}
	}
	buttons = append(buttons, clarifyButton(clarifyID, i18n.T(ctx, "platform.google_chat.clarify.other_button", "", nil), clarifyOtherChoice))
	questionText := formatMessage(i18n.T(ctx, "platform.google_chat.clarify.question", "", map[string]any{"question": question}))
	return cardV2{CardID: "clarify-" + clarifyID, Card: card{
		Header: &cardHeader{Title: i18n.T(ctx, "platform.google_chat.clarify.header", "", nil)},
		Sections: []cardSection{{Widgets: []cardWidget{{TextParagraph: &textParagraph{Text: questionText}},
			{ButtonList: &buttonList{Buttons: buttons}}}}}}}
}

// Answered or expired clarify card, the question kept and the buttons replaced by the answer when one was taken
func clarifySettledCard(ctx context.Context, clarifyID, questionText, answer string) cardV2 {
	widgets := []cardWidget{{TextParagraph: &textParagraph{Text: questionText}}}
	if answer != "" {
		widgets = append(widgets, cardWidget{TextParagraph: &textParagraph{Text: "*" + answer + "*"}})
	}
	return cardV2{CardID: "clarify-" + clarifyID, Card: card{
		Header:   &cardHeader{Title: i18n.T(ctx, "platform.google_chat.clarify.header", "", nil)},
		Sections: []cardSection{{Widgets: widgets}}}}
}

// Question text of the first text paragraph of a clarify card message
func clarifyQuestionText(cards []cardV2) string {
	for _, c := range cards {
		for _, s := range c.Card.Sections {
			for _, w := range s.Widgets {
				if w.TextParagraph != nil {
					return w.TextParagraph.Text
				}
			}
		}
	}
	return ""
}
