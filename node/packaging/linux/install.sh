#!/usr/bin/env bash
# Install an AIs@home node on Linux (systemd): llama-server plus aisathome-node, both
# running as the unprivileged system user "aisathome", sandboxed by systemd.
#
# From the prebuilt bundle (this script next to bin/llama-server, lib/ and aisathome-node):
#   sudo ./install.sh [--node-id <id>] [--enable-at-boot] [--dry-run]
# With your own llama.cpp build:
#   sudo ./install.sh --llama-build <llama.cpp/build> \
#        (--node-bin <aisathome-node> | --node-release <latest|vX.Y.Z>) [options]
# Options: --model <file.gguf>, --node-config <node.json>, --gateway-url <wss://...>,
#   --enable-at-boot, --link, --anonymous, --keep-token, --dry-run, --download-only <dir> [--tier standard|big],
#   --speculation off|mtp|dflash2 (default: off, or what an earlier install chose).
#
# Without --model, the tier's pinned model is downloaded from Hugging Face, resumed if
# interrupted and checked against its SHA-256 (a --model file must be that same file).
# --download-only <dir> only does that (no root needed), for the tier of this machine's card or
# the one --tier names (standard: 16 GB cards, big: 24 GB and more), so it also works on a
# machine without the card. Without --node-config, node.json is written from --node-id
# (default: the host name), --gateway-url and the tier. The bundle checks the NVIDIA driver and
# GPU first. --node-release downloads aisathome-node for this machine's architecture from a
# GitHub release of aisathome/aisathome and checks it against that release's SHA256SUMS.
# Tier, from the card's size: standard (16 GB cards: UD-Q3_K_XL, 80k context, room for a light
# desktop) or big (24 GB+: UD-IQ4_XS, 192k); see README.md. A short speed test runs after the
# install.
# --dry-run runs the checks, prints the plan and the llama-server command, and exits
# without installing (no root needed). Your account: at a terminal, press Enter at the token
# question and the install ends by linking the node to your account in the browser (a link
# and a code; approve on any device). --link does that without asking. A node token instead
# (advanced, and for scripts) is read from stdin, never from argv: paste it when asked, or
# cat token.txt | sudo ./install.sh ...
# --anonymous starts the node without an account instead (science only, no credit until it
# is linked with `aisathome-node link`): it registers with the gateway after the speed test.
# Updating: run the new bundle's install.sh. At a terminal, Enter on an empty line keeps the
# installed token (--keep-token does so without asking), and a generated node.json keeps the
# installed node id and gateway URL unless --node-id or --gateway-url is given.
#
# What it touches:
#   /opt/aisathome-node/            binaries and llama.cpp shared libraries (root-owned)
#   /var/lib/aisathome-node/models  the model (moved or downloaded; owned by aisathome)
#   /etc/aisathome-node/            node.json (root:aisathome 0640), token.env (root 0600)
#   /etc/systemd/system/aisathome-{llama,node}.service
#   local UI: see the "Local node UI" section below (UI_PORT=<port>|off, default 7381)
# Nothing listens on a public port: llama-server and the node's local UI bind 127.0.0.1, and
# the node only dials out.
#
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail

usage() { sed -n '2,35p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }
die() { echo "$1" >&2; exit "${2:-1}"; }
here="$(cd "$(dirname "$0")" && pwd)"
llama_build="" model="" node_bin="" node_release="" node_config="" at_boot=0 dry_run=0
node_id="" gateway_url="" download_only="" keep_token=0 link=0 anonymous=0 want_tier="" want_spec=""
conf=/etc/aisathome-node
preset="${PRESET:-}" alias="${MODEL_ALIAS:-}"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --llama-build) llama_build="$2"; shift 2 ;;
    --model) model="$2"; shift 2 ;;
    --node-bin) node_bin="$2"; shift 2 ;;
    --node-release) node_release="$2"; shift 2 ;;
    --node-config) node_config="$2"; shift 2 ;;
    --node-id) node_id="$2"; shift 2 ;;
    --gateway-url) gateway_url="$2"; shift 2 ;;
    --download-only) download_only="$2"; shift 2 ;;
    --tier) want_tier="$2"; shift 2 ;;
    --speculation) want_spec="$2"; shift 2 ;;
    --preset) preset="$2"; shift 2 ;;
    --enable-at-boot) at_boot=1; shift ;;
    --keep-token) keep_token=1; shift ;;
    --link) link=1; shift ;;
    --anonymous) anonymous=1; shift ;;
    --dry-run) dry_run=1; shift ;;
    *) usage ;;
  esac
done

[[ $link = 0 || $keep_token = 0 ]] || die "use --link or --keep-token, not both" 2
[[ $anonymous = 0 || ($link = 0 && $keep_token = 0) ]] || die "use --anonymous alone, without --link or --keep-token" 2

# What a node without an account means, shown before it installs (node.sh shows the same lines).
anon_notice() {
  cat >&2 <<'NOTICE'
Without an account this node does science work only, never people's chats, and earns no credit
until you link it to an account (aisathome-node link).
You must be 18 or older; running it means you accept the terms: https://aisathome.org/about/terms/
NOTICE
}

# Two tiers, chosen by the size of the card only (owner, 2026-10-22/23 and 2026-11-09): one model
# file per tier, 1 request at a time, and a context set by the card, never by the machine's
# other load.
#   standard  cards with 15.5 to 23 GB (16 GB class): UD-Q3_K_XL, 1 x 80k  (81,920),  KV q4_0
#   big       cards with 23 GB or more (24 GB+):      UD-IQ4_XS,  1 x 192k (196,608), KV q4_0
# The two files answer under different model names (tier_alias), so a client and the gateway
# can tell which one answered; both serve the roles "deep" and "agent".
# When the tier does not fit in the card's free memory, the installer stops with a message; it
# never picks a smaller context. Per-slot context means one long request cannot starve the
# others (a single shared pool can: "Context size has been exceeded" on 2026-10-08).
# Measured with llama.cpp v0.5.0 and these flags (research notes, tiers, 2026-10-22/23, and
# 16 GB, 2026-10-31 and 2026-11-09):
#   standard, UD-Q3_K_XL: RTX 5070 Ti (15,839 MiB visible to CUDA), llama-server's own peak
#     at a 95% full context is 13,659 MiB at 65,536 and 14,211 at 90,112, so about 14,030 at
#     81,920. With other programs holding memory on the card, 81,920 needs less than 90,112,
#     which ran with 1.5 GB held; 81,920 failed at start with 2.0 GB held. Decode 29.6
#     tokens/s after a 32k prompt. (UD-IQ4_XS at 81,920 peaks at about 15,100 MiB: it left
#     only about 640 MiB for a desktop.)
#   big, UD-IQ4_XS: RTX 4090, 1 x 192k: llama-server holds 18,060 MiB after load, 18,246 at
#     the peak of a 190k prompt. 256k on an RTX 3090 fit in 19,202 MiB; hard needles 77/80 at
#     ~195k (62/80 at ~247k, hence 192k), decode 16.7 tokens/s at 195k.
# tier_min_free (MiB): what nvidia-smi must report free on the card before llama-server starts
# (the peaks above plus a margin; more SMs take a little more memory). Standard: 14,300 leaves
# about 270 MiB at the peak on the RTX 5070 Ti and lets a desktop of about 1.5 GB stay on it.
# Explicit environment variables (SLOTS, CTX_PER_SLOT, KV_TYPE) override the tier, for
# testing only; the free-memory check is then skipped.
# --preset / PRESET: the old single setting (long-context); accepted and ignored.
case "$preset" in
  ""|long-context|auto) ;;
  *) die "unknown preset '$preset'; presets are gone: the tier follows the card (standard: 80k on 16 GB cards, big: 192k on 24 GB+)" 2 ;;
esac
case "$want_tier" in
  ""|standard|big) ;;
  *) die "--tier must be standard (16 GB cards) or big (24 GB and more)" 2 ;;
esac
[[ -z "$want_tier" || -n "$download_only" ]] ||
  die "--tier is for --download-only; an install takes the tier from the card" 2
case "$want_spec" in
  ""|off|mtp|dflash2) ;;
  *) die "--speculation must be off, mtp or dflash2" 2 ;;
esac
# The Windows node has the same table in Go (node/setup/tiers.go); its drift test runs these
# tier_* functions and fails until both agree. Change both together.
tier_ctx() { case "$1" in standard) echo 81920 ;; big) echo 196608 ;; esac; }
tier_kv() { echo q4_0; }
tier_min_free() { case "$1" in standard) echo 14300 ;; big) echo 18500 ;; esac; }
# tier_alias: the model name the tier's nodes answer under (llama-server's --alias and the
# node's "models" entry). MODEL_ALIAS replaces it (testing).
tier_alias() { case "$1" in standard) echo qwen3.8-27b-q3 ;; big) echo qwen3.8-27b ;; esac; }
# tier_for_total <MiB>: the tier for a card with this much memory, or "" (too small).
tier_for_total() {
  if (( $1 >= 23000 )); then echo big; elif (( $1 >= 15500 )); then echo standard; fi
}
custom=0
[[ -z "${SLOTS:-}${CTX_PER_SLOT:-}${KV_TYPE:-}" ]] || custom=1
slots="${SLOTS:-1}" ctx_per_slot="${CTX_PER_SLOT:-}" kv_type="${KV_TYPE:-}"
# Minimum decode speed (tokens/s) after a ~32k-token prompt, measured at install. A short
# prompt misranks cards (an RTX 5070 Ti is the fastest 16 GB card at 200 tokens and slow at
# 64k with q4_0 KV). Measured at 32k: RTX 4080 35.6, RTX 3090 35.1, RTX 5070 Ti 28.9,
# RTX 5060 Ti 13.8. MIN_DECODE_TPS overrides it, for testing only (0: measure, never refuse).
min_tps="${MIN_DECODE_TPS:-20}" speed_ctx=32000
# Thinking budget in tokens (-1 = unlimited). Unlimited thinking ran for 8 minutes and filled
# the context; after the budget the model is told to answer.
reasoning_budget="${REASONING_BUDGET:-8192}"
# Host-RAM prompt cache in MiB (llama-server --cache-ram; default there 8192, 0 = off). It
# bounds how much conversation data stays warm in RAM; llama-server has no idle-time limit.
# cache_ram_for <total RAM MiB>: half the machine's RAM, at most 16384 (owner, 2026-11-12);
# 2048 when the RAM is not known (0). One cached agent conversation of the hybrid model is
# 1-1.5 GiB at 19-44k tokens and an entry larger than the cache is dropped, so 2048 held one
# conversation; 16384 raised cache hits after a switch between two conversations from 29% to
# 88%, peaking at 9.3 GiB (measured 2026-11-12). It fills up on a busy node, hence the cap at
# half the RAM: volunteers may run BOINC CPU work beside the node. The Windows node has the
# same rule in Go (node/setup/engine.go, CacheRAMFor); the drift test compares them.
cache_ram_for() {
  if (( $1 <= 0 )); then echo 2048; elif (( $1 / 2 < 16384 )); then echo $(( $1 / 2 )); else echo 16384; fi
}
mem_total_mib() { awk '$1 == "MemTotal:" { print int($2 / 1024); f = 1 } END { if (!f) print 0 }' /proc/meminfo 2>/dev/null || echo 0; }
cache_ram="${CACHE_RAM_MIB:-$(cache_ram_for "$(mem_total_mib)")}"
case "$kv_type" in ""|q8_0|q4_0|f16) ;; *) die "KV_TYPE must be q8_0, q4_0 or f16" 2 ;; esac
[[ "$slots" =~ ^[1-9][0-9]*$ ]] || die "SLOTS must be a positive integer" 2
[[ -z "$ctx_per_slot" || "$ctx_per_slot" =~ ^[1-9][0-9]*$ ]] || die "CTX_PER_SLOT must be a positive integer" 2
[[ "$min_tps" =~ ^[0-9]+([.][0-9]+)?$ ]] || die "MIN_DECODE_TPS must be a non-negative number" 2
[[ "$reasoning_budget" =~ ^(-1|[0-9]+)$ ]] || die "REASONING_BUDGET must be -1 or a non-negative integer" 2
[[ "$cache_ram" =~ ^[0-9]+$ ]] || die "CACHE_RAM_MIB must be a non-negative integer" 2
[[ -z "$node_bin" || -z "$node_release" ]] || die "use --node-bin or --node-release, not both" 2
[[ -z "$node_release" || "$node_release" = latest || "$node_release" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$ ]] ||
  die "--node-release must be 'latest' or a tag like v1.2.3" 2
