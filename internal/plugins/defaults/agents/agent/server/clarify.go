package server

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"

	"github.com/nickheyer/icebath/internal/hooks"
	agentconfig "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/config"
	"github.com/nickheyer/icebath/internal/tools"
	agentv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Asks the user the questions on the Watch stream and waits until every one is answered, the user dismisses them, the time runs out or the
// turn stops, which dismisses them too. The answers locked by then come back either way
func (a *Agent) clarify(ctx context.Context, r *run, questions []tools.Question) (reply tools.Reply, err error) {
	set := &agentv1.Questions{Id: rand.Text(), Answers: map[string]*agentv1.Answer{}}
	var prompts []string
	for _, q := range questions {
		set.Questions = append(set.Questions, &agentv1.Question{Id: q.ID, Text: q.Text, Choices: q.Choices, MultiSelect: q.MultiSelect, Secret: q.Secret})
		prompts = append(prompts, q.Text)
	}
	// The human input hooks hear the questions asked and how they ended: a sudo password the terminal asks for as provided or skipped
	sudo := len(questions) == 1 && questions[0].ID == "sudo" && questions[0].Secret
	settings := a.settings(r)
	turn := hooks.TurnOf(ctx)
	human := hooks.HumanInput{Kind: "clarify", ID: strs.Random(16), Session: r.sessionID(), SessionKey: r.sessionID(), Platform: turn.Platform,
		Prompt: masked(strings.Join(prompts, "\n")), Turn: turn.ID, Call: hooks.CallOf(ctx)}
	if sudo {
		human.Kind = "sudo"
	}
	a.hooks.Fire(ctx, settings, human)
	defer func() {
		outcome := reply.Outcome
		if sudo && outcome == tools.Submitted {
			outcome = "skipped"
			if values := reply.Answers["sudo"]; len(values) > 0 && values[0] != "" {
				outcome = "provided"
			}
		}
		if sudo && outcome == tools.TimedOut {
			outcome = "timeout"
		}
		a.hooks.Fire(context.WithoutCancel(ctx), settings, hooks.HumanAnswered{HumanInput: human, Outcome: cmp.Or(outcome, "error")})
	}()
	settled := make(chan string, 1)
	limit := a.settings(r).GetInt(agentconfig.ClarifyTimeout)
	r.mu.Lock()
	r.questions, r.settled = set, settled
	r.publish(questionsEvent(set))
	r.mu.Unlock()
	var expired <-chan time.Time
	if limit > 0 {
		timer := time.NewTimer(time.Duration(limit) * time.Second)
		defer timer.Stop()
		expired = timer.C
	}
	var outcome string
	select {
	case outcome = <-settled:
	case <-expired:
		outcome = tools.TimedOut
	case <-ctx.Done():
		outcome = tools.Cancelled
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.questions == set {
		r.questions, r.settled = nil, nil
	} else {
		// Settled by the user just as the wait ended
		select {
		case outcome = <-settled:
		default:
		}
	}
	r.publish(questionsEvent(&agentv1.Questions{}))
	reply = tools.Reply{Outcome: outcome, Answers: map[string][]string{}}
	for id, answer := range set.GetAnswers() {
		reply.Answers[id] = answer.GetValues()
	}
	return reply, nil
}

// Locks the answer of one question of the set, values that are all blank skipping it, and settles the set when it was the last one waiting.
// Gives the questions still waiting
func (a *Agent) Answer(id, set, question string, values []string) ([]string, error) {
	r, err := a.run(id)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	pending := r.questions
	if pending == nil || pending.GetId() != set {
		return nil, errors.New("Those questions are no longer waiting for an answer.")
	}
	at := slices.IndexFunc(pending.GetQuestions(), func(q *agentv1.Question) bool { return q.GetId() == question })
	if at < 0 {
		return nil, fmt.Errorf("There is no question %s to answer.", question)
	}
	// A secret, such as a password or a code, is kept exactly as typed
	secret := pending.GetQuestions()[at].GetSecret()
	answer := &agentv1.Answer{}
	for _, value := range values {
		if !secret {
			value = strings.TrimSpace(value)
		}
		if value != "" {
			answer.Values = append(answer.Values, value)
		}
	}
	pending.Answers[question] = answer
	var remaining []string
	for _, q := range pending.GetQuestions() {
		if _, ok := pending.Answers[q.GetId()]; !ok {
			remaining = append(remaining, q.GetId())
		}
	}
	if len(remaining) > 0 {
		r.publish(questionsEvent(pending))
		return remaining, nil
	}
	r.settled <- tools.Submitted
	r.questions, r.settled = nil, nil
	return nil, nil
}

// Dismisses the questions of the set, keeping the answers locked so far
func (a *Agent) Dismiss(id, set string) error {
	r, err := a.run(id)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.questions == nil || r.questions.GetId() != set {
		return errors.New("Those questions are no longer waiting for an answer.")
	}
	r.settled <- tools.Cancelled
	r.questions, r.settled = nil, nil
	return nil
}

// Event of the questions as they stand, a copy a watcher can send while answers come in; the answer to a secret is never sent, only that
// one was given
func questionsEvent(set *agentv1.Questions) *agentv1.WatchResponse {
	shown := proto.CloneOf(set)
	for _, q := range shown.GetQuestions() {
		if answer := shown.GetAnswers()[q.GetId()]; q.GetSecret() && answer != nil && len(answer.GetValues()) > 0 {
			shown.Answers[q.GetId()] = &agentv1.Answer{Values: []string{"secret"}}
		}
	}
	return &agentv1.WatchResponse{Event: &agentv1.WatchResponse_Questions{Questions: shown}}
}

func (s *service) Answer(_ context.Context, request *agentv1.AnswerRequest) (*agentv1.AnswerResponse, error) {
	remaining, err := s.agent.Answer(request.GetSessionId(), request.GetId(), request.GetQuestionId(), request.GetAnswer().GetValues())
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error())
	}
	return &agentv1.AnswerResponse{Remaining: remaining}, nil
}

func (s *service) Dismiss(_ context.Context, request *agentv1.DismissRequest) (*agentv1.DismissResponse, error) {
	if err := s.agent.Dismiss(request.GetSessionId(), request.GetId()); err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err.Error())
	}
	return &agentv1.DismissResponse{}, nil
}
