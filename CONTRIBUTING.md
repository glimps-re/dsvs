# Contributing to dsvs

## Build and test

You need the Go version named in `go.mod`, and Node 23.6 or later: the tests of `internal/config` and `internal/core` load a `config.dsvs.ts` by importing the TypeScript as is. dsvs is Linux only.

```sh
./build.sh                  # bin/<name>, for this machine
./build.sh --install        # and installs it in ~/.local/bin, with its systemd user unit
go vet ./...
go test ./...
```

The binary is named after the branch: `main` builds `dsvs`, `beta` builds `dsvs-beta`, `feat/x` builds `dsvs-x`. Everything that isolates a binary derives from its name, so a branch build runs next to your everyday `dsvs` without touching it.

Once per clone, enable the repository hooks:

```sh
git config core.hooksPath .githooks
```

The `pre-commit` hook regenerates the usage block of `BETA.md` from the command table, and refuses the commit if it had drifted. That block documents what this tree builds, the beta; the README describes the stable, and sends to `dsvs help`.

## Commit messages

Commits follow [Conventional Commits 1.0](https://www.conventionalcommits.org/en/v1.0.0/), in English. The type says what the change is to a user, and the subject says what it does, in the imperative and lower case:

```
feat: show the version of each binary in the panel
fix(update): keep the old binary when the new one fails to start
docs: explain the port shift
refactor(hub): pass descriptors through a single helper
feat(cli)!: replace the flags with subcommands
```

`feat` and `fix` make the release notes; `docs`, `refactor`, `test`, `perf`, `build`, `ci` and `chore` do not. A `!` after the type, or a `BREAKING CHANGE:` footer, marks a breaking change. Body lines have no length limit: editors wrap them.

A pull request checks every one of its commits with [commitlint](https://commitlint.js.org). To check yours before pushing:

```sh
npx -p @commitlint/cli -p @commitlint/config-conventional commitlint --from origin/main
```

## Branches and pull requests

Two long-lived branches, both protected, both with a linear history:

- **`main` is the stable.** Every stable release is tagged on it. A fix to the stable branches from `main` and goes back to `main`.
- **`beta` carries the next version.** New features branch from `beta` and go back to `beta`, and every beta release is tagged on it. `beta` is rebased on `main` whenever `main` moves, so the fixes of the stable reach the beta.

When a beta has proven itself, `main` fast-forwards to `beta` and the stable release is tagged there.

1. Branch from `main` for a fix, from `beta` for a feature, and keep your branch rebased on it rather than merging it in.
2. Keep commits small and about one thing each: they land as they are, and each one should build and pass the tests.
3. Open the pull request against the branch you started from. The `tests` and `commits` checks must pass, and it is merged by rebase.

## Releases

Pushing to a branch publishes nothing. A release is a signed tag that a maintainer pushes, following [Semantic Versioning 2.0](https://semver.org/spec/v2.0.0.html):

```sh
git tag -s v0.2.0 -m v0.2.0
git push origin v0.2.0
```

The tag runs the tests again, then [GoReleaser](https://goreleaser.com) builds `dsvs-linux-amd64` and `dsvs-linux-arm64`, publishes them with a `checksums.txt` and release notes grouped by commit type, and GitHub attests their build provenance. To verify a downloaded binary:

```sh
sha256sum --check --ignore-missing checksums.txt
gh attestation verify dsvs-linux-amd64 --repo glimps-re/dsvs
```

There are two channels:

- **stable**, tagged `vX.Y.Z` on `main`, is what `dsvs` installs and updates to;
- **beta**, tagged `vX.Y.Z-beta.N` on `beta` and published as a prerelease, is what `dsvs-beta` follows. It installs next to `dsvs` and reads its own `config.beta.dsvs.ts`, so the stable binary stays a working fallback.
