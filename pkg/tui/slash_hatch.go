package tui

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/pet"
)

const (
	// Longest the base look step of a hatch may take, a handful of image model calls
	hatchDraftTimeout = 10 * time.Minute
	// Longest the animation row step of a hatch may take, one image model call per row
	hatchRowsTimeout = 30 * time.Minute
	// Columns of the base draft preview in the hatch box
	hatchPreviewCols = 16
	// Widest the text of the hatch box runs
	hatchBoxCols = 34
)

// One /hatch run, the base look then the animation rows, fenced by its identity
type hatchJob struct {
	concept string
	name    string
	// Token of the draft run from its first progress frame and the own cancel token of the row step
	token     string
	cancelKey string
	// Step in flight, draft, rows, compose or save
	phase       string
	preview     []string
	state       string
	done, total int
	cancelled   bool
}

// Slash command that hatches a new pet from a description on the pet verbs, cancel stopping a run
func (a *app) hatchCommands() []slashCommand {
	return []slashCommand{{name: "hatch", aliases: []string{"generate-pet"}, help: tr("slashCmd.session.hatch.help"), usage: "[description|cancel]",
		suggest: func(string) []string { return []string{"cancel"} },
		run: func(r *slashRun) tea.Cmd {
			concept := trimJS(r.arg)
			switch {
			case strings.EqualFold(concept, "cancel"):
				return tea.Batch(r.ran(), a.pet.cancelHatch())
			case concept == "":
				ran := r.ran()
				r.sys(tr("slashCmd.session.hatch.usage"))
				return ran
			}
			return tea.Batch(r.ran(), a.pet.startHatch(concept))
		}}}
}

// Gateway request of a long pet run, its answer continuing on the update loop
func (p *petView) long(method string, params map[string]any, timeout time.Duration, then func(map[string]any, error) tea.Cmd) tea.Cmd {
	gw := p.a.gw
	return func() tea.Msg {
		result, err := gw.request(context.Background(), method, params, timeout)
		return asyncDoneMsg{run: func() tea.Cmd { return then(asRPCResult(result), err) }}
	}
}

// Starts a hatch, the base look on pet.generate then every animation row on pet.hatch, adopting the new pet
func (p *petView) startHatch(concept string) tea.Cmd {
	if p.hatch != nil {
		p.a.sys(tr("slashCmd.session.hatch.busy"))
		return nil
	}
	name := pet.DisplayNameFor(concept)
	if name == "" {
		name = tr("slashCmd.session.hatch.defaultName")
	}
	job := &hatchJob{concept: concept, name: name, phase: "draft"}
	p.hatch = job
	p.a.sys(tr("slashCmd.session.hatch.designing", concept))
	return p.long("pet.generate", map[string]any{"prompt": concept, "count": 1}, hatchDraftTimeout, func(res map[string]any, err error) tea.Cmd {
		if p.hatch != job {
			return nil
		}
		drafts, _ := res["drafts"].([]any)
		switch {
		case job.cancelled:
			return p.endHatch(job, tr("slashCmd.session.hatch.cancelled"))
		case err != nil:
			return p.endHatch(job, tr("slashCmd.session.hatch.baseFailed", rpcErrorMessage(err)))
		case len(drafts) == 0:
			return p.endHatch(job, tr("slashCmd.session.hatch.noBaseDraft"))
		}
		first, _ := drafts[0].(map[string]any)
		index, _ := first["index"].(float64)
		job.token, job.cancelKey, job.phase = stringOr(res["token"], job.token), uuid.NewString(), "rows"
		var preview tea.Cmd
		if job.preview == nil {
			preview = p.previewDraft(job, stringOr(first["dataUri"], ""))
		}
		params := map[string]any{"token": job.token, "index": int(index), "name": name, "prompt": concept, "cancelToken": job.cancelKey}
		return tea.Batch(preview, p.long("pet.hatch", params, hatchRowsTimeout, func(res map[string]any, err error) tea.Cmd {
			if p.hatch != job {
				return nil
			}
			slug := stringOr(res["slug"], "")
			switch {
			case err != nil && job.cancelled:
				return p.endHatch(job, tr("slashCmd.session.hatch.cancelled"))
			case err != nil:
				return p.endHatch(job, tr("slashCmd.session.hatch.failed", rpcErrorMessage(err)))
			case slug == "":
				return p.endHatch(job, tr("slashCmd.session.hatch.failed", tr("pickers.common.invalidResponse", "pet.hatch")))
			case job.cancelled:
				return gatewayCall(p.a.gw, "pet.remove", map[string]any{"slug": slug}, func(_ any, err error) tea.Cmd {
					if err != nil {
						p.a.sys(tr("slashCmd.session.pet.error", rpcErrorMessage(err)))
					}
					return p.endHatch(job, tr("slashCmd.session.hatch.cancelled"))
				})
			}
			display := stringOr(res["displayName"], name)
			return p.claimSlug(job, slug, name, func(final string) tea.Cmd {
				return gatewayCall(p.a.gw, "pet.select", map[string]any{"slug": final}, func(_ any, err error) tea.Cmd {
					if err != nil {
						return p.endHatch(job, tr("slashCmd.session.pet.error", rpcErrorMessage(err)))
					}
					return p.endHatch(job, tr("slashCmd.session.hatch.hatched", display))
				})
			})
		}))
	})
}

