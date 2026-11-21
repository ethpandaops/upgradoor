#!/usr/bin/env bash
# enroll-signer.sh -- make your FIDO security key (YubiKey, Ledger FIDO app, ...) a release signer:
#
#   scripts/enroll-signer.sh "<your name>" [--add]
#
# 1. creates the key handle RELEASE_KEY (default ~/.ssh/upgradoor-release) on the security key
#    unless it exists (ssh-keygen -t ed25519-sk, application ssh:upgradoor-release; touch the key).
#    The private key never leaves the security key; the handle file alone cannot sign;
# 2. signs a test message with it (namespace upgradoor-release) and verifies the signature, so
#    the key works for scripts/sign-release.sh;
# 3. prints your allowed_signers line; --add appends it to deploy/allowed_signers.
# The line only takes effect once it is merged here and in the pinned copy of the Ansible role
# (ansible-collection-general: roles/upgradoor_agent/files/allowed_signers): hosts trust only that.
#
# RELEASE_KEY      key handle path (default ~/.ssh/upgradoor-release)
# RELEASE_KEY_TYPE ed25519-sk (default) or ecdsa-sk for keys without ed25519 support
# RELEASE_KEY_OPTS extra ssh-keygen -O options, e.g. "verify-required" to demand the PIN on signing
set -euo pipefail

usage='usage: enroll-signer.sh "<your name>" [--add]'
NAME=${1:?$usage}
shift
ADD=
for arg in "$@"; do
  case "$arg" in
    --add) ADD=1 ;;
    *) echo "$usage" >&2; exit 1 ;;
  esac
done
case "$NAME" in *[!A-Za-z0-9\ ._@-]* | "") echo "!!! name: letters, digits, space, . _ @ - only" >&2; exit 1 ;; esac

KEY=${RELEASE_KEY:-$HOME/.ssh/upgradoor-release}
TYPE=${RELEASE_KEY_TYPE:-ed25519-sk}
HERE=$(cd "$(dirname "$0")/.." && pwd)
SIGNERS=$HERE/deploy/allowed_signers
NS=upgradoor-release

die() { echo "!!! $*" >&2; exit 1; }

if [ ! -e "$KEY" ]; then
  opts=(-O "application=ssh:$NS")
  for o in ${RELEASE_KEY_OPTS:-}; do opts+=(-O "$o"); done
  echo ">>> creating $KEY on the security key: touch it when it blinks"
  ssh-keygen -t "$TYPE" "${opts[@]}" -C "$NAME" -f "$KEY"
fi
[ -r "$KEY.pub" ] || die "$KEY.pub not found"

read -r ktype kdata _ < "$KEY.pub"
case "$ktype" in sk-*) ;; *) die "$KEY is not a security-key (sk-) key: $ktype" ;; esac
line="$NS namespaces=\"$NS\" $ktype $kdata $NAME"

mkdir -p "$HERE/.cache"
work=$(mktemp -d "$HERE/.cache/enroll.XXXXXX")
trap 'rm -rf "$work"' EXIT
echo "$line" > "$work/allowed_signers"
echo "upgradoor signer test $(date -u +%FT%TZ)" > "$work/msg"
echo ">>> test signature: touch the key"
ssh-keygen -Y sign -n "$NS" -f "$KEY" "$work/msg" >/dev/null
ssh-keygen -Y verify -f "$work/allowed_signers" -I "$NS" -n "$NS" -s "$work/msg.sig" < "$work/msg" >/dev/null ||
  die "the test signature does not verify"

echo ">>> your allowed_signers line:"
echo "$line"
if [ -n "$ADD" ]; then
  if grep -qF "$ktype $kdata" "$SIGNERS"; then
    echo ">>> already in $SIGNERS"
  else
    echo "$line" >> "$SIGNERS"
    echo ">>> added to $SIGNERS"
  fi
fi
echo ">>> open pull requests adding it to deploy/allowed_signers here and to"
echo "    roles/upgradoor_agent/files/allowed_signers in ansible-collection-general"
