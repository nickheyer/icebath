package plugins_test

import ()

const (
	memoryCatalogHook     = "def register(ctx):\n    ctx.register_hook('pre_llm_call', print)\n"
	memoryCatalogProvider = "def register(ctx):\n    ctx.register_memory_provider(object())\n"
)
