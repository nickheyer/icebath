import importlib
import os
import site
import sys

ICEBATH_ENTRY_MODULE = "__ICEBATH_ENTRY_MODULE__"
ICEBATH_ENTRY_FUNC = "__ICEBATH_ENTRY_FUNC__"
ICEBATH_REPO_REL = "__ICEBATH_REPO_REL__"
ICEBATH_SITE_REL = "__ICEBATH_SITE_REL__"


def _join_rel(here, rel):
    return os.path.join(here, *rel.split("/"))


def launcher_dir(argv0):
    return os.path.dirname(os.path.abspath(argv0))


def payload_sys_paths(here):
    return [_join_rel(here, ICEBATH_REPO_REL), _join_rel(here, ICEBATH_SITE_REL)]


def default_pycache_dir(environ):
    if sys.platform == "win32":
        base = environ.get("LOCALAPPDATA")
        return os.path.join(base, "icebath", "pycache") if base else None
    base = environ.get("HOME")
    return os.path.join(base, ".cache", "icebath-pycache") if base else None


def configure(here, environ=None):
    environ = os.environ if environ is None else environ
    environ.pop("PYTHONPATH", None)
    environ.pop("PYTHONHOME", None)
    repo_entry, site_entry = payload_sys_paths(here)
    sys.path.insert(0, repo_entry)
    site.addsitedir(site_entry)
    if site_entry in sys.path:
        sys.path.remove(site_entry)
    sys.path.insert(1 if sys.path and sys.path[0] == repo_entry else 0, site_entry)
    if not environ.get("PYTHONPYCACHEPREFIX"):
        if not os.path.exists(os.path.join(os.path.dirname(repo_entry), ".icebath-baked-pycache")):
            default = default_pycache_dir(environ)
            if default:
                environ["PYTHONPYCACHEPREFIX"] = default
                sys.pycache_prefix = default
    return [repo_entry, site_entry]


def main(argv=None):
    argv = list(sys.argv if argv is None else argv)
    here = launcher_dir(argv[0] if argv else sys.argv[0])
    configure(here)
    importlib.import_module("icebath_bootstrap")
    module = importlib.import_module(ICEBATH_ENTRY_MODULE)
    target = getattr(module, ICEBATH_ENTRY_FUNC)
    return target()


if __name__ == "__main__":
    sys.exit(main())
