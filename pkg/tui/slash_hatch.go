//go:build !icebath_core && !icebath_noextras

package tui

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/pet"
	"github.com/nickheyer/icebath/pkg/tuigateway"
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
				return tea.Batch(r.ran(), a.petView().cancelHatch())
			case concept == "":
				ran := r.ran()
				r.sys(tr("slashCmd.session.hatch.usage"))
				return ran
			}
			return tea.Batch(r.ran(), a.petView().startHatch(concept))
		}}}
}

// Typed gateway request of a long pet run, its answer or failure continuing on the update loop
func longPet[P, R any](p *petView, m tuigateway.Method[P, R], params P, timeout time.Duration, then func(*R, error) tea.Cmd) tea.Cmd {
	gw := p.a.gw
	return func() tea.Msg {
		raw, err := gw.request(context.Background(), m.Name(), wireParams(params), timeout)
		var res *R
		if err == nil {
			r, misfit := m.Result(raw)
			if misfit != nil {
				err = fmt.Errorf("%s %w", misfitText(tr("session.common.invalidResponse", m.Name())), misfit)
			} else {
				res = &r
			}
		}
		return asyncDoneMsg{run: func() tea.Cmd { return then(res, err) }}
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
	draft := tuigateway.PetGenerateParams{Prompt: concept, Count: 1}
	return longPet(p, tuigateway.PetGenerate, draft, hatchDraftTimeout, func(res *tuigateway.PetGenerateResult, err error) tea.Cmd {
		if p.hatch != job {
			return nil
		}
		switch {
		case job.cancelled:
			return p.endHatch(job, tr("slashCmd.session.hatch.cancelled"))
		case err != nil:
			return p.endHatch(job, tr("slashCmd.session.hatch.baseFailed", rpcErrorMessage(err)))
		case len(res.Drafts) == 0:
			return p.endHatch(job, tr("slashCmd.session.hatch.noBaseDraft"))
		}
		first := res.Drafts[0]
		job.token, job.cancelKey, job.phase = cmp.Or(res.Token, job.token), uuid.NewString(), "rows"
		var preview tea.Cmd
		if job.preview == nil {
			preview = p.previewDraft(job, first.DataURI)
		}
		params := tuigateway.PetHatchParams{Token: job.token, Index: &first.Index, Name: name, Prompt: concept, CancelToken: job.cancelKey}
		return tea.Batch(preview, longPet(p, tuigateway.PetHatch, params, hatchRowsTimeout, func(res *tuigateway.PetHatchResult, err error) tea.Cmd {
			if p.hatch != job {
				return nil
			}
			slug := ""
			if res != nil {
				slug = res.Slug
			}
			switch {
			case err != nil && job.cancelled:
				return p.endHatch(job, tr("slashCmd.session.hatch.cancelled"))
			case err != nil:
				return p.endHatch(job, tr("slashCmd.session.hatch.failed", rpcErrorMessage(err)))
			case slug == "":
				return p.endHatch(job, tr("slashCmd.session.hatch.failed", misfitText(tr("pickers.common.invalidResponse", "pet.hatch"))))
			case job.cancelled:
				return call(p.a.gw, tuigateway.PetRemove, tuigateway.PetSlugParams{Slug: slug}, func(_ *tuigateway.PetSlugResult, err error) tea.Cmd {
					if err != nil {
						p.a.sys(tr("slashCmd.session.pet.error", rpcErrorMessage(err)))
					}
					return p.endHatch(job, tr("slashCmd.session.hatch.cancelled"))
				})
			}
			display := cmp.Or(res.DisplayName, name)
			return p.claimSlug(job, slug, name, func(final string) tea.Cmd {
				return call(p.a.gw, tuigateway.PetSelect, tuigateway.PetSlugParams{Slug: final}, func(_ *tuigateway.PetSlugResult, err error) tea.Cmd {
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
	return call(p.a.gw, tuigateway.PetRemove, tuigateway.PetSlugParams{Slug: want}, func(_ *tuigateway.PetSlugResult, err error) tea.Cmd {
		if err != nil {
			return fail(err)
		}
		return call(p.a.gw, tuigateway.PetRename, tuigateway.PetRenameParams{Slug: slug, Name: name}, func(res *tuigateway.PetSlugResult, err error) tea.Cmd {
			if err != nil {
				return fail(err)
			}
			return then(cmp.Or(res.Slug, slug))
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
	return call(p.a.gw, tuigateway.PetCancel, tuigateway.PetCancelParams{Token: token}, func(_ *tuigateway.PetOkResult, err error) tea.Cmd {
		if err != nil && p.hatch == job {
			p.a.sys(tr("slashCmd.session.pet.error", rpcErrorMessage(err)))
		}
		return nil
	})
}

// Takes a pet.generate.progress frame, the token of the draft run, a cancel waiting on it, and the draft preview
func (p *petView) draftProgress(payload tuigateway.PetGenerateProgressPayload) tea.Cmd {
	job := p.hatch
	if job == nil || job.phase != "draft" {
		return nil
	}
	token := payload.Token
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
	if uri := payload.DataURI; uri != "" && job.preview == nil {
		cmds = append(cmds, p.previewDraft(job, uri))
	}
	return tea.Batch(cmds...)
}

// Takes a pet.hatch.progress frame, each row drawn, the compose and the save said in the transcript
func (p *petView) rowProgress(payload tuigateway.PetHatchProgressPayload) tea.Cmd {
	job := p.hatch
	if job == nil || job.phase == "draft" {
		return nil
	}
	switch payload.Event {
	case "row":
		state := payload.State
		if state == "" {
			state, _, _ = strings.Cut(payload.Detail, ":")
		}
		job.state = state
		if done, err := strconv.Atoi(payload.Done); err == nil {
			job.done = done
		}
		if total, err := strconv.Atoi(payload.Total); err == nil {
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
		return nil, errors.New(misfitText(tr("pickers.common.invalidResponse", "dataUri")))
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
	return framedBox(t, p.a.uiTheme().Border, " /hatch · "+job.name+" ", inner, width+4)
}
