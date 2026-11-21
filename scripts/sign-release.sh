#!/usr/bin/env bash
# sign-release.sh -- the maintainer's step of a release, run on the machine the release key
# (a FIDO security key) is plugged into; never in CI:
#
#   scripts/sign-release.sh vX.Y.Z [--no-rebuild] [--no-image-check]
#
# 1. downloads the draft release's upgradoor-manifest.txt and binaries, upgradoor-linux-<arch>
#    (hosts) and upgradoor-controller-linux-<arch> (the image) (gh release download);
# 2. checks the manifest: canonical form, every binary and the image, the seq of vX.Y.Z (a wrong
#    one would freeze every host at that number, or never reach them); every binary matches;
# 3. checks the image the manifest names: exactly linux/amd64 and linux/arm64, each holding
#    the manifest's upgradoor-controller as /usr/local/bin/upgradoor-controller
#    (--no-image-check skips this: then CI alone vouches for the image);
# 4. rebuilds the binaries in a fresh worktree of the tag (make release) and demands identical
#    lines (--no-rebuild skips this: then CI alone vouches for the build);
# 5. prints the manifest and its hash, signs it with one touch of the key (ssh-keygen -Y sign,
#    namespace upgradoor-release): the key shows only its application name, so what was printed
#    here is what you approve;
# 6. checks the signature against deploy/allowed_signers and uploads upgradoor-manifest.txt.sig.
# Publishing the release then verifies the signature in CI and tags the signed image.
#
# RELEASE_KEY      the key handle of your security key (default ~/.ssh/upgradoor-release; create
#                  one with scripts/enroll-signer.sh)
# ALLOWED_SIGNERS  the file the signature must verify against (default deploy/allowed_signers)
# For tests, without GitHub or the tag:
# UPGRADOOR_RELEASE_DIR  a local directory stands in for the draft: assets are read from it and
#                        the signature is written into it
# UPGRADOOR_RELEASE_REF  what to rebuild (default refs/tags/vX.Y.Z)
# UPGRADOOR_IMAGE_PULL   the repository to pull the signed digest from (default
#                        ghcr.io/ethpandaops/upgradoor; the Docker Hub mirror holds the same digest)
set -euo pipefail

usage="usage: sign-release.sh vX.Y.Z [--no-rebuild] [--no-image-check]"
VERSION=${1:?$usage}
shift
REBUILD=1
IMAGE_CHECK=1
for arg in "$@"; do
  case "$arg" in
    --no-rebuild) REBUILD= ;;
    --no-image-check) IMAGE_CHECK= ;;
    *) echo "$usage" >&2; exit 1 ;;
  esac
done
KEY=${RELEASE_KEY:-$HOME/.ssh/upgradoor-release}
HERE=$(cd "$(dirname "$0")/.." && pwd)
SIGNERS=${ALLOWED_SIGNERS:-$HERE/deploy/allowed_signers}
LOCAL=${UPGRADOOR_RELEASE_DIR:-}
REF=${UPGRADOOR_RELEASE_REF:-refs/tags/$VERSION}
PULL=${UPGRADOOR_IMAGE_PULL:-ghcr.io/ethpandaops/upgradoor}
MANIFEST=upgradoor-manifest.txt
CHECK=$HERE/scripts/release-manifest.sh

die() { echo "!!! $*" >&2; exit 1; }
sha() { sha256sum < "$1" | cut -d' ' -f1; }

"$CHECK" seq "$VERSION" >/dev/null
[ -r "$KEY" ] || die "release key $KEY not found (RELEASE_KEY=…)"
[ -n "$LOCAL" ] || command -v gh >/dev/null || die "gh (GitHub CLI) is required"
[ -z "$IMAGE_CHECK" ] || { command -v docker >/dev/null && command -v jq >/dev/null; } ||
  die "docker and jq are required to check the image (--no-image-check skips it)"

