#!/usr/bin/env bash
# verify-release.sh -- checks a downloaded upgradoor release; anyone can run it, CI does before it
# tags the release's image:
#
#   scripts/verify-release.sh vX.Y.Z DIR [--min-seq N] [--require-all]
#
# DIR holds upgradoor-manifest.txt, its signature upgradoor-manifest.txt.sig and any of the
# release's binaries (upgradoor-linux-<arch> for hosts, upgradoor-controller-linux-<arch> for
# the image). Checks, in this order:
# 1. the signature: ssh-keygen -Y verify against deploy/allowed_signers, principal and
#    namespace upgradoor-release (a restodian signature, namespace restodian-client, fails);
# 2. the manifest is canonical for vX.Y.Z: every binary, the image's line, the tag's seq;
# 3. --min-seq N: the release's seq is not below N (the highest a host ever installed);
# 4. every binary present in DIR matches its line; with --require-all every binary of the
#    manifest must be present.
# Prints the image reference to pull, ethpandaops/upgradoor@sha256:<digest>.
#
# ALLOWED_SIGNERS=<file> checks against another allowed_signers file (tests, self-builders).
set -euo pipefail

usage="usage: verify-release.sh vX.Y.Z DIR [--min-seq N] [--require-all]"
VERSION=${1:?$usage}
DIR=${2:?$usage}
shift 2
MIN_SEQ=0
REQUIRE_ALL=
while [ $# -gt 0 ]; do
  case "$1" in
    --min-seq) MIN_SEQ=${2:?$usage}; shift 2 ;;
    --require-all) REQUIRE_ALL=1; shift ;;
    *) echo "$usage" >&2; exit 1 ;;
  esac
done
HERE=$(cd "$(dirname "$0")/.." && pwd)
SIGNERS=${ALLOWED_SIGNERS:-$HERE/deploy/allowed_signers}
MANIFEST=$DIR/upgradoor-manifest.txt

die() { echo "!!! $*" >&2; exit 1; }

[ -s "$MANIFEST" ] || die "$MANIFEST is missing"
[ -s "$MANIFEST.sig" ] || die "$MANIFEST.sig is missing: the release is not signed"

ssh-keygen -Y verify -f "$SIGNERS" -I upgradoor-release -n upgradoor-release \
  -s "$MANIFEST.sig" < "$MANIFEST" >/dev/null 2>&1 ||
  die "the signature over $MANIFEST does not verify against $SIGNERS (namespace upgradoor-release)"
echo "signature by upgradoor-release verified against $SIGNERS"

"$HERE/scripts/release-manifest.sh" check "$VERSION" "$MANIFEST"

seq=$("$HERE/scripts/release-manifest.sh" seq "$VERSION")
[[ "$MIN_SEQ" =~ ^[0-9]+$ ]] || die "--min-seq $MIN_SEQ is not a number"
[ "$seq" -ge "$MIN_SEQ" ] || die "$VERSION (seq $seq) is older than seq $MIN_SEQ: refusing a downgrade"

while read -r artifact _ platform sum; do
  [ "$artifact" != image ] || continue
  bin=$DIR/$artifact-$platform
  if [ ! -e "$bin" ]; then
    [ -z "$REQUIRE_ALL" ] || die "$bin is missing (--require-all)"
    continue
  fi
  [ "$(sha256sum < "$bin" | cut -d' ' -f1)" = "$sum" ] || die "$bin does not match its manifest line"
  echo "$bin matches the signed manifest"
done < "$MANIFEST"

awk '$1 == "image" { print "image: " $3 "@sha256:" $4 }' "$MANIFEST"
