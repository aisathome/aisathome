#!/usr/bin/env bash
# The installer-to-agent handoff, end to end: the real install.sh installs the real aisathome-node
# (built from this repository) against stand-in gateways, and the tests check what it leaves in
# /etc/aisathome-node, what the node service was told to do and in which order, which gateway got
# which token, and that the agent itself accepts what was written. Scenarios: a fresh install that
# links, a same-gateway upgrade, a gateway change, a token without its issuer record, an interrupted
# link, the moved-aside token given again, --anonymous over a token, a dry run, and the node binary
# override (one older than the installer is refused).
#
# It runs in a sandbox that cannot touch this machine's node: a user namespace (root inside, this
# user outside), a private mount namespace (/etc is a copy made of links with its own empty
# aisathome-node and systemd/system folders; /opt, /var/lib, /usr/local and /run are empty tmpfs)
# and a network namespace with only its own loopback (the stand-in llama-server on 127.0.0.1:8000
# is not this machine's). systemctl, nvidia-smi and the account commands are stand-ins first on PATH.
#
#   ./node/packaging/linux/handoff_test.sh
#
# Needs Go (to build aisathome-node), unshare (util-linux), ip (iproute2) and python3. Without
# unprivileged user namespaces it skips, unless AISATHOME_HANDOFF_REQUIRED=1 (CI).
#
# SPDX-License-Identifier: Apache-2.0
set -uo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
repo="$(cd "$here/../../.." && pwd)"

if [[ "${AISATHOME_HANDOFF_NS:-}" != 1 ]]; then
  skip() {
    if [[ "${AISATHOME_HANDOFF_REQUIRED:-}" = 1 ]]; then echo "handoff_test: $1" >&2; exit 1; fi
    echo "handoff_test: skipped: $1"; exit 0
  }
  command -v unshare >/dev/null || skip "no unshare"
  command -v ip >/dev/null || skip "no ip (iproute2)"
  command -v go >/dev/null || skip "no go"
  unshare -rmn true 2>/dev/null || skip "no unprivileged user namespaces (unshare -rmn)"
  tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
  mkdir -p "$tmp/bin"
  (cd "$repo" && CGO_ENABLED=0 go build -o "$tmp/bin/aisathome-node" ./node/cmd/aisathome-node) || { echo "handoff_test: go build failed" >&2; exit 1; }
  AISATHOME_HANDOFF_NS=1 HANDOFF_TMP="$tmp" unshare -rmn --propagation private bash "$0"
  exit $?
fi

# ---- Inside the sandbox ------------------------------------------------------------------------
tmp="$HANDOFF_TMP"
exec </dev/null # no test reads this terminal by accident: input is given per call
die() { echo "handoff_test: $1" >&2; exit 1; }
[[ $EUID -eq 0 ]] || die "not root inside the user namespace"
ip link set lo up || die "cannot bring up the sandbox's loopback"
[[ -z "$(ip route show default 2>/dev/null)" ]] || die "the sandbox has a default route"

# /etc: links to this machine's entries (through a read-only view of the real /etc at $tmp/host-etc),
# except aisathome-node (absent: a fresh machine) and systemd (its own, with an empty system/ for the
# units). Then the folders the installer writes.
sandbox_etc() {
  local h="$tmp/host-etc" e="$tmp/etc" f
  mkdir -p "$h"; mount --bind /etc "$h" && mount -o remount,bind,ro "$h" || die "cannot mount a view of /etc"
  rm -rf "$e"; mkdir -p "$e/systemd/system"
  for f in "$h"/* "$h"/.[!.]*; do
    [[ -e "$f" || -L "$f" ]] || continue
    case "${f##*/}" in aisathome-node|systemd) continue ;; esac
    ln -s "$f" "$e/${f##*/}"
  done
  for f in "$h"/systemd/*; do [[ "${f##*/}" = system ]] || ln -s "$f" "$e/systemd/${f##*/}"; done
  touch "$e/.handoff-sandbox"
}
sandbox_etc
mount --bind "$tmp/etc" /etc || die "cannot mount the sandbox's /etc"
for d in /opt /var/lib /usr/local /run; do mount -t tmpfs tmpfs "$d" || die "cannot mount a tmpfs on $d"; done
[[ -e /etc/.handoff-sandbox && ! -e /etc/aisathome-node && -z "$(ls -A /etc/systemd/system)" && ! -e /run/dbus ]] ||
  die "the sandbox's folders are not in place"

