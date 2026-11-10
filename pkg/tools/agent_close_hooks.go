package tools

import "context"

// Closes the browser sessions serving a task, installed by the browser package init
var CleanupBrowser func(ctx context.Context, env *Env, taskID string) error

// Reports browser.headed, installed by the browser package init
var IsHeadedMode func(env *Env) bool

// Releases one session owned computer_use backend, installed by the codeexec package init
var ReleaseComputerUseSession func(ctx context.Context, sessionID string) bool
