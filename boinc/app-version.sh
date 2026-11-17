#!/usr/bin/env bash
# Lay out one BOINC app version of the pilot app, unsigned, for a project's apps/ folder:
#
#   boinc/app-version.sh --version 1 --wrapper <BOINC wrapper, x86_64 Linux> \
#     --node <aisathome-node, linux/amd64> --bundle <aisathome-node-bundle-linux-amd64.tar.xz> \
#     --out <dir> [--plan-class cuda_big] [--beta]
#
# It writes <out>/pilot/<version>/x86_64-pc-linux-gnu__<plan class>/ with the four files and
# version.xml, and lists the files to sign. Signing is offline, with the project's code-signing
# key (BOINC's sign_executable on the offline machine; a <file>.sig beside each file): this
# script never sees a key. Each file's physical name carries the first 12 hex digits of its
# SHA-256, because BOINC never lets a physical name change content, and the pilot unpacks the
# bundle into a folder named after it.
set -euo pipefail

usage() { sed -n '2,13p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }
version="" wrapper="" node="" bundle="" out="" plan=cuda_big beta=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --version) version="${2:-}"; shift 2 ;;
    --wrapper) wrapper="${2:-}"; shift 2 ;;
    --node) node="${2:-}"; shift 2 ;;
    --bundle) bundle="${2:-}"; shift 2 ;;
    --out) out="${2:-}"; shift 2 ;;
    --plan-class) plan="${2:-}"; shift 2 ;;
    --beta) beta=1; shift ;;
    *) usage ;;
  esac
done
[[ -n "$version" && -n "$wrapper" && -n "$node" && -n "$bundle" && -n "$out" ]] || usage
# BOINC's version folders: an integer, or MAJOR.MINOR with MINOR < 100.
[[ "$version" =~ ^[0-9]+(\.[0-9]{1,2})?$ ]] || { echo "--version must be N or N.M (M < 100)" >&2; exit 2; }
[[ "$plan" =~ ^[a-z0-9_]+$ ]] || { echo "--plan-class: lower-case letters, digits and _" >&2; exit 2; }
for f in "$wrapper" "$node" "$bundle"; do [[ -f "$f" ]] || { echo "no such file: $f" >&2; exit 1; }; done
[[ "$bundle" == *.tar.xz ]] || { echo "--bundle must be the node's .tar.xz bundle" >&2; exit 2; }
here="$(cd "$(dirname "$0")" && pwd)"

dir="$out/pilot/$version/x86_64-pc-linux-gnu__$plan"
[[ ! -e "$dir" ]] || { echo "$dir exists; a published app version never changes" >&2; exit 1; }
mkdir -p "$dir"

h() { sha256sum "$1" | cut -c1-12; }
# name <file> <stem> <ext>: the file's physical name.
name() { echo "$2_$(h "$1")$3"; }
w="$(name "$wrapper" wrapper "")"
n="$(name "$node" aisathome-node "")"
j="$(name "$here/job.xml" job .xml)"
b="$(name "$bundle" aisathome-node-bundle-linux-amd64 .tar.xz)"
install -m 0755 "$wrapper" "$dir/$w"
install -m 0755 "$node" "$dir/$n"
install -m 0644 "$here/job.xml" "$dir/$j"
install -m 0644 "$bundle" "$dir/$b"

{
  echo "<version>"
  echo "    <file>"
  echo "        <physical_name>$w</physical_name>"
  echo "        <main_program/>"
  echo "    </file>"
  for pair in "$n aisathome-node" "$j job.xml" "$b engine.tar.xz"; do
    read -r p l <<<"$pair"
    echo "    <file>"
    echo "        <physical_name>$p</physical_name>"
    echo "        <logical_name>$l</logical_name>"
    echo "    </file>"
  done
  echo "    <is_wrapper/>"
  echo "    <needs_network/>"
  [[ $beta = 0 ]] || echo "    <beta/>"
  echo "</version>"
} > "$dir/version.xml"

echo "$dir"
echo "sign these offline (a .sig beside each):"
printf '  %s\n' "$w" "$n" "$j" "$b"
