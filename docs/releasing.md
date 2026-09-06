# Releasing Belay

This document describes how to cut a release for belay, verify the artifacts, and roll back if necessary.

## Semantic Versioning Policy

Belay follows semantic versioning (semver). The public contract is `pkg/belay` — the interfaces and types third-party adapters compile against.

- **Major version** (`X.0.0`): Breaking changes to any exported symbol in `pkg/belay` (interfaces, types, functions)
- **Minor version** (`1.Y.0`): New exported functionality in `pkg/belay` without breaking existing code
- **Patch version** (`1.0.Z`): Bug fixes and improvements that do not change the public API

The `internal/` tree carries **no compatibility promise**. Changes to `internal/cli`, `internal/graph`, `internal/config`, etc. are not considered breaking changes for versioning purposes.

## Prerequisites: What Must Exist First

### 1. GitHub Deploy Token

The release workflow requires a `GITHUB_TOKEN` to publish artifacts. This is provided automatically by GitHub Actions on any repository you own.

### 2. Homebrew Tap Repository

The Homebrew tap must exist **before your first release**. A release that half-succeeds (binaries published, tap update fails) is worse than one that refuses to start.

**To create the Homebrew tap:**

```bash
# Create a new public repository on GitHub named: homebrew-belay
# Clone it locally:
git clone https://github.com/<github-handle>/homebrew-belay
cd homebrew-belay

# Create the formula directory:
mkdir -p Formula

# Create a minimal formula (this will be auto-updated by the release workflow):
cat > Formula/belay.rb << 'EOF'
class Belay < Formula
  desc "Durable, resumable orchestrator for autonomous coding agents"
  homepage "https://github.com/dhaam-ai/belay"
  url "https://github.com/dhaam-ai/belay/releases/download/v0.0.1/belay_0.0.1_darwin_amd64.tar.gz"
  sha256 "0000000000000000000000000000000000000000000000000000000000000000"

  def install
    bin.install "belay"
  end

  test do
    system "#{bin}/belay", "version"
  end
end
EOF

git add Formula/belay.rb
git commit -m "feat: initial formula"
git push origin main
```

### 3. Docker Registry Access

For Docker image publishing to GitHub Container Registry (GHCR), you need:

- A GitHub personal access token with `write:packages` permission, **or**
- Let the workflow use the `GITHUB_TOKEN` provided automatically (recommended)

The workflow is pre-configured to use `GITHUB_TOKEN`, which is sufficient.

### 4. Update GoReleaser Configuration

If you renamed the module from `github.com/dhaam-ai/belay` using `make rename-module OWNER=<handle>`:

1. Update `.goreleaser.yaml`:
   - Change `ldflags` paths (all three: Version, Commit, BuildDate)
   - Change `release.github.owner` to your new GitHub handle

2. Update `.github/workflows/release.yml`:
   - No changes required (it uses `github.repository_owner` dynamically)

3. Update any links in this file that reference `belay-dev`

## How to Cut a Release

### 1. Verify the Build is Green

Ensure main branch is in a good state:

```bash
git checkout main
git pull origin main

# Run the test suite locally to catch any issues early
make test
make lint
make build
```

### 2. Determine the Next Version

Check the current version:

```bash
git tag -l 'v*' | sort -V | tail -5
# Example output:
# v0.1.0
# v0.1.1
# v0.2.0
```

Decide on the next version based on the semantic versioning policy above.

Examples:
- Fixing a bug: `v0.2.1`
- Adding a new feature: `v0.3.0`
- Removing or breaking an API: `v1.0.0`

### 3. Create the Release Tag

Tag the current commit with the version:

```bash
VERSION="v0.3.0"

# Create and push the tag
git tag -a "$VERSION" -m "Release $VERSION"
git push origin "$VERSION"
```

### 4. Watch the Release Workflow

The workflow is triggered automatically when you push a semver tag:

```bash
# Monitor the workflow in GitHub:
# https://github.com/dhaam-ai/belay/actions/workflows/release.yml
```

The workflow runs in this order:

1. **test-and-verify** (gate): Runs tests and linting; **blocks release if it fails**
2. **release**: Builds binaries and publishes to GitHub Releases
3. **homebrew**: Updates the Homebrew tap
4. **docker**: Builds and publishes multi-arch Docker images

**If test-and-verify fails**, the release is canceled and no artifacts are published. Fix the issue, push a new commit, and tag again.

### 5. Verify the Release Artifacts

Once the workflow completes, verify the artifacts:

```bash
# Check GitHub Releases
# https://github.com/dhaam-ai/belay/releases/tag/v0.3.0

# Should contain:
# - belay_0.3.0_darwin_amd64.tar.gz
# - belay_0.3.0_darwin_arm64.tar.gz
# - belay_0.3.0_linux_amd64.tar.gz
# - belay_0.3.0_linux_arm64.tar.gz
# - checksums.txt

# Verify the checksums:
cd /tmp
curl -L https://github.com/dhaam-ai/belay/releases/download/v0.3.0/checksums.txt | head -10
curl -L https://github.com/dhaam-ai/belay/releases/download/v0.3.0/belay_0.3.0_linux_amd64.tar.gz | sha256sum

# Check Homebrew formula was updated
curl https://raw.githubusercontent.com/belay-dev/homebrew-belay/main/Formula/belay.rb | grep -A 5 "version"

# Check Docker image was published
docker pull ghcr.io/dhaam-ai/belay:v0.3.0
docker run ghcr.io/dhaam-ai/belay:v0.3.0 belay version
```

