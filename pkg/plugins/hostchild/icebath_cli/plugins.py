"""Names plugin code imports from the plugin system; inside the host the context is the remote one."""

from __future__ import annotations

from icebath_cli.plugin_host_child import RemotePluginContext as PluginContext

__all__ = ["PluginContext"]
