package gatewayrun

import (
	"context"
	"sync"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Fresh turn tool environment with every session variable of the context bound, the task local values a turn sets
func (r *GatewayRunner) setSessionEnv(ctx context.Context, sc *gateway.SessionContext) (*tools.Env, error) {
	source, err := gateway.SessionEnvSource(ctx, r, sc.Source)
	if err != nil {
		return nil, err
	}
	asyncDelivery := true
	if r.adapters != nil {
		if adapter, ok := r.adapters.Get(source.Platform); ok && adapter != nil {
			asyncDelivery = gateway.AdapterSupportsPush(adapter)
		}
	}
	apiVars, err := gateway.APISessionVars(ctx)
	if err != nil {
		return nil, err
	}
	var historyDelivery *string
	if value, ok := apiVars["session_history_delivery"]; ok {
		text := noneText(value)
		historyDelivery = &text
	}
	cronSession := ""
	env := &tools.Env{Home: home.Dir(ctx), Values: &sync.Map{}}
	tools.SetSessionVars(env, tools.SessionVars{
		Platform: string(source.Platform), ChatID: source.ChatID, ChatType: source.ChatType,
		ChatName: optText(source.ChatName), ThreadID: optText(source.ThreadID), UserID: optText(source.UserID),
		UserIDAlt: optText(source.UserIDAlt), UserName: optText(source.UserName), ScopeID: optText(source.ScopeID),
		ParentChatID: optText(source.ParentChatID), SessionKey: sc.SessionKey, SessionID: sc.SessionID,
		MessageID: optText(source.MessageID), Profile: optText(source.Profile), NoAsyncDelivery: !asyncDelivery,
		CronSession: &cronSession, SessionHistoryDelivery: historyDelivery,
	})
	return env, nil
}

// Resets every session variable the turn bound, so reads stop seeing the finished turn's identity
func (r *GatewayRunner) clearSessionEnv(env *tools.Env) {
	if env != nil {
		tools.ClearSessionVars(env)
	}
}
