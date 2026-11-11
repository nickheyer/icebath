package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Params of a verb as the plain dict the delegated owner operation reads
func plainParams(params *pyjson.Object) map[string]any {
	plain, _ := pyjson.Plain(params).(map[string]any)
	if plain == nil {
		plain = map[string]any{}
	}
	return plain
}

// Answer of a delegated operation, None for a nil dict
func delegatedAnswer(result map[string]any, err error) (any, error) {
	if err != nil || result == nil {
		return nil, err
	}
	return result, nil
}

// Cron recover verb, run as the connection's actor on its authority
func (c *AuthorityConnection) cronRecover(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
	return delegatedAnswer(CronRPC(ctx, c, "recover", plainParams(params)))
}

// Cron submit verb, run as the connection's actor on its authority
func (c *AuthorityConnection) cronSubmit(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
	return delegatedAnswer(CronRPC(ctx, c, "submit", plainParams(params)))
}

// Cron status verb, run as the connection's actor on its authority
func (c *AuthorityConnection) cronStatus(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
	return delegatedAnswer(CronRPC(ctx, c, "status", plainParams(params)))
}

// Cron cancel verb, run as the connection's actor on its authority
func (c *AuthorityConnection) cronCancel(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
	return delegatedAnswer(CronRPC(ctx, c, "cancel", plainParams(params)))
}

// Bot relay deliver verb, one Bot Chat message as tracked owner work
func (c *AuthorityConnection) botDeliver(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
	return delegatedAnswer(BotDeliver(ctx, c.authority, c.actor, plainParams(params)))
}

// Bot relay settle verb, a delivery the sender was unsure of settled
func (c *AuthorityConnection) botSettle(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
	return delegatedAnswer(BotSettle(ctx, c.authority, c.actor, plainParams(params)))
}

// Bot relay roster sync verb
func (c *AuthorityConnection) botRoster(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
	return relayOperation(ctx, c, "roster", params)
}

// Bot relay outbox drain verb
func (c *AuthorityConnection) botOutbox(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
	return relayOperation(ctx, c, "outbox", params)
}

// Bot relay reply verb
func (c *AuthorityConnection) botReply(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
	return relayOperation(ctx, c, "reply", params)
}

// Answer of a worker operation, None for a nil dict
func workerAnswer(result *pyjson.Object, err error) (any, error) {
	if err != nil || result == nil {
		return nil, err
	}
	return result, nil
}

// Worker register verb of a managed worker
func (c *AuthorityConnection) workerRegister(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
	return workerAnswer(workerRequest(ctx, c, ref, params, "register"))
}

// Worker adopt verb of a managed worker
func (c *AuthorityConnection) workerAdopt(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
	return workerAnswer(workerRequest(ctx, c, ref, params, "adopt"))
}

// Worker persist verb of a managed worker
func (c *AuthorityConnection) workerPersist(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
	return workerAnswer(workerRequest(ctx, c, ref, params, "persist"))
}
