package provider

import "context"

// Parameter rung ladder for one caller owned send, returning the recovered response or the narrowed error
func auxParameterLadder(firstErr error, client AuxClient, kwargs map[string]any, task, tag string) func(yield auxLadderYield) (any, error) {
	return func(yield auxLadderYield) (any, error) {
		baseInfo := ""
		if client != nil {
			baseInfo = client.BaseURL()
		}
		route := auxLadderRoute{Client: client, Task: task, Tag: tag, AsyncMode: tag != "", BaseInfo: baseInfo}
		maxTokens := kwargs["max_tokens"]
		if !auxTruthy(maxTokens) {
			maxTokens = kwargs["max_completion_tokens"]
		}
		resp, err, _, raised := auxLadderParameterRungs(yield, firstErr, route, kwargs, maxTokens)
		if raised != nil {
			return nil, raised
		}
		if err == nil {
			return resp, nil
		}
		return nil, err
	}
}

// Sends once and retries a parameter 400 through the parameter rungs, other errors propagating unchanged
func SendWithParameterRungs(ctx context.Context, send func(ctx context.Context, client AuxClient, kwargs map[string]any) (any, error),
	client AuxClient, kwargs map[string]any, task string) (any, error) {
	return auxSendWithParameterRungs(ctx, send, client, kwargs, task, "")
}

// Async twin of the parameter rung send, tagging its log lines as async
func SendWithParameterRungsAsync(ctx context.Context, send func(ctx context.Context, client AuxClient, kwargs map[string]any) (any, error),
	client AuxClient, kwargs map[string]any, task string) (any, error) {
	return auxSendWithParameterRungs(ctx, send, client, kwargs, task, " (async)")
}

// Shared body of the sync and async parameter rung sends
func auxSendWithParameterRungs(ctx context.Context, send func(ctx context.Context, client AuxClient, kwargs map[string]any) (any, error),
	client AuxClient, kwargs map[string]any, task, tag string) (any, error) {
	resp, firstErr := send(ctx, client, kwargs)
	if firstErr == nil || auxIsBaseException(firstErr) {
		return resp, firstErr
	}
	perform := func(ctx context.Context, step auxLadderStep) (any, error) {
		stepClient, _ := step.Args[0].(AuxClient)
		stepKwargs, _ := step.Args[1].(map[string]any)
		return send(ctx, stepClient, stepKwargs)
	}
	return auxDriveLadder(ctx, auxParameterLadder(firstErr, client, kwargs, task, tag), perform)
}
