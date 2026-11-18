package plugins

const hookSourcePlugin = `
def _hook(**kwargs):
    return None


def register(ctx):
    ctx.register_hook("on_session_start", _hook)
`
