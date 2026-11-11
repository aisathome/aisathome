# SPDX-License-Identifier: Apache-2.0
"""The model's container, hardened: networks, the egress forwarder, overlays, limits, the run command.

Network. Every session gets its own *internal* Docker network (`session_network`): no route out, Docker's
DNS there answers no outside names, and no other session's container is on it. The one way out is the
egress forwarder of the session's endpoint (egress_forward.py), a container `<PREFIX>-egress-<host>-<port>-<hash>`
on EGRESS_NET (an ordinary bridge) that is connected to each session network that uses that endpoint (one
forwarder per endpoint URL and key). The sandbox's /etc/hosts maps the endpoint's host name to the
forwarder's address on its own network, and the model's client talks plain HTTP to it (`model_url`); the
forwarder adds the API key and sends the request to that one endpoint over TLS, and nowhere else. So a
session reaches only its own endpoint, never another session, and never sees the key: it stays in the
forwarder's container. A literal IP address or a loopback name as the endpoint cannot be mapped this way
and is refused (use a host name). A forwarder that no session network uses any more is removed when the
last session closes (`remove_session_network`), so no forwarder keeps an old key.

On a shared host, a host firewall should also drop traffic from the session bridges to the host itself and
every container's traffic to the cloud's instance metadata service. Session bridges are named
`<BRIDGE_PREFIX><10 hex>` (15 characters at most), so a firewall matches them by prefix (iptables
`-i hsb+`); the egress bridge is EGRESS_BRIDGE.

Shared build state. A shared cache and a tree's build directory can be mounted as per-session overlays
(`overlay_mount`): the model reads the shared files, and whatever it writes lands in the session's own
upper directory and never reaches the shared inode.

Limits: memory (no swap) and a process cap per container (`limits`).

The names are module settings (`configure`). Standard library only.
"""
from __future__ import annotations

import fcntl
import hashlib
import ipaddress
import json
import os
import re
import shutil
import subprocess
import tempfile
import time
import urllib.parse
from pathlib import Path

PREFIX = "harness"                     # container, network and label names start with it
BRIDGE_PREFIX = "hsb"                  # session bridges: hsb<10 hex>
EGRESS_NET = "harness-egress-out"      # ordinary bridge: only the forwarders join it
EGRESS_BRIDGE = "hsb-egress0"
FORWARDER = Path(__file__).resolve().parent / "egress_forward.py"
LOCK = Path(tempfile.gettempdir()) / "aisathome-harness-egress.lock"
HARDEN = ["--cap-drop=ALL", "--security-opt", "no-new-privileges"]
LOOPBACK = ("localhost", "localhost.localdomain", "ip6-localhost")
FORWARD_PORT = 80                      # the forwarder's plain-HTTP port on the session network
KEY_IN_FORWARDER = "/run/egress/key"   # the key file inside the forwarder's container (never the model's)
SETTINGS = ("PREFIX", "BRIDGE_PREFIX", "EGRESS_NET", "EGRESS_BRIDGE", "FORWARDER", "LOCK")


def configure(**settings):
    """Set module settings (PREFIX, BRIDGE_PREFIX, EGRESS_NET, EGRESS_BRIDGE, FORWARDER, LOCK) for this
    process; unknown names are an error."""
    g = globals()
    for k, v in settings.items():
        if k not in SETTINGS:
            raise KeyError(f"unknown sandbox setting {k}")
        g[k] = v


def _docker(*args: str, check: bool = True) -> subprocess.CompletedProcess:
    p = subprocess.run(["docker", *args], capture_output=True, text=True)
    if check and p.returncode != 0:
        raise RuntimeError(f"docker {' '.join(args[:3])} failed: {p.stderr.strip()[-500:]}")
    return p


def slug(text: str, n: int = 40) -> str:
    return re.sub(r"[^a-z0-9.-]+", "-", text.lower()).strip("-.")[:n] or "x"


def _ensure_network(name: str, bridge: str, internal: bool):
    p = _docker("network", "inspect", name, "--format", "{{.Internal}}", check=False)
    if p.returncode != 0:
        _docker("network", "create", *(["--internal"] if internal else []),
                "-o", f"com.docker.network.bridge.name={bridge}", "--label", f"{PREFIX}.network=1", name, check=False)
        p = _docker("network", "inspect", name, "--format", "{{.Internal}}")
    if (p.stdout.strip() == "true") != internal:
        raise RuntimeError(f"Docker network {name} has internal={p.stdout.strip()}, want {internal}")