// Puts the hatched pet under the slug of its name as /hatch in the CLI does, a pet already there replaced
func (p *petView) claimSlug(job *hatchJob, slug, name string, then func(string) tea.Cmd) tea.Cmd {
	want := pet.Slugify(name)
	if slug == want {
		return then(slug)
	}
	fail := func(err error) tea.Cmd {
		return p.endHatch(job, tr("slashCmd.session.hatch.failed", rpcErrorMessage(err)))
	}
	return gatewayCall(p.a.gw, "pet.remove", map[string]any{"slug": want}, func(_ any, err error) tea.Cmd {
		if err != nil {
			return fail(err)
		}
		return gatewayCall(p.a.gw, "pet.rename", map[string]any{"slug": slug, "name": name}, func(v any, err error) tea.Cmd {
			if err != nil {
				return fail(err)
			}
			return then(stringOr(asRPCResult(v)["slug"], slug))
		})
	})
}

// Ends the hatch with its last line in the transcript
func (p *petView) endHatch(job *hatchJob, line string) tea.Cmd {
	if p.hatch == job {
		p.hatch = nil
	}
	p.a.sys(line)
	return nil
}

// Stops the hatch in flight through pet.cancel
func (p *petView) cancelHatch() tea.Cmd {
	job := p.hatch
	if job == nil {
		p.a.sys(tr("slashCmd.session.hatch.notRunning"))
		return nil
	}
	if job.cancelled {
		return nil
	}
	job.cancelled = true
	p.a.sys(tr("slashCmd.session.hatch.stopping"))
	return p.sendCancel(job)
}

// Stops the step in flight on the gateway, the draft by its run token, the rows by the own token
func (p *petView) sendCancel(job *hatchJob) tea.Cmd {
	token := job.cancelKey
	if job.phase == "draft" {
		token = job.token
	}
	if token == "" {
		return nil
	}
	return gatewayCall(p.a.gw, "pet.cancel", map[string]any{"token": token}, func(_ any, err error) tea.Cmd {
		if err != nil && p.hatch == job {
			p.a.sys(tr("slashCmd.session.pet.error", rpcErrorMessage(err)))
		}
		return nil
	})
}

