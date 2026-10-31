package harness

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
)

// One answer a question offers
type Choice struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// Question put to the person, answered by picking choices or typing an answer of their own
type Question struct {
	Text    string   `json:"question"`
	Header  string   `json:"header,omitempty"`
	Choices []Choice `json:"options"`
	Multi   bool     `json:"multi_select,omitempty"`
}

// Answer the person gave one question, the labels picked or the text typed, declined when they gave neither
type Answer struct {
	Picked   []string `json:"picked,omitempty"`
	Text     string   `json:"text,omitempty"`
	Declined bool     `json:"declined,omitempty"`
}

// Reports whether the answer carries something the asker can read
func (a Answer) Given() bool {
	return !a.Declined && (len(a.Picked) > 0 || strings.TrimSpace(a.Text) != "")
}

// Answer as one line, the labels picked and the text typed, or the refusal
func (a Answer) String() string {
	if !a.Given() {
		return "no answer"
	}
	parts := append([]string(nil), a.Picked...)
	if t := strings.TrimSpace(a.Text); t != "" {
		parts = append(parts, t)
	}
	return strings.Join(parts, ", ")
}

// Questions of one tool call waiting on the person
type Asking struct {
	ID        int
	Run       string
	Session   string
	Questions []Question
	Asked     time.Time
	answer    chan []Answer
}

// Question a person must answer
type QuestionMsg struct {
	Asking *Asking
}

// Questions that were answered, declined or abandoned
type QuestionDoneMsg struct {
	ID      int
	Run     string
	Session string
	Answers []Answer
	Err     error
}

// Error an ask returns once the questioner was closed
var ErrQuestionsClosed = errors.New("harness: questioner closed")

// Asker that posts each set of questions into the program and waits for the answers the program hands back
type Questioner struct {
	send    Sender
	mu      sync.Mutex
	next    int
	pending map[int]*Asking
	closed  bool
}

func NewQuestioner(send Sender) *Questioner {
	return &Questioner{send: send, pending: map[int]*Asking{}}
}

// Posts the questions for the session and blocks until every one is answered or the context ends
func (q *Questioner) Ask(ctx context.Context, run, sessionID string, questions []Question) ([]Answer, error) {
	if len(questions) == 0 {
		return nil, errors.New("harness: at least one question is required")
	}
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return nil, ErrQuestionsClosed
	}
	q.next++
	a := &Asking{ID: q.next, Run: run, Session: sessionID, Questions: questions, Asked: time.Now(), answer: make(chan []Answer, 1)}
	q.pending[a.ID] = a
	q.mu.Unlock()
	q.send(QuestionMsg{Asking: a})
	var answers []Answer
	var err error
	select {
	case answers = <-a.answer:
	case <-ctx.Done():
		err = ctx.Err()
	}
	q.mu.Lock()
	delete(q.pending, a.ID)
	q.mu.Unlock()
	q.send(QuestionDoneMsg{ID: a.ID, Run: run, Session: sessionID, Answers: answers, Err: err})
	return answers, err
}

// Delivers one answer per question to a pending ask and reports whether one was waiting for that many
func (q *Questioner) Answer(id int, answers []Answer) bool {
	q.mu.Lock()
	a, ok := q.pending[id]
	q.mu.Unlock()
	if !ok || len(answers) != len(a.Questions) {
		return false
	}
	select {
	case a.answer <- answers:
		return true
	default:
		return false
	}
}

// Answers every question of a pending ask as declined and reports whether one was waiting
func (q *Questioner) Decline(id int) bool {
	q.mu.Lock()
	a, ok := q.pending[id]
	q.mu.Unlock()
	if !ok {
		return false
	}
	answers := make([]Answer, len(a.Questions))
	for i := range answers {
		answers[i].Declined = true
	}
	return q.Answer(id, answers)
}

// Asks waiting, oldest first
func (q *Questioner) Pending() []*Asking {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]*Asking, 0, len(q.pending))
	for _, a := range q.pending {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Oldest ask waiting on the session, nil while none is
func (q *Questioner) ForSession(id string) *Asking {
	for _, a := range q.Pending() {
		if a.Session == id {
			return a
		}
	}
	return nil
}

func (q *Questioner) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending)
}

// Refuses every later ask and declines the ones waiting
func (q *Questioner) Close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	for _, a := range q.Pending() {
		q.Decline(a.ID)
	}
}