def session_network(key: str) -> tuple[str, str]:
    """(network name, bridge name) of a session; `key` is unique per session on this host."""
    h = hashlib.sha256(key.encode()).hexdigest()
    return f"{PREFIX}-sbx-{slug(key, 30)}-{h[:8]}", f"{BRIDGE_PREFIX}{h[:10]}"


def ensure_session_network(key: str) -> str:
    """Create the session's internal network if missing; refuse one that is not internal."""
    name, bridge = session_network(key)
    _ensure_network(name, bridge, internal=True)
    return name


def remove_session_network(key: str):
    """Disconnect whatever is still attached (the forwarders), remove the session's network, and remove
    the forwarders that no session network uses any more."""
    name, _ = session_network(key)
    p = _docker("network", "inspect", name, "--format", "{{json .Containers}}", check=False)
    if p.returncode != 0:
        return
    for c in (json.loads(p.stdout.strip() or "{}") or {}).values():
        _docker("network", "disconnect", "-f", name, c.get("Name", ""), check=False)
    _docker("network", "rm", name, check=False)
    drop_idle_forwarders()


def drop_idle_forwarders():
    """Remove every forwarder attached to no session network (only to EGRESS_NET): it serves no session
    and holds a key. Under the egress lock, so a session that is attaching one is never cut off."""
    p = _docker("ps", "-a", "--filter", f"label={_label()}", "--format", "{{.Names}}", check=False)
    if p.returncode != 0:
        return
    with _egress_lock():
        for name in p.stdout.split():
            f = _forwarder(name)
            if f is not None and not set(f["networks"]) - {EGRESS_NET}:
                _docker("rm", "-f", name, check=False)


def target_of(base_url: str) -> tuple[str, int]:
    """(host, port) of the endpoint URL. A literal IP address or a loopback name is refused: the sandbox
    reaches its endpoint through a host name mapped to the forwarder, and an address would bypass it."""
    m = re.match(r"(https?)://(\[[^\]]*\]|[^/:]+)(?::(\d+))?", base_url)
    if not m:
        raise ValueError(f"cannot parse base URL {base_url!r}")
    host = m.group(2).strip("[]")
    try:
        ipaddress.ip_address(host)
        literal = True
    except ValueError:
        literal = False
    if literal or host.lower() in LOOPBACK:
        raise ValueError(f"endpoint {base_url!r}: use a host name, not an IP address or a loopback name "
                         "(the sandbox reaches the endpoint only through its forwarder, by name)")
    return host, int(m.group(3) or (443 if m.group(1) == "https" else 80))


def endpoint_of(base_url: str) -> tuple[str, str, int, str]:
    """(scheme, host, port, base path) of the endpoint URL; the host as `target_of` allows it."""
    host, port = target_of(base_url)
    u = urllib.parse.urlsplit(base_url)
    if u.query or u.fragment or u.username or u.password:
        raise ValueError(f"endpoint {base_url!r}: no user, query or fragment in the base URL")
    return u.scheme, host, port, "/" + u.path.strip("/") if u.path.strip("/") else "/"


def model_url(base_url: str) -> str:
    """The base URL the model's client uses: plain HTTP to the endpoint's name (mapped to its forwarder,
    which adds the key and speaks TLS to the endpoint), same base path."""
    _, host, _, path = endpoint_of(base_url)
    port = "" if FORWARD_PORT == 80 else f":{FORWARD_PORT}"
    return f"http://{host}{port}{'' if path == '/' else path}"


def key_fingerprint(key_file: str | None) -> str:
    """A short hash of the key (not the key): a changed key gets a forwarder of its own."""
    if not key_file:
        return "nokey"
    return hashlib.sha256(b"aisathome-harness-key\0" + Path(key_file).read_bytes().strip()).hexdigest()[:16]


def forwarder_name(host: str, port: int, extra: str = "") -> str:
    """One forwarder per endpoint host AND port (and `extra`: the scheme, base path and key): two endpoints
    on one host never share it. The name ends in a hash of all of it, so two long host names that share
    their first characters (the slug is cut) never share a forwarder either."""
    h = hashlib.sha256(f"{host.lower()}:{port}|{extra}".encode()).hexdigest()[:10]
    return f"{PREFIX}-egress-{slug(host, 30).rstrip('-.')}-{port}-{h}"