# Releases come from GitHub; AISATHOME_RELEASE_URL points elsewhere (a mirror, or file:// in tests).
release_base="${AISATHOME_RELEASE_URL:-https://github.com/aisathome/aisathome/releases}"
[[ "$release_base" =~ ^(https|file):// ]] || die "AISATHOME_RELEASE_URL must start with https:// or file://" 2
[[ -z "$alias" || "$alias" =~ ^[A-Za-z0-9._-]+$ ]] || die "MODEL_ALIAS may only contain letters, digits, '.', '_' and '-'" 2

# The network's models, one per tier (owner, 2026-10-22 and 2026-11-09): Qwen3.8 27B from
# unsloth, pinned to a commit of the Hugging Face repository and checked by SHA-256. Sha256 and
# size are each file's LFS pointer at that commit (Checked 2026-10-23 for UD-IQ4_XS: the Hub
# API's paths-info and the resolve URL's x-linked-etag / x-linked-size headers; UD-Q3_K_XL:
# the Hub's file tree, 2026-10-18), the same files the tiers were measured with. UD-IQ4_XS
# replaced UD-Q4_K_M (16.5 GB, sha256 322e194f...) in v0.3.0. The model check cannot tell
# close quants apart reliably (IQ4_XS against the Q4_K_M reference was caught on 71-84% of
# checks), so a tier never mixes files: one file, one model name, one reference.
# tier_model <tier>: "<url> <sha256> <bytes>" of the tier's file.
tier_model() {
  local base=https://huggingface.co/unsloth/Qwen3.8-27B-GGUF/resolve/4ca720788d1e01f1bff70c033e0d0028fd02e502
  case "$1" in
    standard) echo "$base/Qwen3.8-27B-UD-Q3_K_XL.gguf 8c2a45ff85e7674ca185ec8eb6cdeab0e617ed9d8018caed0b64380eb2a67a5e 13146393504" ;;
    big) echo "$base/Qwen3.8-27B-UD-IQ4_XS.gguf 40fac4050e940397dbf13087afd50f4734a11805bf9d65ef8ddd7483470e6199 14252845984" ;;
  esac
}
# Speculative decoding (--speculation): llama-server drafts tokens with a small model and checks
# them in one step of the big one, so the node writes faster; the output distribution is the
# model's own. Off by default; on only for the big tier, with free memory for the draft model,
# and on a llama.cpp build new enough for the mode, else the node runs without it (pick_spec says
# why). The Windows node has the same table in Go (node/setup/speculation.go); drift_test.go
# runs these spec_* functions and fails until both agree. Change both together.
#   mtp      Qwen3.8's multi-token-prediction head (Unsloth's sidecar, the model's revision), 2
#            drafted tokens a step; decode x1.8 on long agent steps, +2.5 GB (RTX 3090, 2026-11-12).
#   dflash2  z-lab's DFlash2 drafter (Apache-2.0), 7 a step; x2.27, +2.9 GB, but only with
#            llama.cpp PR #31533 (0fb8f15, first released as b12342; without it x1.6).
spec_type() { case "$1" in mtp) echo draft-mtp ;; dflash2) echo draft-dflash ;; esac; }
spec_draft_max() { case "$1" in mtp) echo 2 ;; dflash2) echo 7 ;; esac; }
# spec_extra (MiB): what the draft model adds to tier_min_free.
spec_extra() { case "$1" in mtp) echo 2500 ;; dflash2) echo 2900 ;; esac; }
# spec_min_build: the oldest llama.cpp release (b<N>) the mode may run on.
spec_min_build() { case "$1" in mtp) echo 12323 ;; dflash2) echo 12342 ;; esac; }
# spec_model <mode>: "<url> <sha256> <bytes>" of the draft model (x-linked-etag and
# x-linked-size of the resolve URLs, Checked 2026-11-14).
spec_model() {
  case "$1" in
    mtp) echo "https://huggingface.co/unsloth/Qwen3.8-27B-GGUF/resolve/4ca720788d1e01f1bff70c033e0d0028fd02e502/MTP/mtp-Qwen3.8-27B-Q4_0.gguf 50d9ce5a6da381bbcfb31061cf73df94a90e6faf8efeddee379a9cb8f1501c6e 1369590656" ;;
    dflash2) echo "https://huggingface.co/z-lab/Qwen3.8-27B-DFlash2-GGUF/resolve/2d9571f8ce46e151f61c6499c99dee6079e1d610/Qwen3.8-27B-DFlash2-Q4_K_M.gguf 1a25c56858e1ebe93f2718ac1d49d1151f9323325c1bbfd6209370f4db131ebd 1143006816" ;;
  esac
}
# MODEL_URL, MODEL_SHA256 and MODEL_SIZE (bytes) replace the tier's file, all three together
# (testing).
if [[ -n "${MODEL_URL:-}${MODEL_SHA256:-}${MODEL_SIZE:-}" ]] &&
   [[ -z "${MODEL_URL:-}" || -z "${MODEL_SHA256:-}" || -z "${MODEL_SIZE:-}" ]]; then
  die "set MODEL_URL, MODEL_SHA256 and MODEL_SIZE together" 2