# ---- Stand-ins -----------------------------------------------------------------------------------
st="$tmp/state"; mkdir -p "$st" "$tmp/stubs"
# systemctl: the node's and llama's ActiveState in $st/<unit>.state. stop/start/restart of the node are
# logged with where the token and the gateway are at that moment. A start of the node honours the unit's
# ConditionFileNotEmpty=token.env.
cat > "$tmp/stubs/systemctl" <<'SH'
#!/usr/bin/env bash
st="$HANDOFF_STATE"
gw() { python3 -c 'import json,sys
try: print(json.load(open(sys.argv[1])).get("gateway_url",""))
except Exception: print("")' /etc/aisathome-node/node.json; }
where() { echo "token.env:$([[ -s /etc/aisathome-node/token.env ]] && echo here || echo none) gw:$(gw)"; }
args=(); for a in "$@"; do [[ $a == --* ]] || args+=("$a"); done
set -- "${args[@]}"
case "$1" in
  show)
    unit="${*: -1}"
    case "$*" in
      *MainPID*) echo 0 ;;
      *ActiveState*) cat "$st/$unit.state" 2>/dev/null || echo inactive ;;
      *LoadState*) [[ -e /etc/systemd/system/$unit.service ]] && echo loaded || echo not-found ;;
    esac ;;
  stop|start|restart)
    verb="$1"; shift
    for u in "$@"; do
      u="${u%.service}"
      case "$u" in aisathome-node|aisathome-llama) ;; *) continue ;; esac
      [[ $u = aisathome-node ]] && echo "$verb $u $(where)" >> "$st/systemctl.log"
      if [[ $u = aisathome-llama && $verb = restart ]]; then
        flock -n /etc/aisathome-node/node.lock true && echo "llama-restart free" >> "$st/lock.log" || echo "llama-restart held" >> "$st/lock.log"
      fi
      if [[ $verb = stop ]]; then echo inactive > "$st/$u.state"
      elif [[ $u = aisathome-node && ! -s /etc/aisathome-node/token.env ]]; then echo inactive > "$st/$u.state"
      else echo active > "$st/$u.state"; fi
    done ;;
  is-enabled|is-active) exit 1 ;;
  status) echo "status $2: $(cat "$st/$2.state" 2>/dev/null)" ;;
  *) ;;
esac
exit 0
SH
cat > "$tmp/stubs/nvidia-smi" <<'SH'
#!/bin/sh
case "$*" in
  *memory.total*) echo "0, GPU-fake-0, NVIDIA GeForce RTX 3090, 24576, 24126" ;;
  *query-compute-apps*) ;;
  *) echo "0, NVIDIA GeForce RTX 3090, 535.183.01, 8.6" ;;
