#!/bin/sh
# Installs dsvs from its GitHub releases: neither Go nor node, CI already built the binary, and its
# checksum is checked before it takes any place.
#
#   ./install.sh                        the latest stable, dsvs, in ~/.local/bin
#   DSVS_VERSION=v1.2.0 ./install.sh    a given version
#   DSVS_DIR=/usr/local/bin ./install.sh
#
# DSVS_RELEASES points at a mirror laid out as the GitHub releases are.
set -eu

releases=${DSVS_RELEASES:-https://github.com/glimps-re/dsvs/releases}
releases=${releases%/}
dir=${DSVS_DIR:-$HOME/.local/bin}
name=dsvs

os=$(uname -s | tr '[:upper:]' '[:lower:]')
if [ "$os" != linux ]; then
    echo "dsvs only runs on Linux: its private network is made of network namespaces" >&2
    exit 1
fi
arch=$(uname -m)
case $arch in
x86_64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
esac
binary=dsvs-$os-$arch

if ! command -v curl >/dev/null; then
    echo "curl not found: install it, it is what fetches the release" >&2
    exit 1
fi

# The stable is what GitHub calls the latest release, which never is a pre-release.
version=${DSVS_VERSION:-}
if [ -z "$version" ]; then
    version=$(curl -fsSL -o /dev/null -w '%{url_effective}' "$releases/latest")
    case $version in
    */tag/*) version=${version##*/} ;;
    *) version="" ;;
    esac
fi
if [ -z "$version" ]; then
    echo "no release is published at $releases" >&2
    exit 1
fi

# The binary downloads beside its destination, not into /tmp: a tight tmpfs would refuse it, and
# the final rename stays atomic on the same filesystem.
mkdir -p "$dir"
tmp=$(mktemp "$dir/.$name.XXXXXX")
trap 'rm -f "$tmp"' EXIT
curl -fsSL -o "$tmp" "$releases/download/$version/$binary"
sum=$(curl -fsSL "$releases/download/$version/checksums.txt" |
    awk -v file="$binary" '$2 == file || $2 == "*" file { print $1 }')
if [ -z "$sum" ]; then
    echo "$version publishes no checksum for $binary: refusing it" >&2
    exit 1
fi
if ! printf '%s  %s\n' "$sum" "$tmp" | sha256sum -c --status -; then
    echo "$binary of $version does not match its published checksum: refusing it" >&2
    exit 1
fi
chmod 755 "$tmp"
mv "$tmp" "$dir/$name"
trap - EXIT

echo "$name $("$dir/$name" --version) → $dir/$name"
# The unit carries this path, and the install ends with the service running: `dsvs --install-unit` says so.
"$dir/$name" --install-unit
case ":$PATH:" in
*":$dir:"*) ;;
*) echo "note: $dir is not in the PATH" >&2 ;;
esac
