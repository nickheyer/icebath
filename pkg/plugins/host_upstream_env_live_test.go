package plugins

import ()

// Plugin tool reporting the upstream names of the home and of a mapped icebath variable
const upstreamEnvPlugin = `
import json
import os


def register(ctx):
    ctx.register_tool(
        "env_probe", "probe",
        {"name": "env_probe", "description": "d", "parameters": {"type": "object", "properties": {}}},
        lambda args, **kw: json.dumps({"home": os.environ.get("HERMES_HOME"), "flag": os.environ.get("HERMES_PROBE_FLAG")}),
    )
`
