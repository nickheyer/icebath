"""Login backend contract of the Icebath browser credential vault that the plugin SDK ships.

Every backend hands the agent the same shape (opaque handle + login metadata) and resolves the
password server-side at fill time only. A plugin backend subclasses :class:`LoginBackend` and registers
through ``PluginContext.register_login_backend()``; the enabled backend set is owned by the Icebath process.
"""

from agent.vault_backends.base import LoginBackend, UnlockRequired, run_with_secret_env, run_with_stdin_secret

__all__ = ["LoginBackend", "UnlockRequired", "run_with_secret_env", "run_with_stdin_secret"]