// Takes a pet.generate.progress frame, the token of the draft run, a cancel waiting on it, and the draft preview
func (p *petView) draftProgress(payload map[string]any) tea.Cmd {
	job := p.hatch
	if job == nil || job.phase != "draft" || payload == nil {
		return nil
	}
	token := stringOr(payload["token"], "")
	if token != "" && job.token != "" && token != job.token {
		return nil
	}
	var cmds []tea.Cmd
	if token != "" && job.token == "" {
		job.token = token
		if job.cancelled {
			cmds = append(cmds, p.sendCancel(job))
		}
	}
	if uri := stringOr(payload["dataUri"], ""); uri != "" && job.preview == nil {
		cmds = append(cmds, p.previewDraft(job, uri))
	}
	return tea.Batch(cmds...)
}

// Takes a pet.hatch.progress frame, each row drawn, the compose and the save said in the transcript
func (p *petView) rowProgress(payload map[string]any) tea.Cmd {
	job := p.hatch
	if job == nil || job.phase == "draft" || payload == nil {
		return nil
	}
	switch stringOr(payload["event"], "") {
	case "row":
		state := stringOr(payload["state"], "")
		if state == "" {
			state, _, _ = strings.Cut(stringOr(payload["detail"], ""), ":")
		}
		job.state = state
		if done, err := strconv.Atoi(stringOr(payload["done"], "")); err == nil {
			job.done = done
		}
		if total, err := strconv.Atoi(stringOr(payload["total"], "")); err == nil {
			job.total = total
		}
		p.a.sys("  " + tr("slashCmd.session.hatch.drawing", state))
	case "compose":
		job.phase = "compose"
		p.a.sys("  " + tr("slashCmd.session.hatch.composing"))
	case "save":
		job.phase = "save"
		p.a.sys("  " + tr("slashCmd.session.hatch.saving"))
	}
	return nil
}

// Decodes a base draft beside the UI into the half block preview of the hatch box
func (p *petView) previewDraft(job *hatchJob, uri string) tea.Cmd {
	if uri == "" {
		return nil
	}
	return func() tea.Msg {
		rows, err := draftPreview(uri)
		return asyncDoneMsg{run: func() tea.Cmd {
			if p.hatch != job {
				return nil
			}
			if err != nil {
				p.a.sys("  " + tr("slashCmd.session.hatch.previewFailed", err.Error()))
				return nil
			}
			job.preview = rows
			return nil
		}}
	}
}

// Half block rows of a draft image data URI at the preview width
func draftPreview(uri string) ([]string, error) {
	_, data, found := strings.Cut(uri, ",")
	if !found {
		return nil, errors.New(tr("pickers.common.invalidResponse", "dataUri"))
	}
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return nil, err
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	return halfBlockRows(pet.DownscaleCells(toNRGBA(img), hatchPreviewCols)), nil
}

// Framed box of the hatch in flight, the draft preview over the step and the way to stop it
func (p *petView) hatchBox(t theme) []string {
	job := p.hatch
	if job == nil {
		return nil
	}
	status := tr("slashCmd.session.hatch.boxDesigning")
	switch job.phase {
	case "rows":
		status = tr("slashCmd.session.hatch.boxRows")
		if job.total > 0 {
			status = tr("slashCmd.session.hatch.boxDrawing", job.state, job.done, job.total)
		}
	case "compose":
		status = tr("slashCmd.session.hatch.boxComposing")
	case "save":
		status = tr("slashCmd.session.hatch.boxSaving")
	}
	hint := tr("slashCmd.session.hatch.boxCancel")
	if job.cancelled {
		hint = tr("slashCmd.session.hatch.boxStopping")
	}
	width := hatchPreviewCols
	for _, line := range []string{status, hint} {
		width = max(width, ansi.StringWidth(line))
	}
	width = min(width, hatchBoxCols)
	inner := append(append([]string{}, job.preview...), toneStyle(t.Color.Text).Render(truncateEnd(status, width)),
		toneStyle(t.Color.Muted).Render(truncateEnd(hint, width)))
	return framedBox(t, " /hatch · "+job.name+" ", inner, width+4)
}
