package dashboard

import (
	"context"
	"errors"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// One lock per home and server serializing dashboard MCP OAuth transactions
var mcpOAuthTransactions = struct {
	sync.Mutex
	locks map[[2]string]*sync.Mutex
}{locks: map[[2]string]*sync.Mutex{}}

// Transaction lock of the flow home and server, created on first use
func mcpOAuthTransaction(flow *mcp.DashboardOAuthFlow) *sync.Mutex {
	key := [2]string{flow.HomeDir, flow.ServerName}
	mcpOAuthTransactions.Lock()
	defer mcpOAuthTransactions.Unlock()
	lock, ok := mcpOAuthTransactions.locks[key]
	if !ok {
		lock = &sync.Mutex{}
		mcpOAuthTransactions.locks[key] = lock
	}
	return lock
}

// Runs the MCP probe with dashboard redirect and callback handlers, rolling tokens back on failure
func runDashboardMCPOAuth(ctx context.Context, env *tools.Env, flow *mcp.DashboardOAuthFlow, cfg map[string]any) {
	defer flow.MarkWorkerDone()
	err := func() error {
		wctx := home.WithOverride(ctx, flow.HomeDir)
		wctx = provider.WithSecretScope(wctx, provider.BuildProfileSecretScope(flow.HomeDir), flow.HomeDir)
		transaction := mcpOAuthTransaction(flow)
		transaction.Lock()
		defer transaction.Unlock()
		ictx := mcp.WithDashboardOAuthFlow(mcp.ForceInteractiveOAuth(wctx), flow)
		manager := mcp.GetManager()
		storage := mcp.NewTokenStorage(ictx, flow.ServerName, "")
		backup := storage.Snapshot()
		previous, err := manager.Remove(ictx, flow.ServerName, flow.HomeDir)
		if err != nil {
			previous = nil
		} else {
			err = func() error {
				summaries, err := mcp.ProbeSingleServerTimeout(ictx, env, flow.ServerName, cfg, mcp.LoginConnectTimeout(cfg))
				if err != nil {
					return err
				}
				if !mcp.OAuthTokensPresent(ictx, flow.ServerName) {
					return errors.New("The server responded, but no OAuth token was obtained — " +
						"this provider may require a manually-registered OAuth client.")
				}
				if _, err := mcp.SaveServer(ictx, mcp.HeadlessUI(), flow.ServerName, cfg); err != nil {
					return err
				}
				flow.SetTools(summaries)
				if err := flow.MarkApproved(); err != nil {
					return err
				}
				if flow.ReconnectLive {
					mcp.ReconnectServer(ictx, flow.ServerName)
				}
				return nil
			}()
		}
		if err != nil {
			_ = storage.Restore(home.WithOverride(ictx, storage.HomeDir()), backup, true)
			manager.RestoreEntry(ictx, flow.ServerName, previous, flow.HomeDir)
		}
		return err
	}()
	if err == nil {
		return
	}
	msg := mcp.ExceptionMessage(err)
	serverURL, _ := cfg["url"].(string)
	if human, ok := mcp.HumanizeOAuthRegistrationError(flow.ServerName, err.Error(), serverURL); ok && human != "" {
		msg = human
	}
	flow.MarkError(msg, false)
}