def _version() -> str:
    return hashlib.sha256(Path(FORWARDER).read_bytes()).hexdigest()[:12]


def _label() -> str:
    return f"{PREFIX}.egress.version"


def _target_label() -> str:
    return f"{PREFIX}.egress.target"


def _key_label() -> str:
    return f"{PREFIX}.egress.key"


class _egress_lock:
    def __enter__(self):
        lock = Path(LOCK)
        lock.parent.mkdir(parents=True, exist_ok=True)
        self.fh = open(lock, "w")
        fcntl.flock(self.fh, fcntl.LOCK_EX)

    def __exit__(self, *exc):
        self.fh.close()


def _forwarder(name: str) -> dict | None:
    """{"running", "networks": {name: address}, "labels"} of a forwarder container, or None when there is none."""
    p = _docker("inspect", "-f", "{{.State.Running}}\t{{json .NetworkSettings.Networks}}\t{{json .Config.Labels}}",
                name, check=False)
    if p.returncode != 0:
        return None
    running, nets, labels = p.stdout.strip().split("\t")
    return {"running": running == "true", "labels": json.loads(labels) or {},
            "networks": {k: (v or {}).get("IPAddress") for k, v in (json.loads(nets) or {}).items()}}


def forwarder_for(base_url: str, key_file: str | None = None) -> str:
    """The forwarder's name for an endpoint URL and key file."""
    scheme, host, port, path = endpoint_of(base_url)
    return forwarder_name(host, port, f"{scheme}|{path}|{key_fingerprint(key_file)}")


def ensure_egress(base_url: str, image: str, network: str, key_file: str | None = None) -> list[str]:
    """Start (or reuse) the forwarder for the endpoint and its key, attach it to the session's `network`
    and return the sandbox's --add-host args. `image` runs the forwarder (any image with python3 and CA
    certificates). The key file is mounted into the forwarder only, which runs as the calling user so it
    can read a key file only that user may read. A forwarder of an older version, or for another target or
    key, is replaced."""
    scheme, host, port, path = endpoint_of(base_url)
    target, url = f"{host}:{port}", f"{scheme}://{host}:{port}{path}"
    fp = key_fingerprint(key_file)
    _ensure_network(EGRESS_NET, EGRESS_BRIDGE, internal=False)
    name = forwarder_for(base_url, key_file)
    key = ["-v", f"{key_file}:{KEY_IN_FORWARDER}:ro"] if key_file else []
    user = f"{os.getuid()}:{os.getgid()}" if key_file else "65534:65534"
    with _egress_lock():
        f = _forwarder(name)
        if not f or not f["running"] or f["labels"].get(_label()) != _version() \
                or f["labels"].get(_target_label()) != url or f["labels"].get(_key_label()) != fp:
            _docker("rm", "-f", name, check=False)
            _docker("run", "-d", "--name", name, "--restart", "unless-stopped",
                    "--label", f"{_label()}={_version()}", "--label", f"{_target_label()}={url}",
                    "--label", f"{_key_label()}={fp}",
                    "--network", EGRESS_NET, "-u", user, *HARDEN,
                    "--read-only", "--memory", "128m", "--memory-swap", "128m", "--pids-limit", "64",
                    "--sysctl", "net.ipv4.ip_unprivileged_port_start=0",
                    "-v", f"{FORWARDER}:/opt/egress_forward.py:ro", *key, "--entrypoint", "python3", image,
                    "-I", "/opt/egress_forward.py", "--listen", f"0.0.0.0:{FORWARD_PORT}", "--target", target,
                    "--scheme", scheme, "--path-prefix", path,
                    *(["--key-file", KEY_IN_FORWARDER] if key_file else []))
            f = _forwarder(name)
        if f is not None and network not in f["networks"]:
            _docker("network", "connect", network, name)
        ip = None
        for _ in range(20):
            f = _forwarder(name)
            ip = f and f["running"] and f["networks"].get(network)
            if ip:
                break
            time.sleep(0.5)
        if not ip:
            raise RuntimeError(f"egress forwarder {name} is not up on {network}: "
                               + _docker("logs", "--tail", "20", name, check=False).stderr[-800:])
    return ["--add-host", f"{host}:{ip}"]


