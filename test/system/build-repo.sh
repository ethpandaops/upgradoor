#!/bin/sh
# Runs inside the image build. Builds the dummy packages with dpkg-deb and
# publishes them in two local flat apt repositories:
#
#   /opt/upgradoor-repo/main      label "Upgradoor-Test"
#   /opt/upgradoor-repo/security  label "Debian-Security", suite
#                                 "stable-security": a fake security origin
#
# Usage: build-repo.sh stage1   repositories as apt first sees them
#        build-repo.sh stage2   adds upgradoor-late 2.0 afterwards, so that
#                               apt's lists are stale until "apt-get update"
set -eu

REPO=/opt/upgradoor-repo
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

# mkdeb <repo> <name> <version> [preinst-script-body]
mkdeb() {
    repo=$1 name=$2 version=$3 preinst=${4:-}
    root="$WORK/$name-$version"

    mkdir -p "$root/DEBIAN" "$root/usr/share/upgradoor-test" "$root/etc/upgradoor-test"
    printf '%s\n' "$version" > "$root/usr/share/upgradoor-test/$name.version"
    # A conffile whose content differs between versions: upgrading over a
    # locally modified copy makes dpkg ask, unless told what to do.
    printf '# %s %s\nsetting=packaged-%s\n' "$name" "$version" "$version" \
        > "$root/etc/upgradoor-test/$name.conf"
    printf '/etc/upgradoor-test/%s.conf\n' "$name" > "$root/DEBIAN/conffiles"

    cat > "$root/DEBIAN/control" <<EOF
Package: $name
Version: $version
Architecture: all
Maintainer: upgradoor system test <nobody@example.invalid>
Section: misc
Priority: optional
Description: dummy package of the upgradoor system test
 Installs a version file and a conffile, nothing else.
EOF

    if [ -n "$preinst" ]; then
        printf '#!/bin/sh\n%s\n' "$preinst" > "$root/DEBIAN/preinst"
        chmod 0755 "$root/DEBIAN/preinst"
    fi

    mkdir -p "$REPO/$repo"
    dpkg-deb --root-owner-group --build "$root" "$REPO/$repo/${name}_${version}_all.deb" >/dev/null
}

# index <repo> <origin> <label> <suite> <codename> <version>
index() {
    repo=$1
    (
        cd "$REPO/$repo"
        rm -f Packages Release
        apt-ftparchive packages . > Packages
        apt-ftparchive \
            -o "APT::FTPArchive::Release::Origin=$2" \
            -o "APT::FTPArchive::Release::Label=$3" \
            -o "APT::FTPArchive::Release::Suite=$4" \
            -o "APT::FTPArchive::Release::Codename=$5" \
            -o "APT::FTPArchive::Release::Version=$6" \
            release . > "$WORK/Release"
        mv "$WORK/Release" Release
    )
}

index_all() {
    index main "Upgradoor Test" "Upgradoor-Test" upgradoor upgradoor 1
    index security Debian Debian-Security stable-security trixie-security 13
}

case "${1:-}" in
stage1)
    # Blocked in the test, upgradable 1.0 -> 2.0.
    mkdeb main upgradoor-dummy 1.0
    mkdeb main upgradoor-dummy 2.0
    # Blocked; its 2.0 is published in stage2.
    mkdeb main upgradoor-late 1.0
    # Blocked; the upgrade fails in preinst.
    mkdeb main upgradoor-broken 1.0
    mkdeb main upgradoor-broken 2.0 'echo "upgradoor-broken: preinst refuses the upgrade" >&2; exit 1'
    # Not blocked, upgradable: the agent must refuse it.
    mkdeb main upgradoor-plain 1.0
    mkdeb main upgradoor-plain 2.0
    # Not blocked, upgradable from the fake security origin.
    mkdeb main upgradoor-secfix 1.0
    mkdeb security upgradoor-secfix 1.0+deb13u1
    index_all
    ;;
stage2)
    mkdeb main upgradoor-late 2.0
    index_all
    ;;
*)
    echo "usage: $0 stage1|stage2" >&2
    exit 2
    ;;
esac
