#!/usr/bin/env bash
# Add deb and rpm packages to the apt and yum repositories in a local tree
# that mirrors the packages.xscalerlabs.com bucket:
#
#   apt/dists, apt/pool          apt repository (reprepro), public
#   rpm/<channel>/Packages       rpms, public
#   rpm/<channel>/repodata       yum metadata (createrepo_c), public
#   rpm/xscaler.repo             yum repo definition, public
#   gpg.key                      ASCII-armored signing key, public
#   xscaler-archive-keyring.gpg  binary signing key for apt signed-by, public
#   state/reprepro/db            reprepro's database, never served
#
#   GPG_KEY_ID=<fpr> scripts/publish-repo.sh <stable|testing> <package-dir> <root>
#
# The tree does not need the packages already published. reprepro builds the
# apt indexes from its database, and createrepo_c reuses the existing yum
# metadata (--recycle-pkglist --skip-stat), so a release only has to pull
# state/ and rpm/<channel>/repodata, and push what changed.
#
# reprepro (5.3) keeps one version per package, architecture and channel, so
# the apt index lists the newest release; older .debs stay in the pool. The
# yum metadata lists every version.
#
# A package already published at the same version is skipped, so rerunning a
# release does not fail and does not replace published files.
set -euo pipefail

channel=$1
pkgdir=$(cd "$2" && pwd)
root=$3
: "${GPG_KEY_ID:?set GPG_KEY_ID to the signing key fingerprint}"
base_url=${BASE_URL:-https://packages.xscalerlabs.com}

case "$channel" in
  stable | testing) ;;
  *) echo "channel must be stable or testing, got: $channel" >&2; exit 1 ;;
esac

mkdir -p "$root"
root=$(cd "$root" && pwd)
reprepro_dir=$root/state/reprepro

# --- signing key, published next to the repositories ---------------------
gpg --batch --yes --armor --export "$GPG_KEY_ID" >"$root/gpg.key"
gpg --batch --yes --export "$GPG_KEY_ID" >"$root/xscaler-archive-keyring.gpg"

# --- apt -------------------------------------------------------------------
mkdir -p "$reprepro_dir/conf" "$root/apt"
dist() {
  cat <<EOF
Origin: xScaler
Label: xScaler
Suite: $1
Codename: $1
Architectures: amd64 arm64
Components: main
Description: xScaler packages, $2
SignWith: $GPG_KEY_ID

EOF
}
{
  dist stable "stable releases"
  dist testing "release candidates"
} >"$reprepro_dir/conf/distributions"
cat >"$reprepro_dir/conf/options" <<EOF
outdir $root/apt
EOF

repre() { reprepro --basedir "$reprepro_dir" --keepunreferencedfiles "$@"; }

shopt -s nullglob
for deb in "$pkgdir"/*.deb; do
  name=$(dpkg-deb -f "$deb" Package)
  version=$(dpkg-deb -f "$deb" Version)
  arch=$(dpkg-deb -f "$deb" Architecture)
  # shellcheck disable=SC2016 # reprepro's own ${field} format
  have=$(repre -A "$arch" --list-format '${version}\n' list "$channel" "$name")
  if [ "$have" = "$version" ]; then
    echo "apt: $name $version $arch already in $channel, skipping"
    continue
  fi
  echo "apt: adding $name $version $arch to $channel"
  repre includedeb "$channel" "$deb"
done
# Re-export so a run that only skipped still leaves signed indexes behind.
repre export "$channel"

# --- yum -------------------------------------------------------------------
rpmdir=$root/rpm/$channel
mkdir -p "$rpmdir/Packages"

published() {
  local repomd=$rpmdir/repodata/repomd.xml primary
  [ -f "$repomd" ] || return 1
  primary=$(sed -n 's/.*<location href="\(repodata\/[^"]*primary\.xml\.gz\)".*/\1/p' "$repomd")
  zcat "$rpmdir/$primary" | grep -q "<location href=\"Packages/$1\""
}

include=()
for rpm in "$pkgdir"/*.rpm; do
  f=$(basename "$rpm")
  if published "$f"; then
    echo "yum: $f already in $channel, skipping"
    continue
  fi
  echo "yum: adding $f to $channel"
  cp "$rpm" "$rpmdir/Packages/$f"
  include+=(--includepkg "Packages/$f")
done

# gz metadata: Amazon Linux 2's yum cannot read zstd.
if [ -f "$rpmdir/repodata/repomd.xml" ]; then
  if [ "${#include[@]}" -gt 0 ]; then
    createrepo_c --quiet --general-compress-type gz \
      --update --skip-stat --recycle-pkglist "${include[@]}" "$rpmdir"
  fi
else
  createrepo_c --quiet --general-compress-type gz "$rpmdir"
fi
gpg --batch --yes --local-user "$GPG_KEY_ID" --detach-sign --armor \
  --output "$rpmdir/repodata/repomd.xml.asc" "$rpmdir/repodata/repomd.xml"

cat >"$root/rpm/xscaler.repo" <<EOF
[xscaler]
name=xScaler
baseurl=$base_url/rpm/stable
enabled=1
gpgcheck=1
repo_gpgcheck=1
gpgkey=$base_url/gpg.key

[xscaler-testing]
name=xScaler (release candidates)
baseurl=$base_url/rpm/testing
enabled=0
gpgcheck=1
repo_gpgcheck=1
gpgkey=$base_url/gpg.key
EOF

echo "published to $channel under $root"