## Testing a Release Locally

Before pushing a tag to trigger the real release, test the build process locally:

```bash
# Install goreleaser if you haven't already
# https://goreleaser.com/install/

# Build binaries for all platforms (no publishing)
goreleaser release --snapshot --clean --skip=publish

# You should see output like:
# • Created checksums.txt
# • built binaries for linux/amd64, linux/arm64, darwin/amd64, darwin/arm64
# • archived belay_0.3.0_linux_amd64.tar.gz, ...

# Test a binary
./dist/belay_linux_amd64_v1/belay version
# Should print: belay version dev (commit: <hash>, built: <date>)
```

## Verifying Release Artifacts Manually

After a release, verify the artifacts are correct:

### Binary Checksums

```bash
cd /tmp
RELEASE_VERSION="v0.3.0"

# Download checksums
curl -L https://github.com/dhaam-ai/belay/releases/download/$RELEASE_VERSION/checksums.txt -o checksums.txt

# Download one binary
curl -L https://github.com/dhaam-ai/belay/releases/download/$RELEASE_VERSION/belay_${RELEASE_VERSION#v}_linux_amd64.tar.gz -o belay.tar.gz

# Verify
sha256sum -c checksums.txt | grep belay_${RELEASE_VERSION#v}_linux_amd64
# Should print: belay_0.3.0_linux_amd64.tar.gz: OK
```

### Version String

```bash
cd /tmp
tar xzf belay.tar.gz
./belay version
# Should print: belay version 0.3.0 (commit: <full-sha>, built: <timestamp>)
```

### Docker Image

```bash
# Pull the image
docker pull ghcr.io/dhaam-ai/belay:v0.3.0

# Verify the version
docker run ghcr.io/dhaam-ai/belay:v0.3.0 belay version

# List the architectures
docker manifest inspect ghcr.io/dhaam-ai/belay:v0.3.0 | jq '.manifests[].platform'
# Should show: linux/amd64, linux/arm64
```

## Platform Limitations and Unsupported Configurations

**Belay does not build on Windows.** The project uses `//go:build unix` on packages like `internal/exec`, `internal/linter`, and `internal/runner` because belay shells out to `go`, `node`, `pytest`, `golangci-lint`, and `docker` — tools not available in a minimal Windows environment.

The release workflow does not configure any Windows targets. This is intentional, not an oversight.

## Rolling Back a Bad Release

If a released version has a critical bug or security issue that makes it unsuitable for use, follow this procedure:

### 1. Assess the Damage

- How many users are affected?
- Is it a data loss issue, security issue, or just a usability problem?
- Should the release be hidden or the problem fixed in a patch?

### 2. Decide on the Remediation

**Option A: Patch release** (fastest, recommended for most cases)

```bash
# Fix the bug on main
git checkout main
# ... fix the code ...
git commit -m "fix: issue that was in v0.3.0"
git push origin main

# Create a new patch release
git tag -a v0.3.1 -m "Release v0.3.1 (patch)"
git push origin v0.3.1

# The release workflow runs automatically
```

**Option B: Hide a bad release** (for security/data issues)

If the release must be hidden immediately:

1. Go to GitHub Releases for the bad version
2. Click "Edit" → "This is a pre-release" → Save
3. Add a notice at the top: "⚠️ YANKED. Do not use. Use v0.3.1 instead."

```bash
# After publishing a fix, tag and release the new version:
git tag -a v0.3.1 -m "Release v0.3.1 (fixes security issue in v0.3.0)"
git push origin v0.3.1
```

**Option C: Revert the release commit** (only if the commit was just pushed and no one has pulled it)

```bash
# If the bad commit is the very last one and no one has based work on it:
git revert <commit-hash>
git push origin main

# Then create the release tag on the revert commit
git tag -a v0.3.1 -m "Release v0.3.1"
git push origin v0.3.1
```

### 3. Communicate

Notify users of the issue:

- Post in the project's issue tracker
- If belay is installed via Homebrew, the formula will be auto-updated with the new version
- Users running the Docker image should pull the new version

### 4. Update the Homebrew Formula

The formula is auto-updated by the release workflow when you push a new tag, so it will automatically point users to the latest version (not the yanked one).

If you manually yanked a release and need to verify:

```bash
curl https://raw.githubusercontent.com/belay-dev/homebrew-belay/main/Formula/belay.rb | grep version
# Should show the latest safe version, not the yanked version
```

## Rollback in Production

If a belay orchestration run (deployed somewhere) is using a bad version:

1. **Stop new runs** by disabling the orchestration job/schedule
2. **Update the belay version** in your deployment (Dockerfile, install script, etc.) to a known-good version
3. **Restart the orchestration** once the update is deployed
4. **Monitor** for errors