esac
SH
printf '#!/bin/sh\nexit 0\n' > "$tmp/stubs/journalctl"
printf '#!/bin/sh\nexit 0\n' > "$tmp/stubs/useradd"
printf '#!/bin/sh\nexit 0\n' > "$tmp/stubs/userdel"
printf '#!/bin/sh\nexit 0\n' > "$tmp/stubs/chown"
# The service users do not exist in the sandbox (only root is mapped): id says they are root, and setpriv
# runs the command as it is. So the tests below check what the installer does about links it finds, not the
# privilege drop itself (which stops a link swapped in during the install from doing harm).
cat > "$tmp/stubs/id" <<'SH'
#!/bin/sh
case "$*" in *aisathome*) case "$1" in -u|-g) echo 0 ;; esac; exit 0 ;; esac
exec /usr/bin/id "$@"
SH
cat > "$tmp/stubs/setpriv" <<'SH'
#!/bin/sh
while [ $# -gt 0 ] && [ "$1" != "--" ]; do shift; done
shift
exec "$@"
SH
# install -o/-g: the aisathome user does not exist in the sandbox; everything else is the real install.
cat > "$tmp/stubs/install" <<'SH'
#!/usr/bin/env bash
a=(); while [[ $# -gt 0 ]]; do case "$1" in -o|-g) shift 2 ;; *) a+=("$1"); shift ;; esac; done
exec /usr/bin/install "${a[@]}"
SH
chmod +x "$tmp/stubs/"*
export PATH="$tmp/stubs:$PATH" HANDOFF_STATE="$st"

# A llama.cpp build whose llama-server says it is build 1 (so no speculation), and the model.
mkdir -p "$tmp/llama/bin"
printf '#!/bin/sh\necho "version: 1 (0000000)" >&2\n' > "$tmp/llama/bin/llama-server"; chmod +x "$tmp/llama/bin/llama-server"
head -c 200000 /dev/urandom > "$tmp/model.gguf"
model_env=(MODEL_URL="file://$tmp/model.gguf" MODEL_SHA256="$(sha256sum "$tmp/model.gguf" | cut -d' ' -f1)" MODEL_SIZE=200000)

# The stand-in llama-server on 127.0.0.1:8000: /health, and the speed test (tokens/s from $st/tps).
cat > "$tmp/llama.py" <<'PY'
import json, os, sys
from http.server import BaseHTTPRequestHandler, HTTPServer
class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def reply(self, obj):
        b = json.dumps(obj).encode(); self.send_response(200); self.send_header("Content-Type", "application/json"); self.end_headers(); self.wfile.write(b)
    def do_GET(self): self.reply({"status": "ok"})
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        if self.path == "/tokenize": return self.reply({"tokens": list(range(len(body["content"].split())))})
        tps = float(open(os.path.join(sys.argv[1], "tps")).read())
        self.reply({"timings": {"prompt_n": len(body["prompt"]), "prompt_ms": 1000.0, "predicted_per_second": tps}})
HTTPServer(("127.0.0.1", 8000), H).serve_forever()
PY
echo 30 > "$st/tps"
python3 "$tmp/llama.py" "$st" & llama_pid=$!

# Stand-in gateways: link (approved or denied, from $st/<name>.link), register, and /v1/node/self. Each
# issues tokens of its own (nt-n-<tag>0001.<43 x tag letter>) and logs every request with its token.
cat > "$tmp/gateway.py" <<'PY'
import json, os, sys
from http.server import BaseHTTPRequestHandler, HTTPServer
name, port, st = sys.argv[1], int(sys.argv[2]), sys.argv[3]
tag = {"a": "a", "b": "b"}[name]
issued = {"nt-n-0000" + tag + "001." + tag.upper() * 43: ("n-0000" + tag + "001", False),
          "nt-a-0000" + tag + "0a1." + tag.upper() * 43: ("a-0000" + tag + "0a1", True)}
def mode(what):
    try: return open(os.path.join(st, name + "." + what)).read().strip()
    except OSError: return ""
class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def reply(self, code, obj):
        b = json.dumps(obj).encode(); self.send_response(code); self.send_header("Content-Type", "application/json"); self.end_headers(); self.wfile.write(b)
    def log(self):
        tok = self.headers.get("Authorization", "").removeprefix("Bearer ")
        open(os.path.join(st, name + ".log"), "a").write(f"{self.command} {self.path} {tok or '-'}\n")
        return tok
    def do_GET(self):
        tok = self.log()
        if self.path != "/v1/node/self": return self.reply(404, {})
        if tok not in issued or mode("revoked") == tok: return self.reply(401, {"error": {"message": "unknown node token"}})
        nid, anon = issued[tok]
        self.reply(200, {"node_id": nid, "anonymous": anon, "state": "held" if anon and mode("held") else "admitted"})
    def do_POST(self):
        self.log()
        self.rfile.read(int(self.headers.get("Content-Length") or 0))
        base = f"http://127.0.0.1:{port}/link"
        if self.path == "/v1/node/link":
            return self.reply(200, {"device_code": "dev", "user_code": "ABCD-EFGH", "verification_uri": base,
                                    "verification_uri_complete": base + "?code=ABCD-EFGH", "expires_in": 60, "interval": 1})
        if self.path == "/v1/node/link/poll":
            if mode("link") == "deny": return self.reply(200, {"status": "denied"})
            return self.reply(200, {"status": "approved", "token": "nt-n-0000" + tag + "001." + tag.upper() * 43, "node_id": "n-0000" + tag + "001"})
        if self.path == "/v1/node/register":
            return self.reply(200, {"token": "nt-a-0000" + tag + "0a1." + tag.upper() * 43, "node_id": "a-0000" + tag + "0a1"})
        self.reply(404, {})
HTTPServer(("127.0.0.1", port), H).serve_forever()
PY
python3 "$tmp/gateway.py" a 18081 "$st" & gwa_pid=$!
python3 "$tmp/gateway.py" b 18082 "$st" & gwb_pid=$!
trap 'kill $llama_pid $gwa_pid $gwb_pid 2>/dev/null' EXIT
gwa=ws://127.0.0.1:18081/v1/node/connect gwb=ws://127.0.0.1:18082/v1/node/connect
toka="nt-n-0000a001.$(printf 'A%.0s' {1..43})" tokb="nt-n-0000b001.$(printf 'B%.0s' {1..43})"
anona="nt-a-0000a0a1.$(printf 'A%.0s' {1..43})" anonb="nt-a-0000b0a1.$(printf 'B%.0s' {1..43})"
for _ in $(seq 50); do
  curl -fs -o /dev/null http://127.0.0.1:8000/health && curl -s -o /dev/null http://127.0.0.1:18081/ && curl -s -o /dev/null http://127.0.0.1:18082/ && break
  sleep 0.1
done

# ---- Helpers -------------------------------------------------------------------------------------
fails=0 out="" code=0
fail() { echo "FAIL: $1"; echo "$out" | sed 's/^/    /' | tail -n 40; fails=$((fails + 1)); }
has() { [[ "$out" == *"$2"* ]] || fail "$1: missing '$2'"; }
lacks() { [[ "$out" != *"$2"* ]] || fail "$1: unexpected '$2'"; }
want_code() { [[ $code -eq $2 ]] || fail "$1: exit $code, want $2"; }
conf=/etc/aisathome-node models_dir=/var/lib/aisathome-node/models
node_bin="$tmp/bin/aisathome-node"

# fresh: a machine with nothing installed.
fresh() {
  rm -rf "$conf" /opt/aisathome-node /var/lib/aisathome-node /etc/systemd/system/aisathome-* /usr/local/bin/aisathome-node
  rm -f "$st"/*.state "$st"/*.log "$st"/*.link "$st"/*.revoked "$st"/*.held
  echo 30 > "$st/tps"; : > "$st/systemctl.log"
}
# seed <gateway> [<token> [<record>|-]]: an earlier install: node.json for gateway (node id box-1),
# token.env with the token recorded for <record> (default: gateway; "-": no record), units in place and
# the node running.
seed() {
  fresh
  mkdir -p "$conf" /etc/systemd/system
  printf '{\n  "node_id": "box-1",\n  "gateway_url": "%s",\n  "upstream": "http://127.0.0.1:8000"\n}\n' "$1" > "$conf/node.json"
  if [[ -n "${2:-}" ]]; then
    if [[ "${3:-}" = - ]]; then printf 'AISATHOME_NODE_TOKEN=%s\n' "$2" > "$conf/token.env"
    else printf 'AISATHOME_NODE_TOKEN=%s\nAISATHOME_TOKEN_GATEWAY=%s\n' "$2" "${3:-$1}" > "$conf/token.env"; fi
    chmod 600 "$conf/token.env"
  fi
  : > /etc/systemd/system/aisathome-node.service
  echo active > "$st/aisathome-node.state"; echo active > "$st/aisathome-llama.state"
}
# inst [VAR=value...] -- <args>: runs the installer (stdin: none unless the caller redirects it).
inst() {
  local env=()
  while [[ $# -gt 0 && "$1" != "--" ]]; do env+=("$1"); shift; done
  shift
  out="$(env UI_PORT=off "${env[@]}" "${model_env[@]}" bash "$here/install.sh" --llama-build "$tmp/llama" --node-bin "${NODE_BIN:-$node_bin}" "$@" 2>&1)"; code=$?
}
# tty_inst [<prompt> <input>]... -- <args>: the same at a terminal (a pty), typing each <input> once its
# <prompt> has appeared (after the previous one's). An input whose prompt never appears is never typed. The
# installer prints a question only once the terminal no longer echoes, so $out shows what was echoed.
cat > "$tmp/pty_run.py" <<'PY'
import os, pty, select, sys, time
args = sys.argv[1:]
cut = args.index("--")
pairs = [(args[i].encode(), args[i + 1].encode()) for i in range(0, cut, 2)]
cmd = args[cut + 1:]
pid, fd = pty.fork()
if pid == 0:
    os.execvp(cmd[0], cmd)
out, seen, status, deadline = b"", 0, None, time.time() + 600
while time.time() < deadline:
    if status is None:
        done, st = os.waitpid(pid, os.WNOHANG)
        if done:
            status = st
    ready, _, _ = select.select([fd], [], [], 0.2 if status is not None else 1)
    if not ready:
        if status is not None:
            break  # exited, and nothing more to read
        continue
    try:
        chunk = os.read(fd, 4096)
    except OSError:
        chunk = b""
    if not chunk:
        if status is None:
            status = os.waitpid(pid, 0)[1]
        break
    out += chunk
    while pairs and out.find(pairs[0][0], seen) >= 0:
        seen = out.find(pairs[0][0], seen) + len(pairs[0][0])
        os.write(fd, pairs.pop(0)[1])
sys.stdout.buffer.write(out)
if status is None:
    os.kill(pid, 9)
    print("\npty_run: timed out", flush=True)
    sys.exit(124)
sys.exit(os.waitstatus_to_exitcode(status))
PY
tty_inst() {
  local pairs=()
  while [[ $# -gt 0 && "$1" != "--" ]]; do pairs+=("$1"); shift; done
  shift
  out="$(python3 "$tmp/pty_run.py" "${pairs[@]}" -- env "${model_env[@]}" UI_PORT=off bash "$here/install.sh" --llama-build "$tmp/llama" --node-bin "$node_bin" "$@" 2>&1)"; code=$?
}
tok_env() { cat "$conf/token.env" 2>/dev/null; }
json() { python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get(sys.argv[2], ""))' "$1" "$2" 2>/dev/null; }
# token_is <name> <token> <gateway>: token.env holds exactly that token, recorded for that gateway, and
# node.json names that gateway.
token_is() {
  [[ "$(tok_env)" = "AISATHOME_NODE_TOKEN=$2"$'\n'"AISATHOME_TOKEN_GATEWAY=$3" ]] || fail "$1: token.env is $(tok_env | sed 's/=nt-\(.\{6\}\).*/=nt-\1…/' | tr '\n' ' ')"
  [[ "$(json "$conf/node.json" gateway_url)" = "$3" ]] || fail "$1: node.json names $(json "$conf/node.json" gateway_url), want $3"
  [[ "$(stat -c %a "$conf/token.env" 2>/dev/null)" = 600 ]] || fail "$1: token.env mode $(stat -c %a "$conf/token.env" 2>/dev/null)"
}
# never_sent <name> <gateway name> <token>: that gateway never saw the token.
never_sent() { ! grep -qF -- "$3" "$st/$2.log" 2>/dev/null || fail "$1: gateway $2 got a token it did not issue"; }
# agent_accepts <name>: the agent starts with the installed files (still running after 2 s, no config
# error), as its systemd unit runs it.
agent_accepts() {
  local log="$tmp/agent.log" rc
  ( set -a; . "$conf/token.env"; set +a; AISATHOME_UI_PORT=off timeout 2 "$node_bin" -config "$conf/node.json" ) > "$log" 2>&1; rc=$?
  [[ $rc -eq 124 ]] || { out="$(cat "$log")"; fail "$1: the agent did not start with the installed files (exit $rc)"; }
}
# node_json_canonical <name>: lower-case keys, no token in node.json.
node_json_canonical() {
  python3 - "$conf/node.json" <<'PY' || fail "$1: node.json is not canonical: $(cat "$conf/node.json")"
import json, sys
def walk(v):
    if isinstance(v, dict):
        for k, x in v.items():
            assert k == k.lower(), k
            walk(x)
    elif isinstance(v, list):
        for x in v: walk(x)
c = json.load(open(sys.argv[1])); walk(c); assert "token" not in c
PY
}

# ---- Scenarios -----------------------------------------------------------------------------------

# A fresh install that links: the token from the link, recorded for its gateway; the node starts.
fresh
inst -- --link --gateway-url "$gwa" --node-id fresh-1
want_code "fresh --link" 0; has "fresh --link" "Linked: this is node n-0000a001"
token_is "fresh --link" "$toka" "$gwa"; node_json_canonical "fresh --link"
[[ "$(json "$conf/node.json" node_id)" = n-0000a001 ]] || fail "fresh --link: node id $(json "$conf/node.json" node_id)"
[[ "$(tail -n 1 "$st/systemctl.log")" = "restart aisathome-node token.env:here gw:$gwa" ]] || fail "fresh --link: node not started: $(cat "$st/systemctl.log")"
agent_accepts "fresh --link"

# A same-gateway upgrade keeps the token and gateway (--keep-token, as scripts update); the node id is the
# one the token carries.
seed "$gwa" "$toka"
inst -- --keep-token
want_code "upgrade --keep-token" 0; has "upgrade --keep-token" "keeping the installed one"
token_is "upgrade --keep-token" "$toka" "$gwa"; node_json_canonical "upgrade --keep-token"
[[ "$(json "$conf/node.json" node_id)" = n-0000a001 ]] || fail "upgrade --keep-token: node id $(json "$conf/node.json" node_id)"
[[ ! -e "$conf/token.env.unlinked" ]] || fail "upgrade --keep-token: a token was moved aside"
agent_accepts "upgrade --keep-token"
# The same at a terminal: kept without a question.
seed "$gwa" "$toka"
tty_inst "[Y/n]" $'n\r' --
want_code "upgrade at a terminal" 0; token_is "upgrade at a terminal" "$toka" "$gwa"; lacks "upgrade at a terminal" "[Y/n]"

# A fresh install at a terminal: "Link this node to your account? [Y/n]". y links in the browser, n runs the
# node without an account.
fresh
tty_inst "[Y/n]" $'y\r' -- --gateway-url "$gwa"
want_code "fresh, y at a terminal" 0; has "fresh, y at a terminal" "Link this node to your account? [Y/n]"
has "fresh, y at a terminal" "Linked: this is node n-0000a001"; has "fresh, y at a terminal" "model.gguf: 0.0 of 0.0 GB (100%)"; token_is "fresh, y at a terminal" "$toka" "$gwa"
fresh
tty_inst "[Y/n]" $'n\r' -- --gateway-url "$gwa"
want_code "fresh, n at a terminal" 0; has "fresh, n at a terminal" "You must be 18 or older"
has "fresh, n at a terminal" "This node runs without an account"; token_is "fresh, n at a terminal" "$anona" "$gwa"
# --token-stdin at a terminal: pasted, not echoed.
fresh
tty_inst "Paste the node token (input hidden):" "$tokb"$'\r' -- --gateway-url "$gwb" --token-stdin
want_code "--token-stdin at a terminal" 0; has "--token-stdin at a terminal" "Paste the node token (input hidden)"
token_is "--token-stdin at a terminal" "$tokb" "$gwb"; [[ "$out" != *"$tokb"* ]] || fail "--token-stdin at a terminal: the token was echoed"
# A token pasted at the question (the older installer asked for one there): not echoed, not kept, and the
# question comes again.
fresh
tty_inst "[Y/n]" "$tokb"$'\r' "[Y/n]" $'n\r' -- --gateway-url "$gwa"
want_code "token at the question" 0; has "token at the question" "It was not stored, and it was not shown"
[[ "$out" != *"$tokb"* && "$out" != *"${tokb#nt-?-}"* ]] || fail "token at the question: the token was echoed"
token_is "token at the question" "$anona" "$gwa"; never_sent "token at the question" a "$tokb"

# An installed node.json with two keys equal in any case: refused, as the agent refuses it.
seed "$gwa" "$toka"
printf '{"node_id": "box-1", "gateway_url": "%s", "Gateway_URL": "%s"}\n' "$gwa" "$gwb" > "$conf/node.json"
inst -- --keep-token
want_code "case-variant keys" 2; has "case-variant keys" "appears more than once"
[[ ! -s "$st/systemctl.log" && ! -e /opt/aisathome-node ]] || fail "case-variant keys: something was installed"

# A script that pipes a new token for the same gateway: it replaces the installed one.
seed "$gwa" "$toka"
inst -- <<<"$anona"
want_code "piped token" 0; token_is "piped token" "$anona" "$gwa"

# A gateway change with --link: the old token is moved aside and the node stopped before the new
# node.json; the new gateway never sees the old token; the link's token is recorded for the new gateway.
seed "$gwa" "$toka"
inst -- --link --gateway-url "$gwb"
want_code "gateway change --link" 0
token_is "gateway change --link" "$tokb" "$gwb"
[[ "$(tok_env_unlinked=$(cat "$conf/token.env.unlinked" 2>/dev/null); echo "$tok_env_unlinked")" == *"$toka"* ]] || fail "gateway change --link: the old token is not in token.env.unlinked"
[[ "$(json "$conf/node.json.unlinked" gateway_url)" = "$gwa" ]] || fail "gateway change --link: no copy of the old node.json"
[[ "$(head -n 1 "$st/systemctl.log")" = "stop aisathome-node token.env:none gw:$gwa" ]] || fail "gateway change --link: not stopped before the new node.json: $(cat "$st/systemctl.log")"
never_sent "gateway change --link" b "$toka"
agent_accepts "gateway change --link"
# --keep-token over a gateway change: refused before anything changes.
seed "$gwa" "$toka"
inst -- --keep-token --gateway-url "$gwb"
want_code "gateway change --keep-token" 2; has "gateway change --keep-token" "never sent there"
token_is "gateway change --keep-token" "$toka" "$gwa"; [[ ! -s "$st/systemctl.log" ]] || fail "gateway change --keep-token: the node was touched"

# A token without its issuer record (v0.5 and older): never kept, never sent.
seed "$gwa" "$toka" -
inst -- --keep-token
want_code "no record --keep-token" 2; has "no record --keep-token" "unrecorded gateway"
[[ "$(tok_env)" = "AISATHOME_NODE_TOKEN=$toka" && ! -e "$conf/token.env.unlinked" ]] || fail "no record --keep-token: token.env changed"
seed "$gwa" "$toka" -
inst -- --link
want_code "no record --link" 0; token_is "no record --link" "$toka" "$gwa"
[[ -s "$conf/token.env.unlinked" ]] || fail "no record --link: the unrecorded token was not moved aside"
grep -q "^GET /v1/node/self" "$st/a.log" 2>/dev/null && fail "no record --link: the gateway was asked about the unrecorded token"

# An interrupted link (denied) on a gateway change: the old token stays aside, the node stays off.
seed "$gwa" "$toka"; echo deny > "$st/b.link"
inst -- --link --gateway-url "$gwb"
want_code "denied link" 1; has "denied link" "Not linked yet, so the node is off"
[[ ! -e "$conf/token.env" && -s "$conf/token.env.unlinked" ]] || fail "denied link: the old token came back beside the new gateway"
[[ "$(json "$conf/node.json" gateway_url)" = "$gwb" ]] || fail "denied link: node.json $(json "$conf/node.json" gateway_url)"
never_sent "denied link" b "$toka"
# Stopped before its link step (the speed test fails): the old token and node.json come back, and the
# node that was on starts again.
seed "$gwa" "$toka"; echo 1 > "$st/tps"
inst MIN_DECODE_TPS=20 -- --link --gateway-url "$gwb"
want_code "stopped before linking" 1; has "stopped before linking" "the installed token and its node.json are back"
token_is "stopped before linking" "$toka" "$gwa"
[[ ! -e "$conf/token.env.unlinked" ]] || fail "stopped before linking: token.env.unlinked left"
[[ "$(tail -n 1 "$st/systemctl.log")" = "start aisathome-node token.env:here gw:$gwa" ]] || fail "stopped before linking: the node did not start again: $(cat "$st/systemctl.log")"

# The moved-aside token given again (an upgrade script piping it over a gateway change): refused.
seed "$gwa" "$toka"
inst -- --gateway-url "$gwb" <<<"$toka "
want_code "moved-aside token piped" 2; has "moved-aside token piped" "never sent there"
token_is "moved-aside token piped" "$toka" "$gwa"; never_sent "moved-aside token piped" b "$toka"

# --anonymous over a token recorded for this gateway: kept (the gateway still knows it), node updated.
seed "$gwa" "$toka"
inst -- --anonymous
want_code "--anonymous over a token" 0; has "--anonymous over a token" "keeps its token"
token_is "--anonymous over a token" "$toka" "$gwa"
grep -q "^POST /v1/node/register" "$st/a.log" 2>/dev/null && fail "--anonymous over a token: registered anyway"
# ... one the gateway no longer knows: moved aside, and the node registers.
seed "$gwa" "$toka"; echo "$toka" > "$st/a.revoked"
inst -- --anonymous
want_code "--anonymous over a revoked token" 0; has "--anonymous over a revoked token" "no longer knows"
token_is "--anonymous over a revoked token" "$anona" "$gwa"
# ... one recorded for another gateway: never asked about or sent there; the node registers.
seed "$gwb" "$toka" "$gwa"
inst -- --anonymous
want_code "--anonymous, another gateway's token" 0; token_is "--anonymous, another gateway's token" "$anonb" "$gwb"
never_sent "--anonymous, another gateway's token" b "$toka"

# A dry run changes nothing and says what the install would do.
seed "$gwa" "$toka"; cp -a "$conf" "$tmp/conf.before"
inst -- --dry-run --anonymous
want_code "dry run" 0; has "dry run" '"gateway_url": "'"$gwa"'"'; has "dry run" "account: none"
diff -r "$conf" "$tmp/conf.before" >/dev/null || fail "dry run: /etc/aisathome-node changed"
[[ ! -s "$st/systemctl.log" && ! -e /opt/aisathome-node ]] || fail "dry run: something was installed"
rm -rf "$tmp/conf.before"

# The node binary override: the given one is what is installed and what links.
fresh
inst -- --link --gateway-url "$gwa"
want_code "--node-bin" 0
cmp -s /opt/aisathome-node/bin/aisathome-node "$node_bin" || fail "--node-bin: another binary was installed"

# The writers' lock (node.lock, which aisathome-node link takes too): held from the installer's decision
# (config preflight) through its writes and the speed test, and free for the link step.
cat > "$tmp/locked-node" <<SH
#!/bin/sh
lock=/etc/aisathome-node/node.lock
if [ ! -e \$lock ]; then s=none; elif flock -n \$lock true; then s=free; else s=held; fi
echo "\$1 \$s" >> "$st/lock.log"
exec "$node_bin" "\$@"
SH
chmod +x "$tmp/locked-node"
seed "$gwa" "$toka"; rm -f "$st/lock.log"
NODE_BIN="$tmp/locked-node" inst -- --link --gateway-url "$gwb"
want_code "the writers' lock" 0
[[ "$(grep -v '^-features' "$st/lock.log" | paste -sd, -)" = "config held,llama-restart held,link free" ]] || fail "the writers' lock: $(paste -sd, - < "$st/lock.log")"

# A retry after a link that did not finish (node.json, no token.env): it links now.
seed "$gwa"
inst -- --link
want_code "retry an unfinished link" 0; token_is "retry an unfinished link" "$toka" "$gwa"

# The permanent uninstaller and the local UI's stop and uninstall: the installer puts the uninstaller at
# /usr/local/sbin (the bundle's folder may be gone after a reboot) and a root control unit that does what
# the agent writes to its trigger (aisathome-node's ControlNode): stop, uninstall or uninstall-purge, and
# nothing else.
ctl=/var/lib/aisathome-node/node-control
fresh
inst UI_PORT=7381 -- --link --gateway-url "$gwa"
want_code "with the local UI" 0; has "with the local UI" "sudo aisathome-node-uninstall"
[[ -x /usr/local/sbin/aisathome-node-uninstall && -x /usr/local/libexec/aisathome-node-control && -s /etc/systemd/system/aisathome-node-control.path ]] ||
  fail "with the local UI: the uninstaller or the control unit is missing"
grep -qx "Environment=AISATHOME_CONTROL_TRIGGER=$ctl" /etc/systemd/system/aisathome-node.service.d/local-ui.conf || fail "with the local UI: no control trigger for the agent"
grep -qx "PathChanged=$ctl" /etc/systemd/system/aisathome-node-control.path || fail "with the local UI: the control unit watches another file"
control() { : > "$st/systemctl.log"; out="$(/usr/local/libexec/aisathome-node-control 2>&1)"; code=$?; }
echo stop > "$ctl"; control
want_code "control: stop" 0; grep -q "^stop aisathome-node " "$st/systemctl.log" || fail "control: stop did not stop the node: $(cat "$st/systemctl.log")"
for junk in "restart" "rm -rf /" "stopp" ""; do
  printf '%s\n' "$junk" > "$ctl"; control
  [[ ! -s "$st/systemctl.log" && -e /opt/aisathome-node ]] || fail "control: '$junk' did something"
done
rm -f "$ctl"; echo uninstall > "$tmp/elsewhere"; ln -s "$tmp/elsewhere" "$ctl"; control
[[ ! -s "$st/systemctl.log" && -e /opt/aisathome-node ]] || fail "control: a link to a file was followed"
rm -f "$ctl"; echo uninstall > "$ctl"; control
want_code "control: uninstall" 0; has "control: uninstall" "kept /var/lib/aisathome-node"
for f in /opt/aisathome-node "$conf" /etc/systemd/system/aisathome-node.service /etc/systemd/system/aisathome-node-control.path \
         /usr/local/libexec/aisathome-node-control /usr/local/sbin/aisathome-node-uninstall /usr/local/bin/aisathome-node; do
  [[ ! -e "$f" ]] || fail "control: uninstall left $f"
done
[[ -s /var/lib/aisathome-node/models/model.gguf ]] || fail "control: uninstall removed the model"
fresh
inst UI_PORT=7381 -- --link --gateway-url "$gwa"
echo uninstall-purge > "$ctl"; control
want_code "control: uninstall-purge" 0; [[ ! -e /var/lib/aisathome-node && ! -e /usr/local/sbin/aisathome-node-uninstall ]] || fail "control: uninstall-purge left files"

# The models folder belongs to the engine's user, which could put links in it: the installer, as root, never
# follows one. Links planted in place of the model's and the draft model's download (.part) and of the files
# themselves are deleted, and the files they point at keep their content and mode; the model ends up a
# plain file, mode 0640. --model is moved in by a rename (same filesystem) or copied (another one), which
# replaces a link and never writes through it.
victims() { for v in 1 2 3 4; do printf 'system file %s\n' "$v" > "$tmp/victim$v"; chmod 0600 "$tmp/victim$v"; done; }
victims_intact() {
  local v
  for v in 1 2 3 4; do
    [[ "$(cat "$tmp/victim$v")" = "system file $v" && "$(stat -c %a "$tmp/victim$v")" = 600 ]] || fail "$1: a link was followed (victim$v: $(stat -c %a "$tmp/victim$v"), $(head -c 40 "$tmp/victim$v"))"
  done
}
plain_0640() { [[ -f "$2" && ! -L "$2" && "$(stat -c %a "$2")" = 640 ]] || fail "$1: $2 is not a plain file with mode 0640: $(ls -l "$2" 2>&1)"; }
mkdir -p "$tmp/llama-new/bin"
printf '#!/bin/sh\necho "version: 0.9.0-dev (build 12342, commit 0fb8f1577)" >&2\n' > "$tmp/llama-new/bin/llama-server"; chmod +x "$tmp/llama-new/bin/llama-server"
head -c 50000 /dev/urandom > "$tmp/draft.gguf"
draft_env=(DRAFT_URL="file://$tmp/draft.gguf" DRAFT_SHA256="$(sha256sum "$tmp/draft.gguf" | cut -d' ' -f1)" DRAFT_SIZE=50000)
fresh; victims; mkdir -p "$models_dir"
ln -s "$tmp/victim1" "$models_dir/model.gguf.part"; ln -s "$tmp/victim2" "$models_dir/model.gguf"
ln -s "$tmp/victim3" "$models_dir/draft.gguf.part"; ln -s "$tmp/victim4" "$models_dir/draft.gguf"
out="$(env UI_PORT=off "${model_env[@]}" "${draft_env[@]}" bash "$here/install.sh" --llama-build "$tmp/llama-new" --node-bin "$node_bin" --link --gateway-url "$gwa" 2>&1)"; code=$?
want_code "planted links, download" 0; has "planted links, download" "speculation: mtp (the default"
victims_intact "planted links, download"
plain_0640 "planted links, download" "$models_dir/model.gguf"; plain_0640 "planted links, download" "$models_dir/draft.gguf"
cmp -s "$models_dir/model.gguf" "$tmp/model.gguf" || fail "planted links, download: not the model"
# --model on the same filesystem (a rename) and from another one (a copy), over a planted link.
for where in /var/lib/handoff-src "$tmp/src"; do
  fresh; victims; mkdir -p "$models_dir" "$where"
  ln -s "$tmp/victim2" "$models_dir/model.gguf"; ln -s "$tmp/victim1" "$models_dir/model.gguf.part"
  cp "$tmp/model.gguf" "$where/own.gguf"
  inst -- --link --gateway-url "$gwa" --model "$where/own.gguf"
  want_code "planted link, --model from $where" 0; victims_intact "planted link, --model from $where"
  plain_0640 "planted link, --model from $where" "$models_dir/model.gguf"
  [[ ! -e "$where/own.gguf" ]] || fail "planted link, --model from $where: the file was not moved"
done
# The state folder is the agent's (0751: the engine passes through to models/ only), models/ the engine's,
# and llama-server runs as its own user.
[[ "$(stat -c %a /var/lib/aisathome-node)" = 751 && "$(stat -c %a "$models_dir")" = 750 ]] || fail "folder modes: $(stat -c '%n %a' /var/lib/aisathome-node "$models_dir")"
grep -qx "User=aisathome-llama" /etc/systemd/system/aisathome-llama.service && grep -qx "User=aisathome" /etc/systemd/system/aisathome-node.service ||
  fail "the engine does not run as its own user"

# A node binary older than the installer (no config preflight): refused before anything changes, and never
# run with arguments it does not know (an old binary would start the agent on "config ..."). It stands in
# as Go's flag package does: an unknown flag is exit 2; anything else would run the agent.
cat > "$tmp/old-node" <<'SH'
#!/bin/sh
case "$1" in -version) echo "aisathome-node v0.6.0"; exit 0 ;; -features) echo "flag provided but not defined: -features" >&2; exit 2 ;; esac
echo "$*" >> "$HANDOFF_STATE/old-node.ran"; exit 0
SH
chmod +x "$tmp/old-node"
seed "$gwa" "$toka"
NODE_BIN="$tmp/old-node" inst -- --keep-token
want_code "an older --node-bin" 2; has "an older --node-bin" "has no 'config preflight'"
[[ ! -e "$st/old-node.ran" ]] || fail "an older --node-bin: it was run with: $(cat "$st/old-node.ran")"
token_is "an older --node-bin" "$toka" "$gwa"; [[ ! -s "$st/systemctl.log" && ! -e /opt/aisathome-node ]] || fail "an older --node-bin: something was installed"
NODE_BIN="$tmp/old-node" inst -- --dry-run
want_code "an older --node-bin, dry run" 2

if [[ $fails -gt 0 ]]; then echo "handoff: $fails failure(s)"; exit 1; fi
echo "handoff: all tests passed"
