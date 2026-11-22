#!/usr/bin/env bash
# release-manifest.sh -- writes and checks upgradoor-manifest.txt, the one file a release's
# signature covers:
#
#   scripts/release-manifest.sh write vX.Y.Z DIR [IMAGE_DIGEST]
#       DIR/upgradoor-<os>-<arch> and DIR/upgradoor-controller-<os>-<arch> ->
#       DIR/upgradoor-manifest.txt, plus the controller image's line when IMAGE_DIGEST
#       (sha256:<hex> or <hex>, the multi-platform index) is given
#   scripts/release-manifest.sh check vX.Y.Z FILE [--binaries-only]
#       FILE is in canonical form for vX.Y.Z: both binaries of every platform, the image's line
#       (unless --binaries-only), one seq derived from the tag, sorted, nothing else
#   scripts/release-manifest.sh seq vX.Y.Z
#       prints the tag's seq
#
# One line per artifact, sorted (LC_ALL=C), every seq the same:
#   image <seq> ghcr.io/ethpandaops/upgradoor <sha256 hex of the image index>
#   upgradoor <seq> linux-amd64 <sha256 hex>
#   upgradoor <seq> linux-arm64 <sha256 hex>
#   upgradoor-controller <seq> linux-amd64 <sha256 hex>
#   upgradoor-controller <seq> linux-arm64 <sha256 hex>
# upgradoor is the host binary, upgradoor-controller the one in the image.
# seq is X*1000000 + Y*1000 + Z for vX.Y.Z; a host never installs a lower one than it has.
set -euo pipefail

MANIFEST=upgradoor-manifest.txt
PLATFORMS="linux-amd64 linux-arm64"
BINARIES="upgradoor upgradoor-controller"
LINE_RE='^(image|upgradoor|upgradoor-controller) ([1-9][0-9]*) ([a-z0-9/._-]+) ([0-9a-f]{64})$'
IMAGE=ghcr.io/ethpandaops/upgradoor
export LC_ALL=C

die() { echo "!!! $*" >&2; exit 1; }

# only exactly vX.Y.Z is a release: a pre-release (v1.2.3-rc1) would carry its final's number
seq_of() {
  if ! [[ "$1" =~ ^v(0|[1-9][0-9]{0,2})\.(0|[1-9][0-9]{0,2})\.(0|[1-9][0-9]{0,2})$ ]] || [ "$1" = v0.0.0 ]; then
    die "$1 is not a release tag: only exactly vX.Y.Z is a release (anything else is a development build)"
  fi
  echo $((BASH_REMATCH[1] * 1000000 + BASH_REMATCH[2] * 1000 + BASH_REMATCH[3]))
}

sha() { sha256sum < "$1" | cut -d' ' -f1; }

# the canonical manifest for the binaries in $2 (and the image digest $3), on stdout
render() {
  local seq=$1 dir=$2 digest=${3:-} b p
  {
    [ -z "$digest" ] || echo "image $seq $IMAGE ${digest#sha256:}"
    for b in $BINARIES; do
      for p in $PLATFORMS; do
        [ -s "$dir/$b-$p" ] || die "$dir/$b-$p is missing"
        echo "$b $seq $p $(sha "$dir/$b-$p")"
      done
    done
  } | sort
}

cmd=${1:-}
case "$cmd" in
  seq)
    seq_of "${2:?usage: release-manifest.sh seq vX.Y.Z}"
    ;;
  write)
    seq=$(seq_of "${2:?usage: release-manifest.sh write vX.Y.Z DIR [IMAGE_DIGEST]}")
    dir=${3:?usage: release-manifest.sh write vX.Y.Z DIR [IMAGE_DIGEST]}
    digest=${4:-}
    [ -z "$digest" ] || [[ "$digest" =~ ^(sha256:)?[0-9a-f]{64}$ ]] || die "$digest is not a sha256 digest"
    render "$seq" "$dir" "$digest" > "$dir/$MANIFEST.tmp"
    mv "$dir/$MANIFEST.tmp" "$dir/$MANIFEST"
    cat "$dir/$MANIFEST"
    ;;
  check)
    usage="usage: release-manifest.sh check vX.Y.Z FILE [--binaries-only]"
    seq=$(seq_of "${2:?$usage}")
    file=${3:?$usage}
    want_image=1
    case "${4:-}" in
      "") ;;
      --binaries-only) want_image= ;;
      *) die "$usage" ;;
    esac
    [ -s "$file" ] || die "$file is missing or empty"
    # canonical bytes: printable ASCII lines, each ending in \n, no \r, no blank lines
    [ "$(tail -c1 "$file" | od -An -c | tr -d ' ')" = '\n' ] || die "$file does not end in a newline"
    ! grep -q $'\r' "$file" || die "$file has carriage returns"
    ! LC_ALL=C grep -q '[^ -~]' "$file" || die "$file has bytes other than printable ASCII"
    sort -c "$file" 2>/dev/null || die "$file is not sorted"
    expected=""
    [ -z "$want_image" ] || expected="image $IMAGE"$'\n'
    for b in $BINARIES; do
      for p in $PLATFORMS; do expected+="$b $p"$'\n'; done
    done
    got=""
    while IFS= read -r line; do
      [[ "$line" =~ $LINE_RE ]] ||
        die "not a manifest line: $line"
      [ "${BASH_REMATCH[2]}" = "$seq" ] ||
        die "seq ${BASH_REMATCH[2]} is not $2's ($seq): $line"
      got+="${BASH_REMATCH[1]} ${BASH_REMATCH[3]}"$'\n'
    done < "$file"
    [ "$got" = "$expected" ] || die "$file does not list exactly: $(printf "%s" "$expected" | paste -sd, -)"
    echo "$file is a canonical manifest of $2 (seq $seq)"
    ;;
  *)
    die "usage: release-manifest.sh write|check|seq vX.Y.Z …"
    ;;
esac
