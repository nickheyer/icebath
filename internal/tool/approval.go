package tool

import (
	"context"
	"time"
)

type Approver interface {
	Command(ctx context.Context, c *Call, cmd CommandCheck) Decision
	Code(ctx context.Context, c *Call, code CodeCheck) Decision
	Action(ctx context.Context, c *Call, a ActionRequest) Decision
	Elicit(ctx context.Context, c *Call, e ElicitRequest) Decision
}

type CommandCheck struct {
	Command    string
	EnvKind    string
	HostAccess bool
	SudoGuard  bool
}

type CodeCheck struct {
	Code       string
	EnvKind    string
	HostAccess bool
}

type ActionRequest struct {
	Tool                                               string
	PatternKey                                         string
	PatternKeys                                        []string
	Description                                        string
	Target                                             string
	Subject                                            string
	Noun                                               string
	Advice                                             string
	FailClosed                                         bool
	DenyCron, DenySingleQuery, DenyUnattended, NoHuman string
}

type ElicitRequest struct {
	Surface     string
	Title       string
	Message     string
	Description string
	RememberKey string
}

type Decision struct {
	Approved       bool
	Message        string
	PatternKey     string
	PatternKeys    []string
	Description    string
	Outcome        string
	Status         string
	UserConsent    *bool
	UserApproved   bool
	SmartApproved  bool
	SmartDenied    bool
	AllowPermanent *bool
	UserSummary    string
	DenyReason     string
	ReasonAsked    bool
	Command        string
}

func (d Decision) Fields() []Field {
	var msg any
	if d.Message != "" {
		msg = d.Message
	}
	switch d.Status {
	case "approval_required":
		return []Field{{"approved", false}, {"pattern_key", d.PatternKey}, {"status", d.Status},
			{"command", d.Command}, {"description", d.Description}, {"message", d.Message}}
	case "pending_approval":
		out := []Field{{"approved", false}, {"pattern_key", d.PatternKey}, {"status", d.Status},
			{"approval_pending", true}, {"command", d.Command}, {"description", d.Description}, {"message", d.Message}}
		if d.SmartDenied {
			out = append(out, Field{"smart_denied", true}, Field{"allow_permanent", false})
		}
		return out
	}
	if d.Approved {
		switch {
		case d.UserApproved:
			return []Field{{"approved", true}, {"message", msg}, {"user_approved", true}, {"description", d.Description}}
		case d.SmartApproved:
			return []Field{{"approved", true}, {"message", msg}, {"smart_approved", true}, {"description", d.Description}}
		}
		return []Field{{"approved", true}, {"message", msg}}
	}
	if d.UserConsent != nil {
		out := []Field{{"approved", false}, {"message", msg}, {"pattern_key", d.PatternKey},
			{"description", d.Description}, {"outcome", d.Outcome}, {"user_consent", *d.UserConsent},
			{"user_summary", d.UserSummary}}
		if d.ReasonAsked {
			var reason any
			if d.DenyReason != "" {
				reason = d.DenyReason
			}
			out = append(out, Field{"deny_reason", reason})
		}
		return out
	}
	if d.SmartDenied {
		return []Field{{"approved", false}, {"message", msg}, {"smart_denied", true}}
	}
	if d.PatternKey != "" || d.Description != "" {
		return []Field{{"approved", false}, {"message", msg}, {"pattern_key", d.PatternKey}, {"description", d.Description}}
	}
	return []Field{{"approved", false}, {"message", msg}}
}

type Surface interface {
	Kind() string
	PromptApproval(ctx context.Context, p ApprovalPrompt) (Choice, bool, error)
	NotifyApproval(ctx context.Context, p ApprovalPrompt) error
	PromptClarify(ctx context.Context, q ClarifyPrompt) (ClarifyReply, bool, error)
	NotifyClarify(ctx context.Context, q ClarifyPrompt) error
	SettleClarify(ctx context.Context, id string, answered, last bool)
	PromptSecret(ctx context.Context, p SecretPrompt) (string, bool, error)
}

type ApprovalPrompt struct {
	ID             string
	SessionKey     string
	Title          string
	Command        string
	Description    string
	PatternKey     string
	PatternKeys    []string
	AllowSession   bool
	AllowPermanent bool
	SmartDenied    bool
	Timeout        time.Duration
	Surface        string
}

type Choice struct {
	Answer string
	Reason string
}

type ClarifyPrompt struct {
	ID         string
	SessionKey string
	Questions  []ClarifyQuestion
	Timeout    time.Duration
}

type ClarifyQuestion struct {
	ID          string
	Text        string
	Choices     []string
	Offered     []string
	MultiSelect bool
}

type ClarifyReply struct {
	Answers map[string]ClarifyAnswer
	Outcome string
	Notice  string
}

type ClarifyAnswer struct {
	Text    string
	List    []string
	Skipped bool
}

type SecretPrompt struct {
	Kind    string
	Prompt  string
	Timeout time.Duration
}

type HumanInput struct {
	Kind       string
	RequestID  string
	SessionID  string
	SessionKey string
	Platform   string
	Prompt     string
	Outcome    string
}
