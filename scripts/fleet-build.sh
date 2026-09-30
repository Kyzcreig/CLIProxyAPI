#!/usr/bin/env bash
# fleet-build.sh - the one build path for the fleet CLIProxyAPI binary (spec
# one-cliproxyapi-lineage D5). Builds <ref> (default origin/fleet) from a clean detached
# worktree, with Go VCS stamping ON, and writes the binary OUTSIDE any checkout as
#   <out-dir>/cliproxyapi.<sha8>.staged                  (native target)
#   <out-dir>/cliproxyapi.<sha8>.<goos>-<goarch>.staged  (--target other than native)
# plus <binary>.pinned: line 1 = full sha (PINNED_COMMIT.txt contract), line 2 = the
# machine line `sha=<full> describe=<d> base=<FLEET-BASE> built=<iso> host=<h> target=<t> sha256=<bin>`.
# Refuses (exit 3) unless `go version -m` reports vcs.revision == sha and vcs.modified=false.
#
# Usage: scripts/fleet-build.sh [--check] [--ref <rev>] [--target <goos/goarch>] [--out-dir <dir>]
#   --check   print the sha that would be built and sha256(go.sum) at it; build nothing.
# Exit: 0 ok, 2 usage, 3 build/verification refused.
set -euo pipefail

ref="origin/fleet"
target=""
out_dir="${HOME}/.hermes/cliproxyapi"
check=0
while [ $# -gt 0 ]; do
  case "$1" in
    --check) check=1 ;;
    --ref) ref="${2:?--ref needs a value}"; shift ;;
    --target) target="${2:?--target needs goos/goarch}"; shift ;;
    --out-dir) out_dir="${2:?--out-dir needs a value}"; shift ;;
    -h|--help) sed -n '2,15p' "$0"; exit 0 ;;
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

wt="$(mktemp -d "${TMPDIR:-/tmp}/fleet-build.${sha8}.XXXXXX")"
cleanup() { git -C "$repo" worktree remove --force "$wt/src" >/dev/null 2>&1 || true; rm -rf "$wt"; }
trap cleanup EXIT
git -C "$repo" worktree add -q --detach "$wt/src" "$sha"

describe="$(git -C "$wt/src" describe --tags --always "$sha")"
base="$(git -C "$wt/src" show "${sha}:FLEET-BASE.txt" 2>/dev/null | head -1 | tr -s ' ' '_' || true)"
built="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

(
  cd "$wt/src"
  CGO_ENABLED="$cgo" GOOS="$goos" GOARCH="$goarch" go build -trimpath -buildvcs=true \
    -ldflags "-s -w -X main.Version=${describe} -X main.Commit=${sha} -X main.BuildDate=${built}" \
    -o "$out.tmp" ./cmd/server
)

info="$(go version -m "$out.tmp")"
rev="$(printf '%s\n' "$info" | awk '$2=="vcs.revision"{split($3,a,"=");print a[2]} $2 ~ /^vcs.revision=/{split($2,a,"=");print a[2]}' | head -1)"
mod="$(printf '%s\n' "$info" | awk '$2 ~ /^vcs.modified=/{split($2,a,"=");print a[2]}' | head -1)"
if [ "$rev" != "$sha" ] || [ "$mod" != "false" ]; then
  rm -f "$out.tmp"
  echo "fleet-build: REFUSED vcs.revision=${rev:-<none>} vcs.modified=${mod:-<none>} (want ${sha} / false)" >&2
  exit 3
fi
mv -f "$out.tmp" "$out"
bin_sha="$(shasum -a 256 "$out" | cut -d' ' -f1)"
host="$(hostname -s)"
{
  echo "$sha"
  echo "sha=${sha} describe=${describe} base=${base:-unknown} built=${built} host=${host} target=${goos}/${goarch} sha256=${bin_sha}"
} >"$out.pinned"
echo "built ${out}"
cat "$out.pinned"