fi
[[ -z "${MODEL_URL:-}" || "$MODEL_URL" =~ ^(https|file)://[^[:space:]]+$ ]] || die "MODEL_URL must start with https:// or file://" 2
[[ -z "${MODEL_SHA256:-}" || "$MODEL_SHA256" =~ ^[0-9a-f]{64}$ ]] || die "MODEL_SHA256 must be 64 lowercase hex digits" 2
[[ -z "${MODEL_SIZE:-}" || "$MODEL_SIZE" =~ ^[1-9][0-9]{0,14}$ ]] || die "MODEL_SIZE must be a positive number of bytes" 2
model_url="" model_sha="" model_size=0 model_name=""
# set_model <tier>: the model file and model name for the tier (after the tier is known).
set_model() {
  read -r model_url model_sha model_size <<<"$(tier_model "$1")"
  [[ -z "${MODEL_URL:-}" ]] || model_url="$MODEL_URL" model_sha="$MODEL_SHA256" model_size="$MODEL_SIZE"
  model_name="${model_url##*/}"; model_name="${model_name%%\?*}"
  [[ "$model_name" =~ ^[A-Za-z0-9._-]+\.gguf$ ]] || model_name=model.gguf
  alias="${alias:-$(tier_alias "$1")}"
}
# Disk space kept free beyond the model: the binaries (about 1.5 GB) and some slack.
reserve=$((2 * 1024 * 1024 * 1024))

# installed_value <node.json> <key>: a string field of an earlier install's node.json, or ""
# (no file, not readable, or no such field). The key is matched as the agent matches it (Go's
# encoding/json: any case, the last one wins).
installed_value() {
  [[ -r "$1" ]] || return 0
  python3 -c 'import json,sys
class Pairs(list): pass
v=""
try:
  c=json.load(open(sys.argv[1]), object_pairs_hook=Pairs)
  for k, x in (c if isinstance(c, Pairs) else []):
    if k.lower() == sys.argv[2].lower(): v=x
except Exception: v=""
print(v if isinstance(v, str) else "")' "$1" "$2"
}

# canon_node_config <in> <out>: node.json as the node agent will read it, written once: every object's keys
# in lower case (the agent's Go decoder matches keys in any case), "token" dropped (it lives in token.env).
# Two keys that are equal in any case are refused: a reader that keeps the first and one that keeps the
# last would then see different gateways. The checks below read this file, and it is installed byte for
# byte, so what was checked is what the node gets.
canon_node_config() {
  local err
  err="$(python3 - "$1" "$2" <<'PY'
import json, sys
src, dst = sys.argv[1], sys.argv[2]
class Dup(Exception): pass
def pairs(ps):
    out = {}
    for k, v in ps:
        lk = k.lower()
        if lk in out:
            raise Dup(f"key {k!r} appears more than once (in any case); keep one")
        out[lk] = v
    return out
try:
    c = json.load(open(src), object_pairs_hook=pairs)
    if not isinstance(c, dict):
        raise Dup("not a JSON object")
except (Dup, ValueError) as e:
    print(e)
    sys.exit(1)
c.pop("token", None)
with open(dst, "w") as f:
    json.dump(c, f, indent=2)
    f.write("\n")
PY
)" || die "node config $1: $err" 2
}

# gateway_url_ok <url>: a gateway address a node token may go to, as the agent rules (node.CheckGatewayURL):
# wss://, or ws:// only to this machine (localhost, 127.x.x.x), where nothing crosses a network.
gateway_url_ok() {
  local h
  [[ "$1" =~ ^wss?://[A-Za-z0-9.-]+(:[0-9]+)?(/[A-Za-z0-9._~/-]*)?$ ]] || return 1
  [[ $1 != wss://* ]] || return 0
  h="${1#ws://}"; h="${h%%/*}"; h="${h%%:*}"
  [[ $h == localhost || $h =~ ^127\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}$ ]]
}

# trim <string>: without leading and trailing whitespace (spaces, tabs, CR), as systemd reads a value.
trim() {
  local v="$1"
  v="${v#"${v%%[![:space:]]*}"}"
  printf '%s' "${v%"${v##*[![:space:]]}"}"
}

# pick_gateway_url <installed>: --gateway-url, else the installed node.json's, else the
# network's gateway.
pick_gateway_url() {
  local from="--gateway-url"
  if [[ -z "$gateway_url" && -n "$1" ]]; then gateway_url="$1" from="the installed gateway_url"; fi
  gateway_url="${gateway_url:-wss://api.aisathome.org/v1/node/connect}"
  gateway_url_ok "$gateway_url" ||
    die "$from '$gateway_url' must look like wss://host/v1/node/connect (ws:// only to this machine; pass --gateway-url)" 2
}

# node.json is written by the installer unless --node-config is given. A rerun keeps the
# installed node id and gateway URL (not readable without root: a dry run may not see them).
# The installed node.json must be canonical too: one with keys equal in any case is refused, as the agent
# and aisathome-node link refuse it.
[[ ! -r "$conf/node.json" ]] || canon_node_config "$conf/node.json" /dev/null
old_id="" old_gw=""
if [[ -n "$node_config" ]]; then
  [[ -z "$node_id$gateway_url" ]] || die "--node-id and --gateway-url are for a generated node.json; edit your --node-config instead" 2
else
  [[ -z "$node_id" || "$node_id" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$ ]] ||
    die "--node-id '$node_id': use letters, digits, '.', '_' and '-' (up to 64)" 2
  old_id="$(installed_value "$conf/node.json" node_id)"
  old_gw="$(installed_value "$conf/node.json" gateway_url)"
  pick_gateway_url "$old_gw"
fi

# The prebuilt bundle: this script next to bin/llama-server, lib/ and BUILD-INFO. An explicit
# --llama-build, --node-bin or --node-release wins over the bundle's copy.
bundle=0
if [[ -z "$llama_build" && -x "$here/bin/llama-server" && -f "$here/BUILD-INFO" ]]; then
  bundle=1 llama_build="$here"
  [[ -n "$node_bin" || -n "$node_release" ]] || node_bin="$here/bin/aisathome-node"
fi

target=/var/lib/aisathome-node/models/model.gguf
# set_engine: llama-server's command for the tier (after pick_tier).
# The Windows node builds the same command in Go (node/setup/engine.go); the drift test runs
# set_engine and fails until both agree. Change both together.
llama_exec="" tier_label=""
set_engine() {
  ctx_per_slot="${ctx_per_slot:-$(tier_ctx "$tier")}" kv_type="${kv_type:-$(tier_kv "$tier")}"
  tier_label="$tier"; [[ $custom = 0 ]] || tier_label="test settings (card: $tier)"
  llama_exec="/opt/aisathome-node/bin/llama-server -m $target --alias $alias \\
  --host 127.0.0.1 --port 8000 -ngl all -fa on -np $slots --kv-unified-per-slot $ctx_per_slot \\
  --reasoning-budget $reasoning_budget \\
  --reasoning-budget-message \"(Thinking budget reached; answering now.)\" --no-reasoning-preserve \\
  -ctk $kv_type -ctv $kv_type --cache-prompt --cache-ram $cache_ram --metrics"
  if [[ "${spec:-off}" != off ]]; then
    llama_exec="$llama_exec \\
  --spec-type $(spec_type "$spec") -md $draft_target --spec-draft-n-max $(spec_draft_max "$spec")"
    tier_label="$tier_label, speculation $spec"
  fi
}

# GPU access for the sandboxed services. Their device allowlist (implied by ProtectClock=)
# would otherwise hide the GPU, and CUDA would silently fall back to the CPU. systemd matches
# char-<name> against the names in /proc/devices (a glob) and skips a name that is not listed
# there, with a debug message only (Checked: src/core/bpf-devices.c and cgroup.c in systemd
# v249 and main). The GPU's major 195 (/dev/nvidia0, /dev/nvidiactl) is "nvidia-frontend" up
# to driver 535 and "nvidia" (plus "nvidiactl") from 545 on (Checked: NVIDIA's open kernel
# modules, nv-frontend.c in 535.183.01, nv.c in 545.23.06 and 575.51.03; on machines, 530
# lists nvidia-frontend and 575 lists nvidia, 2026-10-18). With only the old name, driver 575
# ran llama-server on the CPU. nvidia-uvm and nvidia-caps kept their names.
gpu_devices='DeviceAllow=char-nvidia-frontend rw
DeviceAllow=char-nvidia rw'

# Shared sandbox: no access to /home, read-only system, no privileges. Prompts must not
# reach disk: no core dumps (a dump holds prompts from RAM) and no swap (MemorySwapMax needs
# cgroup v2; systemd ignores it on v1).
sandbox='LimitCORE=0
MemorySwapMax=0
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
ProtectClock=yes
RestrictSUIDSGID=yes
RestrictNamespaces=yes
LockPersonality=yes
CapabilityBoundingSet=
ReadWritePaths=/var/lib/aisathome-node'

llama_unit() {
  cat <<UNIT
[Unit]
Description=AIs@home node: local llama-server (127.0.0.1 only)
After=network.target aisathome-gpu-init.service
Requires=aisathome-gpu-init.service
# On resume, start only after the driver has woken the GPU (nvidia-resume.service runs in
# parallel with the sleep hook's restart otherwise). No effect when no resume is pending.
After=nvidia-resume.service

[Service]
User=aisathome
Group=aisathome
Environment=LD_LIBRARY_PATH=/opt/aisathome-node/lib
# On a crash, ggml forks a debugger to print a backtrace; inside this sandbox that can hang
# the process instead of ending it (CUDA MMU fault, 2026-10-09). Exit at once so systemd
# restarts it.
Environment=GGML_NO_BACKTRACE=1
${gpu_pin:+# Several GPUs: llama-server uses only the card the tier was chosen for.
$gpu_pin
}# GPU access: the GPU under its old and new driver names, then CUDA's memory manager.
$gpu_devices
DeviceAllow=char-nvidia-uvm rw
DeviceAllow=char-nvidia-caps r
ExecStart=$llama_exec
Restart=on-failure
RestartSec=5
# A hung CUDA process may ignore SIGTERM: kill it quickly instead of waiting 90 s.
TimeoutStopSec=20
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
$sandbox

[Install]
WantedBy=multi-user.target
UNIT
}

# engine_on_gpu <since>: llama-server, started at <since>, must run on the GPU. When CUDA sees
# no GPU (in the sandbox, most likely device rules that do not match the driver), llama-server
# carries on on the CPU: stop it then and leave it disabled, so nothing runs on the CPU.
engine_on_gpu() {
  # grep without -q reads the whole log: with -q, an early match ends grep, journalctl gets
  # SIGPIPE, and under pipefail the pipeline reports failure (the check passed silently).
  journalctl -u aisathome-llama --since "$1" -o cat | grep "no usable GPU found" >/dev/null || return 0
  systemctl disable --now aisathome-node aisathome-llama >/dev/null 2>&1 || true
  local names
  names="$(grep -i nvidia /proc/devices 2>/dev/null | paste -sd, - || true)"
  die "llama-server found no GPU and ran on the CPU, so the installer stopped it. If nvidia-smi works, the service's device rules probably do not match this driver: get the newest installer and run it again, or tell us (NVIDIA devices in /proc/devices: ${names:-none}). Details: journalctl -u aisathome-llama"
}

# engine_fits <since>: llama-server, started at <since>, did not become healthy. If it ran out
# of GPU memory (it fails at start, cleanly, when the tier does not fit), stop it, leave it
# disabled (systemd would restart it every 5 s) and say why. Otherwise return.
engine_fits() {
  journalctl -u aisathome-llama --since "$1" -o cat |
    grep -iE "out of memory|failed to allocate|unable to allocate|cudaMalloc failed" >/dev/null || return 0
  systemctl disable --now aisathome-node aisathome-llama >/dev/null 2>&1 || true
  die "llama-server ran out of GPU memory while loading the $tier tier, so the installer stopped the node. $(fit_advice) Details: journalctl -u aisathome-llama"
}

# ---- Tier: the card, its free memory and a speed test -------------------------------------
gpu_idx="" gpu_uuid="" gpu_name="" gpu_total=0 gpu_free=0 gpu_own=0 gpu_count=0 tier="" gpu_pin=""

# pick_tier: the card llama-server will use (the largest; on a tie, the one with more free
# memory), its total and free memory (MiB), and the tier for its size. On an update, the memory
# this node's own running llama-server holds counts as free on its card, before the cards are
# compared: it is released at the restart (two equal cards: the node's own card, not the other).
# With several GPUs, llama-server is pinned to that card (CUDA_VISIBLE_DEVICES=<uuid>).
pick_tier() {
  local out idx uuid name total free pid apps="" own
  command -v nvidia-smi >/dev/null || die "nvidia-smi not found: install the NVIDIA driver and reboot"
  out="$(nvidia-smi --query-gpu=index,uuid,name,memory.total,memory.free --format=csv,noheader,nounits 2>&1)" ||
    die "nvidia-smi failed, so the NVIDIA driver is not working: $out"
  pid="$(systemctl show -p MainPID --value aisathome-llama 2>/dev/null || true)"
  if [[ "$pid" =~ ^[1-9][0-9]*$ ]]; then
    apps="$(nvidia-smi --query-compute-apps=gpu_uuid,pid,used_memory --format=csv,noheader,nounits 2>/dev/null || true)"
  fi
  while IFS=, read -r idx uuid name total free; do
    idx="$(trim "$idx")" uuid="$(trim "$uuid")" name="$(trim "$name")" total="$(trim "$total")" free="$(trim "${free:-}")"
    [[ -n "$idx" ]] || continue
    [[ "$total" =~ ^[0-9]+$ && "$free" =~ ^[0-9]+$ ]] || die "cannot read the GPU memory from nvidia-smi: $out"
    gpu_count=$((gpu_count + 1))
    own=0
    if [[ -n "$apps" ]]; then
      own="$(awk -F', *' -v u="$uuid" -v p="$pid" '$1 == u && $2 == p && $3 ~ /^[0-9]+$/ { s += $3 } END { print s + 0 }' <<<"$apps")"
    fi
    free=$((free + own))
    if (( total > gpu_total || (total == gpu_total && free > gpu_free) )); then
      gpu_idx="$idx" gpu_uuid="$uuid" gpu_name="$name" gpu_total="$total" gpu_free="$free" gpu_own="$own"
    fi
  done <<<"$out"
  [[ $gpu_count -gt 0 ]] || die "nvidia-smi lists no GPU"
  tier="$(tier_for_total "$gpu_total")"
  [[ -n "$tier" ]] ||
    die "GPU $gpu_idx ($gpu_name) has $gpu_total MiB of memory; the network needs a card with 16 GB or more (standard tier: 80k context on 16 GB cards; big tier: 192k on 24 GB and more)."
  [[ $gpu_count -eq 1 ]] || gpu_pin="Environment=CUDA_VISIBLE_DEVICES=$gpu_uuid"
  echo "GPU $gpu_idx: $gpu_name, $gpu_total MiB: the $tier tier ($(tier_text "$tier"))${gpu_pin:+; llama-server uses only this GPU}" >&2
}

# tier_text <tier>: the tier's model file and context, for messages.
tier_text() { local f; f="$(tier_model "$1")"; f="${f%% *}"; f="${f##*/Qwen3.8-27B-}"; echo "${f%.gguf}, $(( $(tier_ctx "$1") / 1024 ))k context"; }

# fit_advice: what to do when the tier does not fit in the card's free memory.
fit_advice() {
  if [[ "$tier" = standard ]]; then
    echo "On a 16 GB card the standard tier leaves room for a light desktop only (about 1.5 GB in all for the screen and other programs). Close programs that use the GPU (nvidia-smi lists them; a browser can take several hundred MB), or connect the monitor to the motherboard's graphics (integrated GPU), and run the installer again."
  else
    echo "Close programs that use the GPU (nvidia-smi lists them; a desktop with a browser can take 2 GB) or connect the monitor to the motherboard's graphics, and run the installer again."
  fi
}

# check_fit: the tier must fit in the card's free memory now; it is never shrunk to fit.
check_fit() {
  local need; need="$(tier_min_free "$tier")"
  if [[ $custom = 1 ]]; then
    echo "memory: test settings (SLOTS, CTX_PER_SLOT or KV_TYPE), so the $tier tier's check is skipped; $gpu_free MiB free" >&2
    return 0
  fi
  (( gpu_free >= need )) ||
    die "GPU $gpu_idx ($gpu_name) has $gpu_free MiB free; the $tier tier ($(tier_text "$tier")) needs $need MiB free. $(fit_advice)"
  echo "memory: $gpu_free MiB free${gpu_own:+$( (( gpu_own > 0 )) && echo " (incl. $gpu_own MiB of this node's running llama-server)")}, the $tier tier needs $need: fits" >&2
}

# llama_build_number: the llama.cpp build (the N of release bN) of the llama-server this install
# uses: the bundle's BUILD-INFO, else what `llama-server --version` prints ("version: 0.9.0-dev
# (build 12342, commit 0fb8f1577)", or the older "version: 12342 (0fb8f157)"); 0 when unknown.
llama_build_number() {
  local v=""
  if [[ $bundle = 1 ]]; then
    v="$(info llama_cpp_tag)"; v="${v#b}"
  elif [[ -n "$llama_build" && -x "$llama_build/bin/llama-server" ]]; then
    v="$("$llama_build/bin/llama-server" --version 2>&1 | sed -n -e 's/.*(build \([0-9][0-9]*\), commit .*/\1/p' -e 's/^version: \([0-9][0-9]*\) (.*/\1/p' | head -n1)"
  fi
  # A build from a shallow clone counts 1 commit (v0.5.0's bundle said "build 1"): not a release.
  [[ "$v" =~ ^[0-9]+$ ]] && (( v >= 1000 )) && echo "$v" || echo 0
}

# The speculation an earlier install chose (/etc/aisathome-node/speculation), so an update keeps
# it without --speculation.
installed_spec() {
  local v=""
  [[ -r "$conf/speculation" ]] && v="$(head -c 16 "$conf/speculation" | tr -dc 'a-z0-9')"
  case "$v" in mtp|dflash2) echo "$v" ;; *) echo off ;; esac
}

# pick_spec (after pick_tier and check_fit): the speculation this node runs: --speculation, else
# an earlier install's, when the node qualifies (big tier, a new enough llama.cpp, free memory for
# the draft model); otherwise off, and it says why. Sets spec, draft_target and the draft file.
spec=off draft_target="" draft_url="" draft_sha="" draft_size=0
pick_spec() {
  local want="${want_spec:-$(installed_spec)}" build need min
  spec=off
  [[ "$want" != off ]] || return 0
  build="$(llama_build_number)" min="$(spec_min_build "$want")" need=$(( $(tier_min_free "$tier") + $(spec_extra "$want") ))
  if [[ "$tier" != big ]]; then
    echo "speculation: off; $want runs on the big tier (24 GB cards) only, and this card is in the $tier tier" >&2
  elif (( build < min )); then
    echo "speculation: off; $want needs llama.cpp b$min or newer, and this llama-server is $( (( build > 0 )) && echo "b$build" || echo "an unknown build")" >&2
  elif [[ $custom = 0 ]] && (( gpu_free < need )); then
    echo "speculation: off; $want needs $need MiB free on the card, and it has $gpu_free" >&2
  else
    spec="$want"
    read -r draft_url draft_sha draft_size <<<"$(spec_model "$spec")"
    draft_target="/var/lib/aisathome-node/models/${draft_url##*/}"
    echo "speculation: $spec (llama.cpp b$build, $gpu_free MiB free, $need needed), draft model ${draft_url##*/}" >&2
  fi
}

# fetch_draft <file>: download the draft model to <file> and verify it, as fetch_model does for
# the model (the same function, with the draft's file).
fetch_draft() {
  local model_url="$draft_url" model_sha="$draft_sha" model_size="$draft_size"
  fetch_model "$1"
}

# speed_probe <url>: decode speed after a ~$speed_ctx-token prompt, from llama-server's own
# timings: prints "<tokens/s> <prompt tokens> <prompt seconds>". The prompt is a fixed text's
# tokens repeated (decode speed depends on the context's length, not its content); no prompt
# cache, 256 tokens written.
speed_probe() {
  python3 - "$1" "$speed_ctx" <<'PY'
import json, sys, urllib.request
url, n = sys.argv[1].rstrip("/"), int(sys.argv[2])
text = ("The river town kept its records in a stone house by the bridge. Every spring the clerks "
        "counted boats, bales of wool, barrels of salt and the names of travellers, and every autumn "
        "they argued about the numbers. A merchant from the coast once paid in copper coins stamped "
        "with a heron; nobody could say where they were minted. ")
def post(path, body, timeout):
    req = urllib.request.Request(url + path, json.dumps(body).encode(), {"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.load(r)
try:
    toks = post("/tokenize", {"content": text}, 60)["tokens"]
    prompt = (toks * (n // len(toks) + 1))[:n]
    t = post("/completion", {"prompt": prompt, "n_predict": 256, "temperature": 0, "ignore_eos": True,
                             "cache_prompt": False}, 1200)["timings"]
    print(f'{t["predicted_per_second"]:.1f} {t["prompt_n"]} {t["prompt_ms"] / 1000:.0f}')
except Exception as e:
    print(f"{type(e).__name__}: {e}"); sys.exit(1)
PY
}

# speed_check: llama-server is up; refuse cards that write too slowly after a long prompt.
speed_check() {
  local r tps n secs
  echo "speed test: a ${speed_ctx}-token prompt, then 256 tokens (a minute or two)" >&2
  if ! r="$(speed_probe "${engine_url:-http://127.0.0.1:8000}")"; then
    systemctl disable --now aisathome-node aisathome-llama >/dev/null 2>&1 || true
    die "the speed test failed ($r), so the installer stopped the node. Details: journalctl -u aisathome-llama"
  fi
  read -r tps n secs <<<"$r"
  echo "speed: $tps tokens/s after a $n-token prompt (read in $secs s); the network needs at least $min_tps" >&2
  if LC_ALL=C awk -v a="$tps" -v b="$min_tps" 'BEGIN { exit !(a + 0 < b + 0) }'; then
    systemctl disable --now aisathome-node aisathome-llama >/dev/null 2>&1 || true
    die "GPU $gpu_idx ($gpu_name) writes $tps tokens/s after a $n-token prompt; the network needs at least $min_tps, so the installer stopped the node (users would wait too long on long prompts). If other programs used the GPU during the test (BOINC GPU tasks, games, video), stop them and run the installer again."
  fi
}

# check_model_file <file> <hash 0|1>: a --model file must be the tier's pinned model (size, and
# with 1 the SHA-256 too): a tier never mixes quants. The other tier's file gets its own message.
check_model_file() {
  local size got other t
  size="$(stat -c %s "$1")"
  if [[ "$size" != "$model_size" ]]; then
    for t in standard big; do
      [[ "$t" != "${tier:-}" ]] || continue
      read -r _ _ other <<<"$(tier_model "$t")"
      [[ "$size" = "$other" ]] &&
        die "$1 is the $t tier's model ($(tier_text "$t")); this card is in the ${tier:-other} tier, which runs ${model_name}. Leave out --model and the installer downloads it, or download it with --download-only <dir> --tier ${tier:-<tier>}."
    done
    die "$1 is not the network's model: $size bytes, the pinned ${model_name} has $model_size. Leave out --model and the installer downloads it."
  fi
  [[ "$2" = 1 ]] || return 0
  echo "model: checking $1 (about a minute)" >&2
  got="$(sha256sum "$1" | cut -d' ' -f1)"
  [[ "$got" = "$model_sha" ]] ||
    die "$1 is not the network's model: sha256 $got, the pinned ${model_name} has $model_sha. Leave out --model and the installer downloads it."
  echo "model: $1, sha256 $got (verified)" >&2
}
# ---- end of tier -----------------------------------------------------------------------------

# A temporary directory, removed on exit; created on first use (call workdir, then use $work).
work="" token_aside=0 node_was_on=0 park_token=0 parked_token=""
# on_exit: removes the work folder, and puts an installed token back if the install stopped before
# its link step (token_aside=1: set_aside_before_link, restore_unlinked).
on_exit() { [[ -z "$work" ]] || rm -rf "$work"; [[ $token_aside = 0 ]] || restore_unlinked "$conf"; }
trap on_exit EXIT
workdir() { [[ -n "$work" ]] || work="$(mktemp -d)"; }

# Download the node binary for this architecture from a release and verify it against the
# release's SHA256SUMS. This catches corrupt downloads; it does not protect against a
# compromised release, since the checksums come from the same place.
fetch_node_release() {
  local arch url want got
  case "$(uname -m)" in
    x86_64|amd64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) die "no release binary for $(uname -m); build aisathome-node and use --node-bin" ;;
  esac
  local asset="aisathome-node-linux-$arch"
  if [[ "$node_release" = latest ]]; then url="$release_base/latest/download"; else url="$release_base/download/$node_release"; fi
  workdir
  for f in "$asset" SHA256SUMS; do
    curl -fsSL --proto '=https,file' --proto-redir '=https' --retry 3 -o "$work/$f" "$url/$f" ||
      die "download failed: $url/$f"
  done
  want="$(awk -v a="$asset" '$2 == a || $2 == "*" a { print $1 }' "$work/SHA256SUMS")"
  [[ "$want" =~ ^[0-9a-f]{64}$ ]] || die "no checksum for $asset in $url/SHA256SUMS"
  got="$(sha256sum "$work/$asset" | cut -d' ' -f1)"
  [[ "$got" = "$want" ]] || die "checksum mismatch for $asset: got $got, SHA256SUMS says $want"
  chmod 0755 "$work/$asset"
  node_bin="$work/$asset"
  echo "aisathome-node: $asset from $url, sha256 $got (verified)" >&2
}

# ---- Local node UI ---------------------------------------------------------------------
# aisathome-node serves a status page with pause/resume and engine restart on
# http://127.0.0.1:$UI_PORT (README.md, "Local UI"). Kept in these functions, apart from the
# rest of the installer. UI_PORT=off installs no UI.
ui_port="${UI_PORT:-7381}"
ui_password=/etc/aisathome-node/ui-password
ui_restart_trigger=/var/lib/aisathome-node/restart-engine

node_ui_check() {
  [[ "$ui_port" = off || ( "$ui_port" =~ ^[1-9][0-9]{3,4}$ && "$ui_port" -le 65535 && "$ui_port" != 8000 ) ]] ||
    die "UI_PORT must be a port from 1000 to 65535 other than 8000 (llama-server), or off" 2
}

# The agent's systemd drop-in: UI settings, state file and the GPU access nvidia-smi needs.
node_ui_dropin() {
  cat <<UNIT
[Service]
Environment=AISATHOME_UI_PORT=$ui_port
Environment=AISATHOME_UI_PASSWORD_FILE=$ui_password
Environment=AISATHOME_STATE_FILE=/var/lib/aisathome-node/node-state.json
Environment=AISATHOME_RESTART_TRIGGER=$ui_restart_trigger
Environment="AISATHOME_PRESET=$tier_label: $slots x $ctx_per_slot tokens, KV $kv_type"
# Read-only GPU facts for the UI (name, memory, temperature) through nvidia-smi.
$gpu_devices
UNIT
}

node_ui_install() {
  local d=/etc/systemd/system/aisathome-node.service.d
  install -d -m 0755 "$d"
  node_ui_dropin > "$d/local-ui.conf"
  [[ "$ui_port" != off ]] || return 0
  # Password: random, kept across re-installs; readable by root and the agent only.
  if [[ ! -s "$ui_password" ]]; then
    (umask 077; head -c 18 /dev/urandom | base64 | tr '+/' '-_' > "$ui_password")
  fi
  chown root:aisathome "$ui_password"; chmod 0640 "$ui_password"
  # Engine restart from the UI: the unprivileged agent writes a file; this root path unit
  # restarts llama-server (and with it the agent). No sudo, polkit or capability needed.
  cat > /etc/systemd/system/aisathome-node-restart.path <<UNIT
[Unit]
Description=AIs@home node: restart llama-server when the local UI asks

[Path]
PathChanged=$ui_restart_trigger

[Install]
WantedBy=multi-user.target
UNIT
  cat > /etc/systemd/system/aisathome-node-restart.service <<UNIT
[Unit]
Description=AIs@home node: restart llama-server (asked by the local UI)

[Service]
Type=oneshot
ExecStart=/bin/systemctl restart aisathome-llama.service
UNIT
  systemctl daemon-reload
  systemctl enable --now aisathome-node-restart.path
  # "aisathome-node status" on PATH, with this UI port.
  printf '#!/bin/sh\nexport AISATHOME_UI_PORT="${AISATHOME_UI_PORT:-%s}"\nexec /opt/aisathome-node/bin/aisathome-node "$@"\n' \
    "$ui_port" > /usr/local/bin/aisathome-node
  chmod 0755 /usr/local/bin/aisathome-node
  # A menu entry when a desktop is installed.
  if compgen -G '/usr/share/xsessions/*.desktop' >/dev/null || compgen -G '/usr/share/wayland-sessions/*.desktop' >/dev/null; then
    install -d -m 0755 /usr/local/share/applications /usr/local/share/icons/hicolor/scalable/apps
    cat > /usr/local/share/icons/hicolor/scalable/apps/aisathome-node.svg <<'SVG'
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" width="32" height="32"><rect width="32" height="32" rx="7" fill="#c2410c"/><text x="16" y="22" text-anchor="middle" font-family="monospace" font-size="17" font-weight="700" fill="#fff8f0">@</text></svg>
SVG
    cat > /usr/local/share/applications/aisathome-node.desktop <<DESKTOP
[Desktop Entry]
Type=Application
Name=AIs@home node
Comment=Status, pause and resume of this computer's AIs@home node
Exec=xdg-open http://127.0.0.1:$ui_port/
Icon=aisathome-node
Terminal=false
Categories=Network;System;Monitor;
DESKTOP
  fi
}

node_ui_message() {
  [[ "$ui_port" != off ]] || return 0
  cat <<MSG
Local UI (this computer only):               http://127.0.0.1:$ui_port/   or: aisathome-node status
UI password:                                 sudo cat $ui_password
From another computer:                       ssh -N -L $ui_port:127.0.0.1:$ui_port <user>@<this machine>
MSG
}
node_ui_check
# ---- end of local node UI ----------------------------------------------------------------

gb() { LC_ALL=C awk -v b="$1" 'BEGIN { printf "%.1f", b / 1e9 }'; }
trim() { local s="$1"; s="${s#"${s%%[![:space:]]*}"}"; printf '%s' "${s%"${s##*[![:space:]]}"}"; }
# ver_ge A B: version A >= version B.
ver_ge() { [[ "$(printf '%s\n%s\n' "$1" "$2" | sort -V | head -n1)" = "$2" ]]; }
# info KEY: a value from the bundle's BUILD-INFO (key=value lines; read, never sourced).
info() { sed -n "s/^$1=//p" "$here/BUILD-INFO" | head -n1; }

# The prebuilt llama-server needs x86_64 with AVX2, glibc 2.35 or newer, and an NVIDIA
# driver new enough for its CUDA version, on a GPU it was compiled for.
check_bundle() {
  local caps min min12 cuda out idx name drv cap n=0 missing
  caps="$(info compute_caps)" min="$(info min_driver)" min12="$(info min_driver_cc12)" cuda="$(info cuda_version)"
  [[ "$caps" =~ ^[0-9]+\.[0-9]+(\ [0-9]+\.[0-9]+)*$ && "$min" =~ ^[0-9]+(\.[0-9]+)+$ && "$min12" =~ ^[0-9]+(\.[0-9]+)+$ ]] ||
    die "$here/BUILD-INFO is incomplete; download the bundle again"
  echo "llama-server: prebuilt, llama.cpp $(info llama_cpp_tag) ($(info llama_cpp_commit | cut -c1-12)), CUDA $cuda, compute capability $caps" >&2
  [[ "$(uname -m)" = x86_64 ]] || die "the prebuilt bundle is for x86_64; this machine is $(uname -m). Build llama.cpp and use --llama-build."
  grep -qw avx2 /proc/cpuinfo || die "this CPU has no AVX2, which the prebuilt llama-server needs. Build llama.cpp and use --llama-build."
  command -v nvidia-smi >/dev/null ||
    die "nvidia-smi not found: install the NVIDIA driver ($min or newer; $min12 or newer for an RTX 50) and reboot"
  out="$(nvidia-smi --query-gpu=index,name,driver_version,compute_cap --format=csv,noheader 2>&1)" ||
    die "nvidia-smi failed, so the NVIDIA driver is not working: $out"
  while IFS=, read -r idx name drv cap; do
    idx="$(trim "$idx")" name="$(trim "$name")" drv="$(trim "$drv")" cap="$(trim "${cap:-}")"
    [[ -n "$idx" ]] || continue
    [[ "$drv" =~ ^[0-9]+(\.[0-9]+)+$ && "$cap" =~ ^[0-9]+\.[0-9]+$ ]] || die "cannot read nvidia-smi's output: $out"
    ver_ge "$drv" "$min" || die "NVIDIA driver $drv is too old for the prebuilt llama-server (CUDA $cuda): it needs $min or newer. Update the driver (Ubuntu: sudo ubuntu-drivers install, then reboot), or build llama.cpp yourself and use --llama-build."
    case " $caps " in
      *" $cap "*) ;;
      *) die "GPU $idx ($name) has compute capability $cap; the prebuilt llama-server supports $caps (RTX 30, 40 and 50 series). Build llama.cpp for this GPU and use --llama-build." ;;
    esac
    if [[ "${cap%%.*}" -ge 12 ]] && ! ver_ge "$drv" "$min12"; then
      die "GPU $idx ($name) needs NVIDIA driver $min12 or newer; this machine has $drv. Update the driver, then reboot."
    fi
    echo "GPU $idx: $name, driver $drv, compute capability $cap: supported" >&2
    n=$((n + 1))
  done <<<"$out"
  [[ $n -gt 0 ]] || die "nvidia-smi lists no GPU"
  missing="$(LD_LIBRARY_PATH="$here/lib" ldd "$here/bin/llama-server" 2>&1 | grep 'not found' || true)"
  [[ -z "$missing" ]] || die "the prebuilt llama-server cannot run here (it needs glibc 2.35 or newer, e.g. Ubuntu 22.04 or Debian 12):
$missing"
}

# model_missing <file>: bytes still to download for <file> (a complete file counts as 0;
# fetch_model checks its hash).
model_missing() {
  local have=0
  if [[ -f "$1" && "$(stat -c %s "$1")" = "$model_size" ]]; then echo 0; return; fi
  if [[ -f "$1.part" ]]; then have="$(stat -c %s "$1.part")"; fi
  if (( have > model_size )); then have=0; fi
  echo $((model_size - have))
}

# check_disk <dir> <bytes>: <dir> (or its nearest existing parent) must have <bytes> free.
check_disk() {
  local d="$1" kb
  while [[ ! -d "$d" ]]; do d="$(dirname "$d")"; done
  kb="$(df -Pk "$d" | awk 'NR == 2 { print $4 }')"
  [[ "$kb" =~ ^[0-9]+$ ]] || die "cannot read the free disk space of $d"
  (( kb * 1024 >= $2 )) ||
    die "not enough disk space on $d: need $(gb "$2") GB for the model download, $(gb $((kb * 1024))) GB free. Free some space and run the installer again (it resumes)."
  echo "disk: $(gb $((kb * 1024))) GB free on $d, $(gb "$2") GB needed" >&2
}

# fetch_model <file>: download the model to <file> and verify its SHA-256. An interrupted
# download resumes from <file>.part; a complete <file> with the right hash is kept.
fetch_model() {
  local dest="$1" part="$1.part" have=0 got rc i progress=(-sS)
  [[ -t 2 ]] && progress=(-#)
  if [[ -f "$dest" && "$(stat -c %s "$dest")" = "$model_size" ]]; then
    echo "model: checking the existing $dest (about a minute)" >&2
    got="$(sha256sum "$dest" | cut -d' ' -f1)"
    if [[ "$got" = "$model_sha" ]]; then echo "model: $dest, sha256 $got (verified)" >&2; return 0; fi
    echo "model: $dest is not the pinned model; downloading it" >&2
  fi
  if [[ -f "$part" ]]; then have="$(stat -c %s "$part")"; fi
  if (( have > model_size )); then rm -f "$part"; have=0; fi
  check_disk "$(dirname "$dest")" $((model_size - have + reserve))
  if (( have < model_size )); then
    if (( have > 0 )); then
      echo "model: resuming at $(gb "$have") of $(gb "$model_size") GB from $model_url" >&2
    else
      echo "model: downloading $(gb "$model_size") GB from $model_url" >&2
    fi
    # Retries resume where the last attempt stopped; a stalled transfer (under 10 kB/s for
    # a minute) counts as interrupted. HTTP errors (22) and missing files (37) are final; a
    # server that cannot resume (33) gets a fresh download.
    for i in 1 2 3 4 5; do
      rc=0
      curl -fL "${progress[@]}" --proto '=https,file' --proto-redir '=https' --retry 3 --retry-delay 5 \
        --connect-timeout 30 --speed-limit 10000 --speed-time 60 -C - -o "$part" "$model_url" || rc=$?
      [[ $rc -ne 0 ]] || break
      case $rc in
        22|37) die "model download failed: $model_url (curl exit $rc)" ;;
        33) rm -f "$part" ;;
      esac
      (( i < 5 )) || die "model download failed 5 times (curl exit $rc); run the installer again to resume"
      echo "model: download interrupted (curl exit $rc); resuming in 10 s" >&2
      sleep 10
    done
  fi
  echo "model: checking sha256 (about a minute)" >&2
  got="$(sha256sum "$part" | cut -d' ' -f1)"
  if [[ "$got" != "$model_sha" ]]; then
    rm -f "$part"
    die "model checksum mismatch: got $got, want $model_sha. The download was deleted; run the installer again to download it again."
  fi
  mv -f "$part" "$dest"
  echo "model: $dest, sha256 $got (verified)" >&2
}

# install_libs <build dir> <lib dir>: llama.cpp's shared libraries, with modes set here rather
# than taken from the source. A bundle unpacked under umask 077 has owner-only files, which
# the aisathome service user then cannot load ("cannot open shared object file", 2026-10-15).
install_libs() {
  find "$1" -name '*.so*' \( -type f -o -type l \) -exec cp -P {} "$2"/ \;
  find "$2" -type f -exec chmod u=rwX,go=rX {} +
}

# pick_node_id <token> [installed id]: the node id for a generated node.json: --node-id,
# else the id that a token from the account page carries ("nt-<id>.<secret>", as
# node.TokenNodeID reads it), else the installed node.json's, else the host name. (The node
# also takes an account token's id over node_id, with a warning.)
pick_node_id() {
  [[ -z "$node_id" ]] || return 0
  if [[ "$1" =~ ^nt-([na]-[0-9a-f]{8})\. ]]; then node_id="${BASH_REMATCH[1]}"; return 0; fi
  if [[ -n "${2:-}" ]]; then
    node_id="$2"
    [[ "$node_id" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$ ]] ||
      die "the installed node id '$node_id' is not valid; pass --node-id" 2
    return 0
  fi
  node_id="$(hostname -s 2>/dev/null || true)"
  [[ "$node_id" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$ ]] ||
    die "the host name '$node_id' is not a valid node id; pass --node-id" 2
}

# interactive: stdin is a terminal (a person answers the token prompt).
interactive() { [[ -t 0 ]]; }

# read_token <token.env>: the node token from stdin, never from argv. An installed token counts only
# together with its recorded issuing gateway (token.env's AISATHOME_TOKEN_GATEWAY, written with every
# token): one without a record, or whose record is not this install's gateway, is never kept or sent
# anywhere; it is moved aside (park_token, set_aside_before_link). A usable installed token is kept with
# --keep-token, or when a person at a terminal presses Enter on an empty line; piped empty input is still an
# error, so a script never keeps a token by accident. Without one, Enter at a terminal chooses linking
# (link=1, as --link does): no token now, and the install ends with `aisathome-node link`. --anonymous keeps
# a usable token (the node stays as it is and is updated) unless its gateway says it is gone or a held node
# without an account's (token_state; the reinstall a hold message advises registers anew), and registers
# otherwise.
#
# Called under the writers' lock (take_lock), and token.env is read once: the token and its record come from
# the same contents, so a concurrent change can never pair one file's token with another's record.
read_token() {
  local env="" line old="" rec=""
  [[ ! -r "$1" ]] || env="$(cat "$1")"
  while IFS= read -r line; do
    case $line in
      AISATHOME_NODE_TOKEN=*) [[ -n $old ]] || old="$(trim "${line#AISATHOME_NODE_TOKEN=}")" ;;
      AISATHOME_TOKEN_GATEWAY=*) [[ -n $rec ]] || rec="$(trim "${line#AISATHOME_TOKEN_GATEWAY=}")" ;;
    esac
  done <<<"$env"
  if [[ -n "$old" && "$rec" != "$gateway_url" ]]; then
    [[ $keep_token = 0 ]] || die "--keep-token: the installed token was issued by ${rec:-an unrecorded gateway}, not $gateway_url, and is never sent there: link the node (--link), or paste a node token for $gateway_url" 2
    park_token=1 parked_token="$old" old=""
    echo "token: the installed one was issued by ${rec:-an unrecorded gateway}, not $gateway_url: it is moved aside ($1.unlinked) and never sent there" >&2
  fi
  if [[ $anonymous = 1 ]]; then
    if [[ -n "$old" ]]; then
      case "$(token_state "$old" "$gateway_url")" in
        gone) park_token=1
          echo "account: $gateway_url no longer knows the installed token (deleted or revoked): it is moved aside ($1.unlinked), and this node registers without an account" >&2 ;;
        held) park_token=1
          echo "account: the installed token is a held node's without an account: it is moved aside ($1.unlinked), and this node registers again" >&2 ;;
        *) anonymous=0 keep_token=1 token="$old"
          if [[ $old == nt-a-* ]]; then echo "account: this node already runs without an account on $gateway_url: it keeps its token and is updated" >&2
          else echo "account: this node is linked to an account on $gateway_url: it keeps its token and is updated (not made a node without an account)" >&2; fi
          return 0 ;;
      esac
    fi
    token=""; echo "account: none; the install ends with registering this node without an account (science only)" >&2; return 0
  fi
  if [[ $link = 1 ]]; then
    token=""; echo "account: the install ends with linking this node to your account in the browser" >&2; return 0
  fi
  if [[ $keep_token = 1 ]]; then
    [[ -n "$old" ]] || die "--keep-token: no installed token in $1" 2
    token="$old"; echo "token: keeping the installed one" >&2; return 0
  fi
  if interactive; then
    if [[ -n "$old" ]]; then echo "Paste a node token (input hidden), or press Enter to keep the installed one (to link the node to another account instead: --link):" >&2
    else echo "Press Enter to link this node to your account in the browser at the end of the install, or paste a node token (advanced; input hidden):" >&2; fi
  fi
  token=""
  IFS= read -rs token || true
  # Whitespace around it (a trailing space, a CR) goes, as systemd drops it from the saved value; what is
  # left must be exactly the form the gateway issues (the agent's rule, node.IssuedAnyTokenNodeID), so no
  # variant of another token can pass the checks below.
  token="$(trim "$token")"
  [[ -z "$token" || "$token" =~ ^nt-[na]-[0-9a-f]{8}\.[A-Za-z0-9_-]{43}$ ]] ||
    die "the token given is not a node token (nt-n-… or nt-a-… as the account page gives it; not shown here)" 2
  if [[ -z "$token" && -n "$old" ]] && interactive; then
    token="$old"; echo "token: keeping the installed one" >&2; return 0
  fi
  if [[ -z "$token" ]] && interactive; then
    link=1; echo "account: the install ends with linking this node to your account in the browser" >&2; return 0
  fi
  [[ -n "$token" ]] || die "no token on stdin${old:+ (to keep the installed one: --keep-token)}; to link the node to your account in the browser instead: --link"
  # The token given is the one being moved aside (another gateway's, or without a record): it would be
  # recorded as this gateway's and sent here. Refused (code review 2026-11-24).
  [[ -z "$parked_token" || "$token" != "$parked_token" ]] ||
    die "the token given is the installed one, which is not $gateway_url's, so it is never sent there: link the node (--link), or give a node token from $gateway_url" 2
}

# token_state <token> <gateway_url>: what the gateway says of a node token (GET /v1/node/self): "gone" (it does
# not know it: deleted or revoked), "held" (a held node without an account), else "ok" (also when it does not
# answer: the token stays). Only ever asked of the token's recorded issuer (read_token checks the record
# first), which gets this token anyway each time the node connects; the token goes in a header read from
# stdin, never on a command line.
token_state() {
  local base out code
  gateway_url_ok "$2" || die "not asking $2 about the installed token: it is not encrypted (ws:// only to this machine)" 2
  base="${2/#wss:/https:}"; base="${base/#ws:/http:}"; base="${base%%/v1/*}"
  out="$(printf 'Authorization: Bearer %s\n' "$1" | curl -s --max-time 20 --proto '=https,http' -H @- -w '\n%{http_code}' "$base/v1/node/self" || true)"
  code="${out##*$'\n'}"
  if [[ $code == 401 ]]; then echo gone
  elif [[ $code == 200 && $out =~ \"anonymous\":[[:space:]]*true && $out =~ \"state\":[[:space:]]*\"held\" ]]; then echo held
  else echo ok; fi
}

# take_lock: the writers' lock (node.lock beside node.json; aisathome-node link and register and the Windows
# setup page take the same, node/nodeconf), on fd 8. The installer holds it from reading the installed token
# until the rollback (restore_unlinked) is disarmed right before its link or register step, which take the
# lock themselves: nothing else changes token.env or node.json between this install's decision, its writes
# and a rollback of them.
take_lock() {
  mkdir -p -m 0750 "$conf"
  exec 8>"$conf/node.lock"
  flock -w 60 8 || die "another install or aisathome-node link is changing this node; run the installer again when it is done"
}

# A gateway change on a node that has a token (linking, or a new token pasted for the new gateway): the
# installed token is the old gateway's and must never be sent to the new one. It is moved aside
# (token.env.unlinked, with the installed node.json copied to node.json.unlinked) BEFORE the new
# node.json is installed, so at no point do the new node.json and the old token.env sit side by side:
# the node's unit (ConditionFileNotEmpty) stays off until token.env has the new token (the pasted one, or
# `aisathome-node link`'s). The node is then stopped, still before the new node.json: a start already
# under way may have read the old token.env. If it does not stop, the install stops (the EXIT trap puts
# the token back).
# set_aside_before_link <config dir> <new node.json>: does that for a token read_token will not keep or
# send (park_token: no record of its gateway, or another gateway's).
set_aside_before_link() {
  local now state
  now="$(installed_value "$2" gateway_url)"
  [[ -s "$1/token.env" && $park_token = 1 ]] || return 0
  state="$(systemctl show -p ActiveState --value aisathome-node 2>/dev/null || true)"
  [[ $state != active && $state != activating && $state != reloading ]] || node_was_on=1
  rm -f "$1/node.json.unlinked"
  [[ ! -e "$1/node.json" ]] || cp -p "$1/node.json" "$1/node.json.unlinked"
  mv -f "$1/token.env" "$1/token.env.unlinked"
  token_aside=1
  stop_node || die "the node did not stop (systemctl stop aisathome-node), so the install stops before it changes the gateway; the installed token and node.json stay as they were"
  echo "The installed token is not $now's: moved aside to $1/token.env.unlinked." >&2
}

# stop_node: stops the node's service; succeeds only if it is off afterwards (or not installed).
stop_node() {
  local state
  if ! systemctl stop aisathome-node 2>/dev/null; then
    [[ "$(systemctl show -p LoadState --value aisathome-node 2>/dev/null)" = not-found ]] || return 1
  fi
  state="$(systemctl show -p ActiveState --value aisathome-node 2>/dev/null)" || return 1
  [[ $state = inactive || $state = failed ]]
}

# restore_unlinked <config dir>: the install stopped before its link step started (an error, Ctrl+C):
# the token goes back together with the node.json it was made for, and a node that was on starts again,
# so the node is as it was. Once the link step has started, a link that did not happen leaves the token
# aside and the node off (no token for the new gateway). Without a copy of the old node.json the token
# stays aside.
#
# Order: the node is stopped first (a start under way may have read the new files), then a token this
# install wrote is removed, then the old node.json and only then the old token come back: at no moment
# does a token sit beside another gateway's node.json.
restore_unlinked() {
  [[ $token_aside = 1 ]] || return 0
  token_aside=0
  [[ -e "$1/token.env.unlinked" && -e "$1/node.json.unlinked" ]] || return 0
  if ! stop_node; then
    # Nothing new was written yet (no token.env, node.json as it was): the old pair goes back as it was.
    if [[ ! -e "$1/token.env" ]] && cmp -s "$1/node.json" "$1/node.json.unlinked"; then
      rm -f "$1/node.json.unlinked"; mv -f "$1/token.env.unlinked" "$1/token.env"
      echo "The install stopped before changing the gateway: the installed token is back in $1." >&2
      return 0
    fi
    echo "The install stopped, and the node did not stop: the installed token stays aside in $1/token.env.unlinked." >&2
    return 0
  fi
  rm -f "$1/token.env"
  mv -f "$1/node.json.unlinked" "$1/node.json"
  mv -f "$1/token.env.unlinked" "$1/token.env"
  echo "The install stopped before linking: the installed token and its node.json are back in $1." >&2
  if [[ $node_was_on = 1 ]]; then
    systemctl start aisathome-node || echo "The node did not start again: sudo systemctl start aisathome-node" >&2
  fi
}

# register_node: the last step of --anonymous, after the speed test (a machine that cannot serve never
# registers): `aisathome-node register` gets the node a token of its own from the gateway (science only)
# and writes it to token.env (0600). It returns non-zero when the gateway refused (limits, a full pool).
register_node() {
  echo
  echo "Last step: register this node without an account."
  /opt/aisathome-node/bin/aisathome-node register -accept-terms -config "$conf/node.json" -token-env "$conf/token.env" -no-start
}

# link_node: the last step without a token: `aisathome-node link` shows a link and a code, waits for
# the approval in a browser on any device, and writes the node's token to token.env (0600). It
# returns non-zero when the node was not linked (denied, expired, stopped).
link_node() {
  echo
  echo "Last step: link this node to your AIs@home account."
  /opt/aisathome-node/bin/aisathome-node link -config "$conf/node.json" -token-env "$conf/token.env" -no-start
}

# node.json for the tier, when no --node-config is given (inputs validated above).
gen_node_config() {
  printf '{\n  "node_id": "%s",\n  "gateway_url": "%s",\n  "upstream": "http://127.0.0.1:8000",\n  "models": [{"name": "%s", "roles": ["deep", "agent"], "slots": %s, "ctx": %s}]\n}\n' \
    "$node_id" "$gateway_url" "$alias" "$slots" "$ctx_per_slot"
}

if [[ -n "$download_only" ]]; then
  [[ -d "$download_only" ]] || die "--download-only: no directory $download_only" 2
  [[ -z "$model" ]] || die "use --download-only or --model, not both" 2
  # The tier's file: --tier, else the tier of this machine's card.
  if [[ -n "$want_tier" ]]; then
    tier="$want_tier"
  else
    command -v nvidia-smi >/dev/null ||
      die "--download-only: no NVIDIA card here to choose the model by; add --tier standard (16 GB cards) or --tier big (24 GB and more)" 2
    pick_tier
  fi
  set_model "$tier"
  echo "model: the $tier tier's $model_name" >&2
  fetch_model "$download_only/$model_name"
  if [[ -n "$want_spec" && "$want_spec" != off ]]; then
    read -r draft_url draft_sha draft_size <<<"$(spec_model "$want_spec")"
    fetch_draft "$download_only/${draft_url##*/}"
  fi
  echo "Downloaded. Install with --model $download_only/$model_name (the file is moved)." >&2
  exit 0
fi

[[ $dry_run = 1 || $EUID -eq 0 ]] || die "run with sudo"
[[ $dry_run = 1 || -n "$llama_build" ]] ||
  die "no llama-server: run install.sh from the prebuilt bundle, or pass --llama-build <llama.cpp/build>" 2
[[ $bundle = 0 ]] || check_bundle
pick_tier
check_fit
pick_spec
set_model "$tier"
set_engine
if [[ -n "$model" ]]; then
  [[ -f "$model" ]] || die "no model file: $model"
  check_model_file "$model" 0
else
  echo "model: $model_url (sha256 $model_sha, $(gb "$model_size") GB) to $target" >&2
  check_disk "$(dirname "$target")" $(( $(model_missing "$target") + reserve ))
fi
[[ $spec = off ]] || echo "draft model: $draft_url (sha256 $draft_sha, $(gb "$draft_size") GB) to $draft_target" >&2
[[ -z "$node_config" || -f "$node_config" ]] || die "no node config: $node_config"
[[ -z "$node_release" ]] || fetch_node_release

if [[ $dry_run = 1 ]]; then
  if [[ -z "$node_config" ]]; then
    [[ -n "$node_id$old_id" ]] || echo "node id: the one an account-page token carries, else the host name" >&2
    [[ -r "$conf/node.json" || ! -e "$conf/node.json" ]] ||
      echo "node.json: $conf/node.json is not readable without sudo; the install keeps its node id and gateway URL" >&2
    pick_node_id "" "$old_id"
    echo "node.json (generated):"; gen_node_config
  fi
  echo "tier: $tier_label: $slots x $ctx_per_slot tokens, KV $kv_type"
  echo "speed test after the install: at least $min_tps tokens/s after a $speed_ctx-token prompt"
  if [[ $anonymous = 1 ]]; then
    echo "account: none; the install ends with registering this node without an account (aisathome-node register); an installed token recorded for this gateway is kept instead (unless that gateway says it is deleted, or a held node without an account's)"
    anon_notice
  else
    [[ $keep_token = 1 ]] || echo "account: $([[ $link = 1 ]] && echo "the install ends" || echo "at a terminal, Enter at the token question ends the install") with linking this node to your account in the browser (aisathome-node link)"
  fi
  echo "$llama_exec"
  echo "local UI: $ui_port"
  node_ui_dropin
  echo "aisathome-llama.service:"; llama_unit
  exit 0
fi

[[ -x "$llama_build/bin/llama-server" ]] || die "no llama-server in $llama_build/bin"
[[ -x "$node_bin" ]] || usage
[[ -z "$node_config" ]] || python3 -m json.tool "$node_config" >/dev/null || die "invalid JSON: $node_config"

# A supplied node.json is made canonical first: its gateway is what the installed token is judged against.
if [[ -n "$node_config" ]]; then
  workdir; canon_node_config "$node_config" "$work/node.canon.json"; node_config="$work/node.canon.json"
  gateway_url="$(installed_value "$node_config" gateway_url)"
  gateway_url_ok "$gateway_url" ||
    die "--node-config: its gateway_url '$gateway_url' must look like wss://host/v1/node/connect (ws:// only to this machine)" 2
fi
take_lock
read_token "$conf/token.env"
[[ $anonymous = 0 ]] || anon_notice
if [[ -z "$node_config" ]]; then
  pick_node_id "$token" "$old_id"
  [[ -z "$old_id$old_gw" ]] || echo "node.json: node id $node_id, gateway $gateway_url" >&2
  workdir; node_config="$work/node.json"; gen_node_config > "$node_config"
  canon_node_config "$node_config" "$work/node.canon.json"; node_config="$work/node.canon.json"
fi

# 1. Unprivileged system user without a login shell.
id aisathome >/dev/null 2>&1 || useradd --system --home-dir /var/lib/aisathome-node --shell /usr/sbin/nologin aisathome

# 2. Binaries and llama.cpp shared libraries. The old lib/ is removed first: no stale
# libraries from an earlier build, and a running llama-server keeps its unlinked copies
# instead of crashing on files overwritten in place. The bundle's BUILD-INFO and licences
# (llama.cpp, NVIDIA CUDA runtime and cuBLAS) go along with the libraries.
rm -rf /opt/aisathome-node/lib
install -d -m 0755 /opt/aisathome-node/bin /opt/aisathome-node/lib
install -m 0755 "$llama_build/bin/llama-server" /opt/aisathome-node/bin/
install -m 0755 "$node_bin" /opt/aisathome-node/bin/aisathome-node
install_libs "$llama_build" /opt/aisathome-node/lib
for f in BUILD-INFO THIRD_PARTY_NOTICES.md licenses; do
  if [[ $bundle = 1 && -e "$llama_build/$f" ]]; then rm -rf "/opt/aisathome-node/$f"; cp -r "$llama_build/$f" /opt/aisathome-node/; fi
done
chown -R root:root /opt/aisathome-node
chmod -R u=rwX,go=rX /opt/aisathome-node

# 3. Model (moved to save disk space, or downloaded and verified) and state directory.
install -d -o aisathome -g aisathome -m 0750 /var/lib/aisathome-node /var/lib/aisathome-node/models
if [[ -z "$model" ]]; then
  fetch_model "$target"
elif [[ "$(readlink -f "$model")" != "$target" ]]; then
  check_model_file "$model" 1
  mv "$model" "$target"
fi
chown aisathome:aisathome "$target"; chmod 0640 "$target"
if [[ $spec != off ]]; then
  # A draft downloaded with --download-only beside the --model file is moved along (fetch_draft
  # then checks it and keeps it), instead of downloading it again.
  if [[ -n "$model" && ! -f "$draft_target" && -f "$(dirname "$model")/${draft_target##*/}" ]]; then
    mv "$(dirname "$model")/${draft_target##*/}" "$draft_target"
  fi
  fetch_draft "$draft_target"
  chown aisathome:aisathome "$draft_target"; chmod 0640 "$draft_target"
fi

# 4. Config and token.
install -d -o root -g aisathome -m 0750 /etc/aisathome-node
# An installed token that is not this gateway's goes aside, and the node stops, before the new node.json.
set_aside_before_link "$conf" "$node_config"
# The canonical node.json (canon_node_config: no token, one key each), installed as checked, byte for byte.
install -o root -g aisathome -m 0640 "$node_config" /etc/aisathome-node/node.json
# Linking (no token now): token.env comes with `aisathome-node link` at the end; an installed one for the
# same gateway (--link on an installed node) stays until then, so a link that does not happen leaves the
# node as it was.
# A token is written with the gateway that issued it (this install's): the agent never sends it elsewhere.
# Only a token kept with its own record, or a different one given for this gateway, gets here; the one moved
# aside never does.
[[ -z "$token" || -z "$parked_token" || "$(trim "$token")" != "$(trim "$parked_token")" ]] || die "not writing the moved-aside token as $gateway_url's" 2
if [[ -n "$token" ]]; then
  umask 077; printf 'AISATHOME_NODE_TOKEN=%s\nAISATHOME_TOKEN_GATEWAY=%s\n' "$token" "$gateway_url" > /etc/aisathome-node/token.env; umask 022
fi
# The speculation asked for (not what ran: a later update on a card with room may turn it on).
printf '%s\n' "${want_spec:-$(installed_spec)}" > /etc/aisathome-node/speculation; chmod 0644 /etc/aisathome-node/speculation
[[ ! -e /etc/aisathome-node/token.env ]] || { chown root:root /etc/aisathome-node/token.env; chmod 0600 /etc/aisathome-node/token.env; }

# 5. systemd units (the sandbox and llama-server's unit are defined above).

# Creates /dev/nvidia-uvm (normally done by a setuid helper on first CUDA use, which the
# sandbox blocks). Runs once, as root, outside the sandbox.
cat > /etc/systemd/system/aisathome-gpu-init.service <<UNIT
[Unit]
Description=AIs@home node: create NVIDIA device nodes for CUDA

[Service]
Type=oneshot
RemainAfterExit=yes
# Not every driver install ships nvidia-modprobe (a Vast VM with driver 535 had none, but
# already had the device nodes; 2026-10-10 install test): use it only when needed.
ExecStart=/bin/sh -c 'test -e /dev/nvidia-uvm && exit 0; command -v nvidia-modprobe >/dev/null && exec nvidia-modprobe -u -c=0; echo "no /dev/nvidia-uvm and no nvidia-modprobe: install it (e.g. apt install nvidia-modprobe)" >&2; exit 1'

[Install]
WantedBy=multi-user.target
UNIT

llama_unit > /etc/systemd/system/aisathome-llama.service

# Watchdog: restart llama-server if /health has failed for 3 checks in a row (one a minute),
# for hangs that do not end the process. Skips the first 5 minutes after a start (loading).
install -d -m 0755 /usr/local/libexec
cat > /usr/local/libexec/aisathome-node-watchdog <<'WD'
#!/bin/sh
state=/run/aisathome-node-watchdog.fails
systemctl is-active -q aisathome-llama || { rm -f "$state"; exit 0; }
since=$(systemctl show -p ActiveEnterTimestampMonotonic --value aisathome-llama)
now=$(cut -d' ' -f1 /proc/uptime | cut -d. -f1)
[ $((now - since / 1000000)) -lt 300 ] && exit 0
if curl -fs -m 10 -o /dev/null http://127.0.0.1:8000/health; then rm -f "$state"; exit 0; fi
n=$(( $(cat "$state" 2>/dev/null || echo 0) + 1 )); echo "$n" > "$state"
if [ "$n" -ge 3 ]; then
  echo "llama-server unhealthy for $n checks; restarting" | systemd-cat -t aisathome-node-watchdog -p warning
  rm -f "$state"; systemctl restart aisathome-llama  # PartOf restarts the agent too
fi
WD
chmod 0755 /usr/local/libexec/aisathome-node-watchdog
cat > /etc/systemd/system/aisathome-node-watchdog.service <<UNIT
[Unit]
Description=AIs@home node: restart a hung llama-server
[Service]
Type=oneshot
ExecStart=/usr/local/libexec/aisathome-node-watchdog
UNIT
cat > /etc/systemd/system/aisathome-node-watchdog.timer <<UNIT
[Unit]
Description=AIs@home node: check llama-server health every minute
[Timer]
OnBootSec=2min
OnUnitActiveSec=1min
[Install]
WantedBy=timers.target
UNIT

# Suspend: GPU memory is usually not preserved across suspend (NVIDIA default
# PreserveVideoMemoryAllocations=0), so a llama-server running through a suspend uses lost
# memory after resume (Xid 31 MMU fault and a broken CUDA state, 2026-10-09). Stop it before
# sleeping and start it again after resume, but only if it was running.
#
# The node must be gone before the GPU sleeps. The driver's nvidia-suspend.service (and
# nvidia-hibernate) suspends the GPU through /proc/driver/nvidia/suspend and is ordered only
# before systemd-suspend.service, so it used to run in parallel with this hook. When it won
# the race, llama-server's CUDA teardown blocked in nvidia_uvm (state D, immune to SIGKILL),
# systemctl stop gave up after 80 s and the kernel's freezer aborted the suspend ("1 tasks
# refusing to freeze", RTX 4090, driver 530, 2026-10-11 00:19 and 22:23; at 00:37 llama
# stopped first and the suspend worked). Hence Before=nvidia-suspend.service etc. below.
#
# Timing: this unit runs inside the suspend job, after logind has released its delay
# inhibitors, so logind's InhibitDelayMaxSec (5 s) does not apply, and a oneshot has no
# start timeout by default. "pre" bounds itself: SIGTERM, SIGKILL after 20 s, give up after
# 30 s (a process stuck in the driver cannot be killed; suspend then fails, as before).
# TimeoutStartSec=90 is only a backstop. "pre" always exits 0, so "post" runs on resume.
cat > /usr/local/libexec/aisathome-node-sleep <<'SL'
#!/bin/sh
# pre:  stop the node and return only when none of its processes is left (bounded).
# post: start it again, if pre stopped it (a node stopped by hand stays stopped).
# AISATHOME_SLEEP_ROOT and the *_WAIT variables exist for install_test.sh only.
root="${AISATHOME_SLEEP_ROOT:-}"
mark="$root/run/aisathome-node-was-running"
units="aisathome-node.service aisathome-llama.service"
term_wait="${AISATHOME_SLEEP_TERM_WAIT:-20}" kill_wait="${AISATHOME_SLEEP_KILL_WAIT:-10}"
log() { echo "aisathome-node-sleep: $*" >&2; }
# PIDs in the node's cgroups (cgroup v2, hybrid or v1 layout) that still exist.
node_pids() {
  for u in $units; do
    for d in "" /unified /systemd; do
      cat "$root/sys/fs/cgroup$d/system.slice/$u/cgroup.procs" 2>/dev/null
    done
  done | sort -un | while read -r p; do [ -d "/proc/$p" ] && echo "$p"; done
}
# wait_gone <seconds>: succeeds once no node process is left; fails after <seconds>.
wait_gone() {
  end=$(( $(date +%s) + $1 ))
  while [ -n "$(node_pids)" ]; do
    [ "$(date +%s)" -lt "$end" ] || return 1
    sleep 0.2
  done
}
case "$1" in
  pre)
    if systemctl is-active -q aisathome-llama; then touch "$mark"; fi
    # Also when inactive: a failed stop can leave processes behind.
    systemctl stop --no-block $units
    wait_gone "$term_wait" && exit 0
    log "still running ${term_wait} s after SIGTERM, sending SIGKILL: $(node_pids | tr '\n' ' ')"
    systemctl kill --signal=KILL $units 2>/dev/null
    for p in $(node_pids); do kill -KILL "$p" 2>/dev/null; done
    wait_gone "$kill_wait" && exit 0
    log "still running after SIGKILL; suspend will probably fail (stuck in the GPU driver?):"
    ps -o pid=,stat=,wchan:40=,comm= -p "$(node_pids | paste -sd, -)" >&2
    exit 0 ;;
  post)
    if [ -e "$mark" ]; then rm -f "$mark"; systemctl start aisathome-llama aisathome-node; fi ;;
esac
SL
chmod 0755 /usr/local/libexec/aisathome-node-sleep
# Before= on units that do not exist (no NVIDIA systemd sleep integration) is harmless.
cat > /etc/systemd/system/aisathome-node-sleep.service <<UNIT
[Unit]
Description=AIs@home node: stop before suspend, restart after resume
Before=sleep.target
Before=nvidia-suspend.service nvidia-hibernate.service nvidia-suspend-then-hibernate.service
StopWhenUnneeded=yes

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/libexec/aisathome-node-sleep pre
ExecStop=/usr/local/libexec/aisathome-node-sleep post
TimeoutStartSec=90

[Install]
WantedBy=sleep.target
UNIT

cat > /etc/systemd/system/aisathome-node.service <<UNIT
[Unit]
Description=AIs@home node agent (outbound connection to the gateway)
After=network-online.target aisathome-llama.service
Wants=network-online.target
# PartOf: restarting llama-server restarts the agent too. (BindsTo stopped the agent on a
# llama restart and never started it again, 2026-10-09.) The agent's own health check
# handles an engine that is up but hung.
Requires=aisathome-llama.service
PartOf=aisathome-llama.service
# No token yet (the install's link step did not finish): the agent stays off rather than failing
# and restarting every few seconds; "aisathome-node link" writes the token and starts it.
ConditionFileNotEmpty=/etc/aisathome-node/token.env

[Service]
User=aisathome
Group=aisathome
EnvironmentFile=/etc/aisathome-node/token.env
ExecStart=/opt/aisathome-node/bin/aisathome-node -config /etc/aisathome-node/node.json
Restart=on-failure
RestartSec=5
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
$sandbox

[Install]
WantedBy=multi-user.target
UNIT

node_ui_install
systemctl daemon-reload
systemctl enable --now aisathome-node-watchdog.timer
systemctl enable aisathome-node-sleep.service
if [[ $at_boot = 1 ]]; then systemctl enable aisathome-gpu-init aisathome-llama aisathome-node; fi
started="$(date '+%Y-%m-%d %H:%M:%S')"
# The agent stays stopped until the speed test is done, so no request reaches the node during
# it (PartOf would otherwise restart a running agent along with llama-server).
systemctl stop aisathome-node 2>/dev/null || true
systemctl restart aisathome-llama
for i in $(seq 1 90); do curl -fs http://127.0.0.1:8000/health >/dev/null && break; sleep 2; done
if ! curl -fs http://127.0.0.1:8000/health >/dev/null; then
  engine_fits "$started"
  echo "llama-server did not become healthy; see: journalctl -u aisathome-llama" >&2; exit 1
fi
engine_on_gpu "$started"
speed_check
linked=1
# From here a link that does not happen leaves an installed token for another gateway aside.
token_aside=0
exec 8>&- # the writers' lock, held since read_token: released only now that no rollback can run; link and register take it themselves
if [[ $link = 1 ]] && ! link_node; then
  linked=0
fi
registered=1
if [[ $anonymous = 1 ]] && ! register_node; then
  registered=0
fi
if [[ -s /etc/aisathome-node/token.env ]]; then
  systemctl restart aisathome-node
  sleep 3
  systemctl --no-pager --lines=3 status aisathome-node | tail -3
else
  systemctl stop aisathome-llama  # no token: free the GPU until the node is linked
fi
cat <<MSG

Installed. Pause the node and free the GPU:  sudo systemctl stop aisathome-node aisathome-llama
Resume:                                      sudo systemctl start aisathome-llama aisathome-node
Logs:                                        journalctl -u aisathome-node -u aisathome-llama -f
Start at boot: $(systemctl is-enabled --quiet aisathome-node 2>/dev/null && echo yes || echo "no (re-run with --enable-at-boot, or: sudo systemctl enable aisathome-llama aisathome-node)")
Remove:                                      sudo ./uninstall.sh
MSG
node_ui_message
if [[ $anonymous = 1 && $registered = 1 ]]; then
  echo "This node runs without an account: science only, no credit. Link it to an account later: sudo aisathome-node link"
fi
if [[ $registered = 0 ]]; then
  echo "Not registered, so the node is off. Try again (it starts the node): sudo aisathome-node register -accept-terms" >&2
  echo "Or link it to your account instead: sudo aisathome-node link" >&2
  exit 1
fi
if [[ $linked = 0 ]]; then
  if [[ -s /etc/aisathome-node/token.env ]]; then
    echo "Not linked to another account: the node runs with its installed token. Try again: sudo aisathome-node link" >&2
  else
    echo "Not linked yet, so the node is off. Link it to your account (it starts the node): sudo aisathome-node link" >&2
  fi
  exit 1
fi