def overlay_mount(volume: str, lower: Path, state: Path, target: Path) -> list[str]:
    """A per-session copy-on-write view of `lower` at `target`: a local-driver volume of type overlay
    (Docker's daemon mounts it while the container runs). Writes go to state/upper. `lower` must exist
    and be checked by the caller when the model can write it (container.Sandbox does: the daemon mounts
    as root and follows symlinks)."""
    upper, work = state / "upper", state / "work"
    upper.mkdir(parents=True, exist_ok=True)
    work.mkdir(parents=True, exist_ok=True)
    if not lower.is_dir():
        raise ValueError(f"overlay lower directory {lower} is missing")
    for p in (lower, upper, work):
        if "," in str(p) or ":" in str(p):
            raise ValueError(f"overlay path {p} contains ',' or ':'")
    # recreate: an existing volume keeps the options it was created with
    _docker("volume", "rm", "-f", volume)
    _docker("volume", "create", "--driver", "local", "--opt", "type=overlay", "--opt", "device=overlay",
            "--opt", f"o=lowerdir={lower},upperdir={upper},workdir={work}", volume)
    return ["--mount", f"type=volume,src={volume},dst={target},volume-nocopy"]


def drop_overlays(volumes: list[str], state_root: Path | None = None, image: str | None = None):
    """Remove overlay volumes and, with `state_root`, their files (disk). overlayfs leaves root-owned,
    mode 000 `work/work` directories that the invoking user can neither list nor empty (a spot reboot
    in the middle of a session leaves them with files in them); with `image`, what an ordinary removal
    leaves goes by `remove_as_root`. Without it, the leftovers stay, as before."""
    for v in volumes:
        _docker("volume", "rm", "-f", v, check=False)
    if state_root is not None and state_root.exists():
        shutil.rmtree(state_root, ignore_errors=True)
        if image and (state_root.exists() or state_root.is_symlink()):
            remove_as_root(state_root, image)


def remove_as_root(path: Path, image: str) -> bool:
    """Remove the host-only directory `path` as root in a throwaway container: no network, every
    capability dropped except DAC_OVERRIDE (to enter and empty mode-000 directories), no new
    privileges, only `path`'s parent mounted, and `rm -rf --one-file-system`, which follows no symlink
    and crosses no mount. `image` needs `rm`. Never for the model's tree or HOME (the caller's rule; its
    state directory holds the overlays' files). A symlink at `path` is not followed, only unlinked.
    True when `path` is gone."""
    path = Path(path)
    if path.is_symlink():
        path.unlink()
        return True
    if not path.exists():
        return True
    parent = path.parent.resolve()
    if "," in str(parent) or ":" in str(parent) or path.name in ("", ".", ".."):
        raise ValueError(f"cannot remove {path} as root: its path contains ',' or ':'")
    _docker("run", "--rm", "--network", "none", "-u", "0:0", "--cap-drop=ALL", "--cap-add=DAC_OVERRIDE",
            "--security-opt", "no-new-privileges", "--mount", f"type=bind,src={parent},dst=/drop",
            "--entrypoint", "rm", image, "-rf", "--one-file-system", "--", f"/drop/{path.name}", check=False)
    return not path.exists()


def limits(policy: dict) -> list[str]:
    """Memory (swap off) and process caps: policy {"docker_memory_gb": int, "docker_pids": int}."""
    mem = f"{policy['docker_memory_gb']}g"
    return ["--memory", mem, "--memory-swap", mem, "--pids-limit", str(policy["docker_pids"])]


def sinkhole(hosts: list[str], address: str = "127.0.0.9") -> list[str]:
    """--add-host args that send each name to an address where nothing listens: a second wall for the
    hosts a model is most likely to try (code hosting, the project's own sites)."""
    return sum((["--add-host", f"{h}:{address}"] for h in hosts), [])


def run_args(name: str, image: str, workdir: Path, command: list[str], *, network: str, user: str = "1000:1000",
             cpu: list[str] = (), limit: list[str] = (), hosts: list[str] = (), volumes: list[str] = (),
             env: list[str] = ()) -> list[str]:
    """`docker run` for the model's container: removed on exit, a fixed name (so a stop can find it),
    an unprivileged user, all capabilities dropped and no new privileges, the internal network."""
    return (["docker", "run", "--rm", "--name", name, "-u", user, "--network", network]
            + HARDEN + list(cpu) + list(limit) + list(hosts) + list(volumes) + list(env)
            + ["-w", str(workdir), image] + list(command))