mkdir -p "$HERE/.cache"
work=$(mktemp -d "$HERE/.cache/sign.XXXXXX")
cleanup() {
  [ ! -d "$work/src" ] || git -C "$HERE" worktree remove --force "$work/src" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT
cd "$work"

mkdir dl
if [ -n "$LOCAL" ]; then
  cp "$LOCAL/$MANIFEST" "$LOCAL"/upgradoor-linux-* "$LOCAL"/upgradoor-controller-linux-* dl/
else
  gh release download "$VERSION" --dir dl --pattern "$MANIFEST" --pattern 'upgradoor-linux-*' \
    --pattern 'upgradoor-controller-linux-*'
fi
[ -s "dl/$MANIFEST" ] || die "no $MANIFEST in release $VERSION (did CI build it?)"

"$CHECK" check "$VERSION" "dl/$MANIFEST" >/dev/null || die "the release manifest is not signable"

while read -r artifact _ platform sum; do
  [ "$artifact" != image ] || continue
  bin=$artifact-$platform
  [ -s "dl/$bin" ] || die "$bin is missing from the release"
  [ "$(sha "dl/$bin")" = "$sum" ] || die "$bin does not match its manifest line"
done < "dl/$MANIFEST"
echo "every binary of the draft matches the manifest"

digest=$(awk '$1 == "image" { print $4 }' "dl/$MANIFEST")
if [ -n "$IMAGE_CHECK" ]; then
  ref="$PULL@sha256:$digest"
  # the index fetched by its digest: its platforms, each with its own manifest digest (pulled by
  # that, so the platforms do not overwrite each other's reference in the local image store)
  docker buildx imagetools inspect --raw "$ref" > index.json
  platforms=$(jq -r '[.manifests[] | .platform.os + "/" + .platform.architecture] | sort | join(" ")' index.json)
  [ "$platforms" = "linux/amd64 linux/arm64" ] || die "$ref is for $platforms, not linux/amd64 linux/arm64"
  for arch in amd64 arm64; do
    m=$(jq -r --arg a "$arch" '.manifests[] | select(.platform.architecture == $a) | .digest' index.json)
    cid=$(docker create --quiet "$PULL@$m")
    docker cp --quiet "$cid:/usr/local/bin/upgradoor-controller" "image-$arch" || {
      docker rm "$cid" >/dev/null
      die "no upgradoor-controller in $ref for $arch"
    }
    docker rm "$cid" >/dev/null
    cmp -s "image-$arch" "dl/upgradoor-controller-linux-$arch" ||
      die "$ref for $arch holds another upgradoor-controller than the manifest's"
  done
  echo "the image holds the manifest's upgradoor-controller binaries"
else
  echo "note: the image was not checked (--no-image-check): you sign the digest CI pushed" >&2
fi

if [ -n "$REBUILD" ]; then
  git -C "$HERE" fetch --tags --quiet || true
  git -C "$HERE" worktree add --detach --quiet "$work/src" "$REF"
  make -s -C "$work/src" release VERSION="$VERSION" >/dev/null
  grep -v '^image ' "dl/$MANIFEST" | cmp -s - "$work/src/dist/$MANIFEST" || {
    grep -v '^image ' "dl/$MANIFEST" | diff - "$work/src/dist/$MANIFEST" >&2 || true
    die "a local rebuild of $VERSION gives other binaries: do not sign"
  }
  echo "a local rebuild of $REF reproduces every binary"
else
  echo "note: not rebuilt locally (--no-rebuild): you sign what CI built" >&2
fi

cp "dl/$MANIFEST" "$MANIFEST"
echo
cat "$MANIFEST"
echo
echo "sha256 of $MANIFEST: $(sha "$MANIFEST")"
echo "signing with $(ssh-keygen -lf "$KEY.pub" 2>/dev/null || echo "$KEY"): touch the security key when it blinks"
ssh-keygen -Y sign -f "$KEY" -n upgradoor-release "$MANIFEST"

ssh-keygen -Y verify -f "$SIGNERS" -I upgradoor-release -n upgradoor-release -s "$MANIFEST.sig" < "$MANIFEST" >/dev/null ||
  die "the signature does not verify against $SIGNERS: is the key's public half in there?"

if [ -n "$LOCAL" ]; then
  cp "$MANIFEST.sig" "$LOCAL/"
  echo "signature written to $LOCAL"
else
  gh release upload "$VERSION" "$MANIFEST.sig" --clobber
  echo "signature uploaded to release $VERSION; publish the release to tag the image"
fi
