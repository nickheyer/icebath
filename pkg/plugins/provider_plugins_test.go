package plugins

import (
	"path/filepath"
	"testing"
)

const providerEntryPointModule = `
import os
from providers import register_provider
from providers.base import ProviderProfile

class Acme(ProviderProfile):
    def build_extra_body(self, *, session_id=None, **context):
        return {"pid": os.getpid(), "session_id": session_id}

register_provider(ProviderProfile(name="acme-module", base_url="https://module.example/v1"))

def register():
    register_provider(Acme(name="acme-call", base_url="https://call.example/v1"))

def general(ctx):
    raise AssertionError("a general plugin target is never called by the provider scan")

def optional(extra=None):
    register_provider(ProviderProfile(name="acme-optional", base_url="https://optional.example/v1"))

def explodes():
    register_provider(ProviderProfile(name="acme-before-raise", base_url="https://raise.example/v1"))
    raise RuntimeError("invoke boom")
`

const providerEntryPointBroken = `
from providers import register_provider
from providers.base import ProviderProfile
register_provider(ProviderProfile(name="acme-partial", base_url="https://partial.example/v1"))
raise ImportError("half imported")
`

const createClientPlugin = `
import os
from providers import register_provider
from providers.base import ProviderProfile

class Completions:
    def __init__(self, key):
        self.key = key
    def create(self, **kwargs):
        return {"pid": os.getpid(), "model": kwargs.get("model"), "key": self.key}

class Chat:
    def __init__(self, key):
        self.completions = Completions(key)

class Client:
    def __init__(self, key):
        self.chat = Chat(key)

class OwnClient(ProviderProfile):
    def create_client(self, **client_kwargs):
        return Client(client_kwargs.get("api_key"))

register_provider(ProviderProfile(name="plain", base_url="https://plain.example/v1"))
register_provider(OwnClient(name="ownclient", base_url="https://own.example/v1"))
`

// Writes the two profile model provider plugin whose second profile overrides create_client
func createClientPluginDir(t *testing.T, h string) string {
	t.Helper()
	pluginDir := filepath.Join(h, "plugins", "model-providers", "ownpair")
	memWrite(t, filepath.Join(pluginDir, "plugin.yaml"), "name: ownpair\nkind: model-provider\n")
	memWrite(t, filepath.Join(pluginDir, "__init__.py"), createClientPlugin)
	return pluginDir
}

const streamClientPlugin = `
import os
from providers import register_provider
from providers.base import ProviderProfile

class Chunk:
    def __init__(self, i):
        self.i = i
    def model_dump(self, **kwargs):
        return {"id": f"c-{self.i}", "choices": [{"delta": {"content": str(self.i)}}], "pid": os.getpid()}

class Stream:
    def __init__(self, n, fail):
        self.left = list(range(n))
        self.fail = fail
    def __iter__(self):
        return self
    def __next__(self):
        if not self.left:
            if self.fail:
                raise RuntimeError("stream broke")
            raise StopIteration
        return Chunk(self.left.pop(0))

class Replay:
    def __iter__(self):
        return iter([{"n": 1}, {"n": 2}])

class Completions:
    def create(self, **kwargs):
        if kwargs.get("replay"):
            return Replay()
        return Stream(kwargs.get("n", 0), kwargs.get("fail", False))

class Chat:
    def __init__(self):
        self.completions = Completions()

class Client:
    def __init__(self):
        self.chat = Chat()

class StreamClient(ProviderProfile):
    def create_client(self, **client_kwargs):
        return Client()

register_provider(StreamClient(name="streamer", base_url="https://stream.example/v1"))
`
