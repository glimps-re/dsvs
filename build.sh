#!/bin/sh
# Builds the binary, and installs it when asked to.
#
#   ./build.sh                    bin/<name>, for the current machine
#   ./build.sh --install          and puts it in ~/.local/bin, where the PATH finds it
#   ./build.sh linux arm64        another architecture — Linux only, the namespace network stack
#                                 does not exist elsewhere
#
# The name comes from the branch: `main` gives dsvs, `feat/daemon` gives dsvs-daemon.
set -eu

cd "$(dirname "$0")"
install=""
if [ "${1:-}" = "--install" ]; then
    install=1
    shift
fi
# The official install puts go in /usr/local/go/bin, often outside the PATH.
command -v go >/dev/null || PATH="$PATH:/usr/local/go/bin"
command -v go >/dev/null || {
    echo "go not found: install it, or put it in the PATH" >&2
    exit 1
}

os=${1:-$(go env GOOS 2>/dev/null || echo linux)}
arch=${2:-$(go env GOARCH 2>/dev/null || echo amd64)}

# A detached HEAD or a directory outside a repository has no branch: it is dsvs, like main.
branch=$(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo HEAD)
suffix=$(printf '%s' "${branch##*/}" | tr -c 'A-Za-z0-9_.' '-')
name=dsvs
case $suffix in
main | master | HEAD | '') ;;
*) name="dsvs-$suffix" ;;
esac

target=$HOME/.local/bin/$name

# The version comes from the tag: `dsvs --version` says exactly what runs, including when it is a
# binary built between two tags on a dirty tree.
version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)

# -trimpath: without it, the binary embeds the absolute paths of the machine that built it and its
# checksum changes from one machine to another.
CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath \
    -ldflags "-X dsvs/internal/build.Version=$version" \
    -o "bin/$name" ./cmd/dsvs
ls -lh "bin/$name"

if [ -n "$install" ]; then
    mkdir -p "$(dirname "$target")"
    # install, not cp: cp fails with ETXTBSY on a running binary.
    install -m 755 "bin/$name" "$target"
    echo "installed: $target"
fi
