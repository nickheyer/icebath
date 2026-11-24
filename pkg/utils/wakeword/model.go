package wakewordutil

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
)

// One wake word classifier scoring rolling windows of embeddings; not safe for concurrent use
type Model struct {
	id         string
	stage      *stage
	windows    int
	embeddings []float32
	pending    int
	cleanup    runtime.Cleanup
}

// Classifier from a model file, named by the file's stem, its window count read from the model's input shape
func (rt *Runtime) Model(path string) (*Model, error) {
	s, err := newStage(rt, path, nil)
	if err != nil {
		return nil, err
	}
	windows, err := s.classifierWindows()
	if err != nil {
		s.release()
		return nil, err
	}
	m := &Model{id: stem(path), stage: s, windows: windows, embeddings: make([]float32, max(maxEmb, windows)*wwFeatures)}
	m.cleanup = runtime.AddCleanup(m, (*stage).release, s)
	return m, nil
}

// Name of the model, the stem of its file name
func (m *Model) ID() string { return m.id }

// Feeds embedding windows and yields one score per classifier window that becomes ready, stopping when yield returns false; nothing once closed
func (m *Model) ProcessStreaming(embeddings []float32, yield func(float32) bool) error {
	defer runtime.KeepAlive(m)
	if !m.stage.open() {
		return nil
	}
	if len(embeddings)%wwFeatures != 0 {
		return fmt.Errorf("%w: %d embedding values are not whole %d-value windows", ErrPartialFrame, len(embeddings), wwFeatures)
	}
	rows := len(m.embeddings) / wwFeatures
	shiftAppend(m.embeddings, embeddings)
	m.pending = min(rows, m.pending+len(embeddings)/wwFeatures)
	for m.pending >= m.windows {
		start := rows - m.pending
		m.pending = max(0, m.pending-1)
		scores, err := m.stage.run(m.embeddings[start*wwFeatures : (start+m.windows)*wwFeatures])
		if err != nil {
			return err
		}
		if len(scores) != 1 {
			return m.stage.shapeError(fmt.Sprintf("classifier output has %d values, want 1", len(scores)))
		}
		if !yield(scores[0]) {
			return nil
		}
	}
	return nil
}

// Clears the rolling embeddings so earlier audio cannot score
func (m *Model) Reset() {
	clear(m.embeddings)
	m.pending = 0
}

// Releases the interpreter and the model, later calls doing nothing
func (m *Model) Close() {
	m.cleanup.Stop()
	m.stage.release()
}

// File name without its last extension, a leading dot not counting as one
func stem(path string) string {
	base := filepath.Base(path)
	if at := strings.LastIndexByte(base, '.'); at > 0 {
		return base[:at]
	}
	return base
}
