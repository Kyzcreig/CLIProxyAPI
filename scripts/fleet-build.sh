#!/usr/bin/env bash
# fleet-build.sh - the one build path for the fleet CLIProxyAPI binary (spec
# one-cliproxyapi-lineage D5). Builds <ref> (default origin/fleet) from a clean detached
# checkout (throwaway shared clone), with Go VCS stamping ON, and writes the binary OUTSIDE any checkout as
#   <out-dir>/cliproxyapi.<sha8>.staged                  (native target)
#   <out-dir>/cliproxyapi.<sha8>.<goos>-<goarch>.staged  (--target other than native)
# plus <binary>.pinned: line 1 = full sha (PINNED_COMMIT.txt contract), line 2 = the
# machine line `sha=<full> describe=<d> base=<FLEET-BASE> built=<iso> host=<h> target=<t> sha256=<bin>`.
# Refuses (exit 3) unless `go version -m` reports vcs.revision == sha and vcs.modified=false.
#
# --dpx-bin-dir <dir> (spec Phase 3, DPX units): ALSO build ./cmd/dpx-mapctl from the same
# checkout, gate it the same way, and install both into <dir> content-addressed, the DPX row
# convention (docs/studio-unit.md): <dir>/cpa-<sha256(bin)[:8]> and <dir>/dpx-mapctl-<sha256(bin)[:8]>,
# each with a .pinned sidecar. Existing files are never overwritten (same name = same bytes);
# running units keep their binaries until their row is repointed.
#
# Usage: scripts/fleet-build.sh [--check] [--ref <rev>] [--target <goos/goarch>] [--out-dir <dir>]
#                              [--dpx-bin-dir <dir>]
#   --check   print the sha that would be built and sha256(go.sum) at it; build nothing.
# Exit: 0 ok, 2 usage, 3 build/verification refused.
set -euo pipefail

ref="origin/fleet"
target=""
out_dir="${HOME}/.hermes/cliproxyapi"
check=0
dpx_dir=""
while [ $# -gt 0 ]; do
  case "$1" in
    --check) check=1 ;;
    --ref) ref="${2:?--ref needs a value}"; shift ;;
    --target) target="${2:?--target needs goos/goarch}"; shift ;;
    --out-dir) out_dir="${2:?--out-dir needs a value}"; shift ;;
    --dpx-bin-dir) dpx_dir="${2:?--dpx-bin-dir needs a value}"; shift ;;
    -h|--help) sed -n '2,22p' "$0"; exit 0 ;;
    *) echo "fleet-build: unknown argument: $1" >&2; exit 2 ;;
  esac
  shift
done

repo="$(git -C "$(dirname "$0")/.." rev-parse --show-toplevel)"
sha="$(git -C "$repo" rev-parse --verify "${ref}^{commit}")" || { echo "fleet-build: cannot resolve $ref" >&2; exit 2; }
sha8="${sha:0:8}"
gosum_sha="$(git -C "$repo" show "${sha}:go.sum" | shasum -a 256 | cut -d' ' -f1)"

if [ "$check" = 1 ]; then
  echo "ref=${ref} sha=${sha} go.sum.sha256=${gosum_sha}"
  exit 0
fi

native="$(go env GOOS)/$(go env GOARCH)"
[ -n "$target" ] || target="$native"
goos="${target%/*}"; goarch="${target#*/}"
if [ "$target" = "$native" ]; then
  name="cliproxyapi.${sha8}.staged"
  cgo="$(go env CGO_ENABLED)"
else
  name="cliproxyapi.${sha8}.${goos}-${goarch}.staged"
  cgo=0
fi

mkdir -p "$out_dir"
out="$(cd "$out_dir" && pwd)/${name}"
case "$out" in "$repo"/*) echo "fleet-build: --out-dir must be outside the checkout (vcs.modified)" >&2; exit 2 ;; esac

# A throwaway shared clone, not a `git worktree`: Go writes no vcs.* stamps when building
# inside a linked worktree (measured with go1.26.1), which would fail the gate below.
wt="$(cd "$(mktemp -d "${TMPDIR:-/tmp}/fleet-build.${sha8}.XXXXXX")" && pwd -P)"
cleanup() { rm -rf "$wt"; }
trap cleanup EXIT
git clone -q --shared --no-checkout "$repo" "$wt/src"
git -C "$wt/src" checkout -q --detach "$sha"

describe="$(git -C "$wt/src" describe --tags --always "$sha")"
base="$(git -C "$wt/src" show "${sha}:FLEET-BASE.txt" 2>/dev/null | head -1 | tr -s ' ' '_' || true)"
built="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

build_one() {  # build_one <pkg> <out>: stamped build + vcs gate; removes <out> on refusal
  (
    cd "$wt/src"
    CGO_ENABLED="$cgo" GOOS="$goos" GOARCH="$goarch" go build -trimpath -buildvcs=true \
      -ldflags "-s -w -X main.Version=${describe} -X main.Commit=${sha} -X main.BuildDate=${built}" \
      -o "$2" "$1"
  )
  local info rev mod
  info="$(go version -m "$2")"
  rev="$(printf '%s\n' "$info" | awk '$2=="vcs.revision"{split($3,a,"=");print a[2]} $2 ~ /^vcs.revision=/{split($2,a,"=");print a[2]}' | head -1)"
  mod="$(printf '%s\n' "$info" | awk '$2 ~ /^vcs.modified=/{split($2,a,"=");print a[2]}' | head -1)"
  if [ "$rev" != "$sha" ] || [ "$mod" != "false" ]; then
    rm -f "$2"
    echo "fleet-build: REFUSED $1 vcs.revision=${rev:-<none>} vcs.modified=${mod:-<none>} (want ${sha} / false)" >&2
    exit 3
  fi
}

build_one ./cmd/server "$out.tmp"
if [ -n "$dpx_dir" ]; then
  build_one ./cmd/dpx-mapctl "$wt/dpx-mapctl"
fi
mv -f "$out.tmp" "$out"
bin_sha="$(shasum -a 256 "$out" | cut -d' ' -f1)"
host="$(hostname -s)"
pinned_line() {  # pinned_line <sha256>
  echo "sha=${sha} describe=${describe} base=${base:-unknown} built=${built} host=${host} target=${goos}/${goarch} sha256=$1"
}
{
  echo "$sha"
  pinned_line "$bin_sha"
} >"$out.pinned"
echo "built ${out}"
cat "$out.pinned"

if [ -n "$dpx_dir" ]; then
  mkdir -p "$dpx_dir"
  dpx_dir="$(cd "$dpx_dir" && pwd)"
  install_dpx() {  # install_dpx <src> <prefix> <sha256>: content-addressed, never overwrites
    local dst="${dpx_dir}/$2-${3:0:8}"
    if [ -e "$dst" ]; then
      [ "$(shasum -a 256 "$dst" | cut -d' ' -f1)" = "$3" ] || {
        echo "fleet-build: REFUSED ${dst} exists with different bytes" >&2; exit 3; }
    else
      cp "$1" "$dst.tmp" && chmod 755 "$dst.tmp" && mv -f "$dst.tmp" "$dst"
    fi
    { echo "$sha"; pinned_line "$3"; } >"$dst.pinned"
    echo "dpx ${dst}"
  }
  install_dpx "$out" cpa "$bin_sha"
  install_dpx "$wt/dpx-mapctl" dpx-mapctl "$(shasum -a 256 "$wt/dpx-mapctl" | cut -d' ' -f1)"
fi
