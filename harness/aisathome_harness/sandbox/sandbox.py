# SPDX-License-Identifier: Apache-2.0
"""The model's container, hardened: networks, the egress forwarder, overlays, limits, the run command.

Network. Every session gets its own *internal* Docker network (`session_network`): no route out, Docker's
DNS there answers no outside names, and no other session's container is on it. The one way out is the
egress forwarder of the session's endpoint (egress_forward.py), a container `<PREFIX>-egress-<host>-<port>`
on EGRESS_NET (an ordinary bridge) that is connected to each session network that uses that endpoint. The
sandbox's /etc/hosts maps the endpoint's host name to the forwarder's address on its own network; the
forwarder passes the TCP stream (TLS end to end) to that one host and port and nowhere else. So a session
reaches only its own endpoint, and never another session. A literal IP address or a loopback name as the
endpoint cannot be mapped this way and is refused (use a host name).

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
import re
import shutil
import subprocess
import tempfile
import time
from pathlib import Path

PREFIX = "harness"                     # container, network and label names start with it
BRIDGE_PREFIX = "hsb"                  # session bridges: hsb<10 hex>
EGRESS_NET = "harness-egress-out"      # ordinary bridge: only the forwarders join it
EGRESS_BRIDGE = "hsb-egress0"
FORWARDER = Path(__file__).resolve().parent / "egress_forward.py"
LOCK = Path(tempfile.gettempdir()) / "aisathome-harness-egress.lock"
HARDEN = ["--cap-drop=ALL", "--security-opt", "no-new-privileges"]
LOOPBACK = ("localhost", "localhost.localdomain", "ip6-localhost")
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
    """Disconnect whatever is still attached (the forwarders) and remove the session's network."""
    name, _ = session_network(key)
    p = _docker("network", "inspect", name, "--format", "{{json .Containers}}", check=False)
    if p.returncode != 0:
        return
    for c in (json.loads(p.stdout.strip() or "{}") or {}).values():
        _docker("network", "disconnect", "-f", name, c.get("Name", ""), check=False)
    _docker("network", "rm", name, check=False)


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


def forwarder_name(host: str, port: int) -> str:
    """One forwarder per endpoint host AND port: two endpoints on one host never share it."""
    return f"{PREFIX}-egress-{slug(host)}-{port}"


def _version() -> str:
    return hashlib.sha256(Path(FORWARDER).read_bytes()).hexdigest()[:12]


def _label() -> str:
    return f"{PREFIX}.egress.version"


def _target_label() -> str:
    return f"{PREFIX}.egress.target"


def _forwarder(name: str) -> dict | None:
    """{"running", "networks": {name: address}, "labels"} of a forwarder container, or None when there is none."""
    p = _docker("inspect", "-f", "{{.State.Running}}\t{{json .NetworkSettings.Networks}}\t{{json .Config.Labels}}",
                name, check=False)
    if p.returncode != 0:
        return None
    running, nets, labels = p.stdout.strip().split("\t")
    return {"running": running == "true", "labels": json.loads(labels) or {},
            "networks": {k: (v or {}).get("IPAddress") for k, v in (json.loads(nets) or {}).items()}}


def ensure_egress(base_url: str, image: str, network: str) -> list[str]:
    """Start (or reuse) the forwarder for the endpoint, attach it to the session's `network` and return
    the sandbox's --add-host args. `image` runs the forwarder (any image with python3). A forwarder of an
    older version, or for another target, is replaced."""
    host, port = target_of(base_url)
    target = f"{host}:{port}"
    _ensure_network(EGRESS_NET, EGRESS_BRIDGE, internal=False)
    name = forwarder_name(host, port)
    lock = Path(LOCK)
    lock.parent.mkdir(parents=True, exist_ok=True)
    with open(lock, "w") as lk:
        fcntl.flock(lk, fcntl.LOCK_EX)
        f = _forwarder(name)
        if not f or not f["running"] or f["labels"].get(_label()) != _version() \
                or f["labels"].get(_target_label()) != target:
            _docker("rm", "-f", name, check=False)
            _docker("run", "-d", "--name", name, "--restart", "unless-stopped",
                    "--label", f"{_label()}={_version()}", "--label", f"{_target_label()}={target}",
                    "--network", EGRESS_NET, "-u", "65534:65534", *HARDEN,
                    "--read-only", "--memory", "128m", "--memory-swap", "128m", "--pids-limit", "64",
                    "--sysctl", "net.ipv4.ip_unprivileged_port_start=0",
                    "-v", f"{FORWARDER}:/opt/egress_forward.py:ro", "--entrypoint", "python3", image,
                    "-I", "/opt/egress_forward.py", "--listen", f"0.0.0.0:{port}", "--target", target)
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


def drop_overlays(volumes: list[str], state_root: Path | None = None):
    """Remove overlay volumes and, with `state_root`, their files (disk)."""
    for v in volumes:
        _docker("volume", "rm", "-f", v, check=False)
    if state_root is not None and state_root.exists():
        shutil.rmtree(state_root, ignore_errors=True)


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
