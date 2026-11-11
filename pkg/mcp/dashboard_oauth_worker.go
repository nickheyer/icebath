package mcp

import (
	"context"
	"errors"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// One lock per home and server serializing dashboard OAuth transactions
var dashboardOAuthTransactions = struct {
	sync.Mutex
	locks map[[2]string]*sync.Mutex
}{locks: map[[2]string]*sync.Mutex{}}

// Transaction lock of the flow home and server, created on first use
func dashboardOAuthTransaction(flow *DashboardOAuthFlow) *sync.Mutex {
	key := [2]string{flow.HomeDir, flow.ServerName}
	dashboardOAuthTransactions.Lock()
	defer dashboardOAuthTransactions.Unlock()
	lock, ok := dashboardOAuthTransactions.locks[key]
	if !ok {
		lock = &sync.Mutex{}
		dashboardOAuthTransactions.locks[key] = lock
	}
	return lock
}

// Tool name and description rows the flow approved, guarded for the status poller
func (f *DashboardOAuthFlow) ToolsSnapshot() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Tools
}

// Runs the dashboard MCP OAuth probe under the flow home and secret scope, rolling tokens back on failure
func RunDashboardMCPOAuth(ctx context.Context, env *tools.Env, flow *DashboardOAuthFlow, cfg map[string]any) {
	defer flow.MarkWorkerDone()
	err := func() error {
		wctx := home.WithOverride(ctx, flow.HomeDir)
		wctx = provider.WithSecretScope(wctx, provider.BuildProfileSecretScope(flow.HomeDir), flow.HomeDir)
		transaction := dashboardOAuthTransaction(flow)
		transaction.Lock()
		defer transaction.Unlock()
		ictx := withDashboardOAuthFlow(ForceInteractiveOAuth(wctx), flow)
		manager := getManager()
		storage := NewTokenStorage(ictx, flow.ServerName, "")
		backup := storage.Snapshot()
		var previous *providerEntry
		err := func() error {
			prev, err := manager.remove(ictx, flow.ServerName, flow.HomeDir)
			if err != nil {
				return err
			}
			previous = prev
			timeout := LoginConnectTimeout(cfg)
			summaries, err := probeServer(ictx, env, flow.ServerName, cfg, &timeout, nil)
			if err != nil {
				return err
			}
			if !oauthTokensPresent(ictx, flow.ServerName) {
				return errors.New("The server responded, but no OAuth token was obtained — " +
					"this provider may require a manually-registered OAuth client.")
			}
			if _, err := SaveServer(ictx, headlessUI{}, flow.ServerName, cfg); err != nil {
				return err
			}
			flow.mu.Lock()
			flow.Tools = summaryDicts(summaries)
			flow.mu.Unlock()
			if err := flow.MarkApproved(); err != nil {
				return err
			}
			if flow.ReconnectLive {
				ReconnectServer(ictx, flow.ServerName)
			}
			return nil
		}()
		if err != nil {
			_ = storage.Restore(home.WithOverride(ictx, storage.homeDir), backup, true)
			manager.restoreEntry(ictx, flow.ServerName, previous, flow.HomeDir)
		}
		return err
	}()
	if err == nil {
		return
	}
	msg := ExceptionMessage(err)
	serverURL, _ := cfg["url"].(string)
	if human, ok := HumanizeOAuthRegistrationError(flow.ServerName, err.Error(), serverURL); ok && human != "" {
		msg = human
	}
	flow.MarkError(msg, false)
}
