package tools

import (
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/internal/tools"
)

// One search_files call
type searchCall struct {
	pattern, target, path, glob string
	limit, offset               int
	mode                        string
	context                     int
	order                       string
}

// Searches file contents or names behind the repeated search and credential guards, hits in files the model may not read left out
func (o *ops) searchTool(c searchCall) string {
	m := o.r.memory
	m.mu.Lock()
	count := o.track.repeat(strings.Join([]string{"search", c.pattern, c.target, c.path, c.glob, fmt.Sprint(c.limit), fmt.Sprint(c.offset), c.order}, "\x00"))
	m.mu.Unlock()
	if count >= repeatsBlock {
		return failure(fmt.Sprintf("BLOCKED: You have run this exact search %d times in a row. The results have NOT changed. You already have this "+
			"information. STOP re-searching and proceed with your task.", count), "pattern", c.pattern, "already_searched", count, guardrail, true)
	}
	if refusal := ntRefusal(c.path, "Search"); refusal != "" {
		return failure(refusal)
	}
	root := o.resolve(c.path)
	if block := o.readBlock(c.path, root); block != "" {
		return failure(block)
	}
	if cached, ok := m.cachedMissing(o.track, "search", root); ok {
		return cached
	}
	r := o.search(c.pattern, c.path, c.target, c.glob, c.limit, c.offset, c.mode, c.context, c.order)
	omitted := 0
	allowed := func(p string) bool {
		if o.readBlock(p, o.resolve(p)) != "" {
			omitted++
			return false
		}
		return true
	}
	kept := r.matches[:0]
	for _, hit := range r.matches {
		if allowed(hit.Path) {
			if hit.Content != "" {
				hit.Content = o.redact(hit.Content, o.resolve(hit.Path))
			}
			kept = append(kept, hit)
		}
	}
	r.matches = kept
	files := r.files[:0]
	for _, f := range r.files {
		if allowed(f) {
			files = append(files, f)
		}
	}
	r.files = files
	counts := r.counts[:0]
	for _, c := range r.counts {
		if allowed(c.path) {
			counts = append(counts, c)
		}
	}
	r.counts = counts
	r.err = tools.BoundError(r.err)
	var extra []string
	if omitted > 0 {
		extra = append(extra, "_omitted", fmt.Sprintf("%d result(s) omitted because they target credential, token, cache, or secret-bearing "+
			"environment files.", omitted))
	}
	if count >= repeatsWarn {
		extra = append(extra, "_warning", fmt.Sprintf("You have run this exact search %d times consecutively. The results have not changed. Use the "+
			"information you already have.", count))
	}
	if r.truncated {
		extra = append(extra, "_hint", fmt.Sprintf("Results truncated. Use offset=%d to see more, or narrow with a more specific pattern or file_glob.",
			c.offset+c.limit))
	}
	out := r.json(extra...)
	if strings.HasPrefix(r.err, "Path not found:") {
		m.rememberMissing(o.track, "search", root, out)
	}
	return out
}
