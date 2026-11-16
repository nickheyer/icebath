# Prints key, enabled flag, source, kind and error of every plugin the reference manager holds after discovery
import sys
from hermes_cli.plugins import get_plugin_manager
m = get_plugin_manager()
m.discover_and_load()
for key, p in sorted(m._plugins.items()):
    mf = p.manifest
    print("%s\t%s\t%s\t%s\t%s" % (key, int(bool(p.enabled)), mf.source, getattr(mf, "kind", ""), (p.error or "")[:80]))