If belay is running as a Docker container:

```bash
# Update to a known-good version
docker pull ghcr.io/dhaam-ai/belay:v0.3.1

# Restart the container
docker stop <container-id>
docker run ghcr.io/dhaam-ai/belay:v0.3.1 ...
```

If belay is installed via Homebrew:

```bash
# Update automatically (includes the latest known-good version)
brew upgrade belay

# Verify the version
belay version
```

## Secrets and Configuration

### GitHub Secrets

The release workflow uses `secrets.GITHUB_TOKEN`, which is automatically provided by GitHub Actions. No additional setup is required.

If you need to publish to a private Homebrew tap or a private Docker registry:

1. Create a personal access token with the appropriate permissions
2. Add it as a secret to your repository (Settings → Secrets → New repository secret)
3. Update the workflow to use your custom secret

### .goreleaser.yaml Considerations

If you rename the module from `github.com/dhaam-ai/belay`:

1. Update `ldflags` in `.goreleaser.yaml` to match the new module path
2. Update `release.github.owner` to your GitHub handle
3. Test locally with `goreleaser release --snapshot --clean --skip=publish`

The placeholder is deliberate to prevent breaking changes after a module rename.

## Troubleshooting

### Release workflow fails at test-and-verify

The test suite must pass before any release is published. Fix the failing test:

```bash
# Pull the code and check what failed
git checkout main
git pull origin main

# Run tests locally
make test

# Fix the issue
# ... edit files ...

# Commit and push
git add .
git commit -m "fix: test failure"
git push origin main

# Try the release again
git tag -a v0.3.0 -m "Release v0.3.0"
git push origin v0.3.0
```

### Homebrew tap is missing

If you see a release published to GitHub but the Homebrew formula is not updated:

1. Verify the `homebrew-belay` repository exists and is public
2. Verify the workflow has permissions to write to it
3. Check the workflow logs for the `homebrew` job

If the tap doesn't exist, create it first (see Prerequisites above).

### Docker image fails to build

If you see `docker` job failing:

1. Check that `docker/setup-buildx-action` has the correct version
2. Verify that the `Dockerfile` exists in the repository root
3. Check the workflow logs for the exact error

If there's no `Dockerfile`, create one (see the Docker section below).

### Binaries don't have version information

If `belay version` prints `dev` instead of the tag:

1. Verify the tag matches the semver pattern: `v<major>.<minor>.<patch>`
2. Verify `.goreleaser.yaml` has the correct `ldflags` paths
3. Test locally: `goreleaser release --snapshot --clean --skip=publish VERSION=v0.3.0`

## Docker Image Configuration

The Docker image is built and published as a multi-architecture image (linux/amd64 and linux/arm64) to GitHub Container Registry (GHCR).

**Image layers and runtime dependencies:**

Belay shells out to external tools (`go`, `node`, `pytest`, `golangci-lint`, `docker`), so a scratch image containing only the binary is insufficient. The Docker image should include:

- The `belay` binary
- A shell (for command execution)
- Common build tools (Go, Node, Python) if you want to use belay with repos in those languages
- Docker-in-Docker or volume mount support if you want belay to orchestrate containerized workloads

**Recommended Dockerfile:**

```dockerfile
# Build stage
FROM golang:1.26.3 AS builder
WORKDIR /build
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o belay ./cmd/belay

# Runtime stage — image that can run belay against common languages
# This is NOT a scratch image because belay needs external tools
FROM debian:bookworm-slim

# Install runtime dependencies
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    curl \
    git \
    && rm -rf /var/lib/apt/lists/*

# Copy the binary from builder
COPY --from=builder /build/belay /usr/local/bin/belay

ENTRYPOINT ["belay"]
CMD ["--help"]
```

**What this image CAN do:**
- Run orchestration against Git repositories
- Invoke the `belay` CLI
- Execute `curl`, `git`, and other shell commands

**What this image CANNOT do (limitations):**
- Run Go tests without `golang` installed (not in this image)
- Run Node tests without Node installed
- Run Python tests without Python installed
- Run Docker commands against host Docker without volume mount

Users should:
1. Mount volumes as needed (source repos, Docker socket, etc.)
2. Extend the image with additional tools if they need language-specific support
3. Or provide their own image that includes the tools their repositories need

Example usage:

```bash
# Basic usage
docker run ghcr.io/dhaam-ai/belay:latest belay version

# With volume mount for source code
docker run -v /path/to/repo:/work ghcr.io/dhaam-ai/belay:latest belay run /work/config.yaml

# With Docker-in-Docker for containerized tests
docker run -v /var/run/docker.sock:/var/run/docker.sock ghcr.io/dhaam-ai/belay:latest belay run /work/config.yaml
```

## Monitoring After Release

After a new release is published:

1. **Monitor the release download statistics** on GitHub
2. **Watch error tracking** (if integrated) for reports from new release versions
3. **Check Homebrew** installation metrics if available
4. **Monitor Docker image pull rates** from GHCR

If you detect issues, follow the rollback procedure above.
