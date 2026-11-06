#!/bin/sh
# Builds the binary, and installs it when asked to.
#
#   ./build.sh                    bin/<name>, for the current machine
#   ./build.sh --install          and puts it in ~/.local/bin, where the PATH finds it
#   DSVS_DIR=... ./build.sh --install      in another directory, under the same name
#   DSVS_TARGET=... ./build.sh --install   at this exact destination — this is how `dsvs update`
#                                          replaces the running binary, and a name that is not the
#                                          branch's is refused
#   ./build.sh linux arm64        another architecture — Linux only, the namespace network stack
#                                 does not exist elsewhere
#
# The script builds, it does not test: the suite runs by hand and in CI, and an update that replayed
# it would fail on a flaky test through no fault of the binary.
#
# The name comes from the branch: `main` gives dsvs, `feat/daemon` gives dsvs-daemon.
set -eu

cd "$(dirname "$0")"
install=""
if [ "${1:-}" = "--install" ]; then
    install=1
    shift
fi
# The official install puts go in /usr/local/go/bin, often outside the PATH — and always outside the
# user service's, from which `dsvs update` runs this script.
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

# `dsvs update` sets DSVS_TARGET to the running binary, full path: that is the one replaced, and the
# new one never lands beside it. But a binary is only replaced by its own: a clone on another branch
# builds another name, and writing it there would make the other binary carry that code. The refusal
# comes before the build, and its last line is what the update reports to the user.
target=${DSVS_TARGET:-${DSVS_DIR:-$HOME/.local/bin}/$name}
if [ -n "$install" ] && [ "$(basename "$target")" != "$name" ]; then
    echo "the clone is on branch $branch, which builds $name, not $(basename "$target"): check out the branch this binary comes from, or install by hand" >&2
    exit 1
fi

# The version comes from the tag: `dsvs --version` says exactly what runs, including when it is a
# binary built between two tags on a dirty tree.
version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)

# The clone is the binary's provenance: that is where it will fetch its updates, and the only
# absolute path it embeds on purpose. A binary built outside a repository has none, like the CI's,
# and then looks to the releases.
origin=$(git rev-parse --show-toplevel 2>/dev/null || echo "")

# -trimpath: without it, the binary embeds the absolute paths of the machine that built it and its
# checksum changes from one machine to another.
CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath \
    -ldflags "-X dsvs/internal/build.Version=$version -X dsvs/internal/build.Origin=$origin" \
    -o "bin/$name" ./cmd/dsvs
ls -lh "bin/$name"

if [ -n "$install" ]; then
    mkdir -p "$(dirname "$target")"
    # install, not cp: cp fails with ETXTBSY on a running binary.
    install -m 755 "bin/$name" "$target"
    echo "installed: $target"
    # The unit carries the binary's path: it is rewritten at every install, and targets this binary.
    "$target" --install-unit
fi
