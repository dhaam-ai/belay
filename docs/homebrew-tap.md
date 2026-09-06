# Setting up the Homebrew tap

A Homebrew *tap* is an ordinary GitHub repository holding formula files.
goreleaser writes belay's formula into it on each release; it does not create
the repository, so the tap must exist before the first release or that release
publishes binaries nobody can `brew install`.

This is a one-time setup.

## 1. Create the tap repository

The name matters: Homebrew derives `brew tap dhaam-ai/belay` from a repository
called **`homebrew-belay`**. The `homebrew-` prefix is stripped, so a repo named
`belay-tap` or `homebrew-belay-tap` will not resolve.

```bash
gh repo create dhaam-ai/homebrew-belay --public \
  --description "Homebrew tap for belay"
```

It must be **public**. Homebrew cannot read a private tap without credentials
that no ordinary user has.

## 2. Give the release a token that can write to it

`GITHUB_TOKEN` in a workflow is scoped to the repository the workflow runs in,
so it cannot push to the tap. Create a fine-grained personal access token with
**Contents: read and write** on `dhaam-ai/homebrew-belay` only, and add it to
the belay repository as the secret `HOMEBREW_TAP_GITHUB_TOKEN`.

```bash
gh secret set HOMEBREW_TAP_GITHUB_TOKEN --repo dhaam-ai/belay
```

Scope it to that one repository. A token that can write to every repository in
the org is a much larger blast radius than publishing a formula requires.

## 3. Release

`.goreleaser.yaml`'s `brews:` block does the rest: on a tag, goreleaser builds
the archives, computes their checksums, renders `Formula/belay.rb`, and pushes
it to the tap.

```bash
git tag -a v0.1.0 -m "belay v0.1.0"
git push origin v0.1.0
```

## 4. Verify from a user's point of view

Do this on a machine that has never built belay, or the test proves nothing —
a stale binary on your PATH will answer instead of the installed one.

```bash
brew tap dhaam-ai/belay
brew install belay
belay version          # should print the tag you just pushed
belay --help
```

## Testing a formula before you publish it

You do not need a tap, a remote, or a release to check a formula works.
Build an archive, point a formula at it with a `file://` URL, and install:

```bash
make build
mkdir -p /tmp/belay-pkg && cp bin/belay LICENSE README.md /tmp/belay-pkg/
tar -czf /tmp/belay.tar.gz -C /tmp/belay-pkg belay LICENSE README.md
shasum -a 256 /tmp/belay.tar.gz          # paste into the formula's sha256

brew install --formula ./belay.rb
brew test belay
brew uninstall belay
```

This is how the shipped formula was verified before the first release.

## Updating the formula

Do not edit `Formula/belay.rb` in the tap by hand. goreleaser overwrites it on
the next release, so a hand edit survives exactly until then and disagrees with
the release in the meantime. Change `brews:` in `.goreleaser.yaml` instead.

## If the tap update fails after binaries publish

The release is then half-done: users can download archives but `brew install`
serves the previous version. Either re-run the release workflow's homebrew job
once the cause is fixed, or push the formula manually and reconcile at the next
release. Publishing binaries and the formula in one atomic step is not
something GitHub offers, which is why the tap is created first and its token
verified before any tag is pushed.
