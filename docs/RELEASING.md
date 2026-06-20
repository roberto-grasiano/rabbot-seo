# Releasing Rabbot-SEO

Releases are **fully automated from a git tag**. Pushing a `vX.Y.Z` tag runs
`.github/workflows/release.yaml`, which uses [GoReleaser](https://goreleaser.com)
(`.goreleaser.yaml`) to:

- build the **6 binaries** (linux/macOS/windows × amd64/arm64), `CGO_ENABLED=0`, static,
  and archive them (`.tar.gz` / `.zip`) with a `checksums.txt` (SHA256);
- generate a **per-archive SBOM** (SPDX JSON, via syft) and **keyless-sign**
  `checksums.txt` + the Docker image with **cosign** (Sigstore: GitHub OIDC → Fulcio →
  Rekor — no stored keys);
- create a **draft** GitHub Release with those archives;
- open PRs to the **Homebrew cask** and **Scoop bucket** repos; and
- build and push the **multi-arch Docker image** to `ghcr.io/roberto-grasiano/rabbot-seo`.

Release binaries **must be built on Go >= 1.26.4** to clear the reachable stdlib
vulnerabilities GO-2026-5039 (net/textproto) and GO-2026-5037 (crypto/x509); the
release workflow pins this toolchain.

Release artifacts are **Sigstore-signed** (keyless cosign) and ship a per-archive SBOM —
see *Verify a download* below. The binaries are **not** OS-notarized (Apple/Windows); that
is a separate concern — see *OS code-signing* below.

---

## One-time setup

You only do this once for the whole project.

### 1. Create the tap + bucket repositories

Create two **empty, public** repos under your account/org (no README needed):

- `roberto-grasiano/homebrew-rabbot-seo` — the Homebrew tap (the `homebrew-` prefix is
  required by Homebrew).
- `roberto-grasiano/scoop-rabbot-seo` — the Scoop bucket.

GoReleaser commits the generated cask/manifest into these on each release.

### 2. Create the tap token and add it as a secret

The default `GITHUB_TOKEN` can only write to *this* repo, so pushing to the tap/bucket
repos needs a separate token:

1. Create a **Personal Access Token** with write access to those two repos:
   - *Fine-grained:* repo access = the two repos above, **Contents: Read and write**,
     **Pull requests: Read and write**; or
   - *Classic:* `repo` scope.
2. In this repo: **Settings → Secrets and variables → Actions → New repository secret**,
   name it **`TAP_GITHUB_TOKEN`**, paste the token.

> If `TAP_GITHUB_TOKEN` is missing, the binaries + Docker image still publish; only the
> Homebrew/Scoop steps fail.

### 3. GitHub Container Registry (Docker)

The workflow pushes to `ghcr.io` using the built-in `GITHUB_TOKEN` (it already has
`packages: write`). After the **first** release, open
`https://github.com/users/roberto-grasiano/packages` → `rabbot-seo` → **Package settings** and
set its visibility to **Public**, and **link it to this repository** (so `docker pull`
works for everyone and the repo shows the package).

---

## Cutting a release

1. **Pick a green commit on `main`.** Make sure CI is passing (`make test`, `make lint`).
2. **Tag and push:**
   ```sh
   git checkout main && git pull
   git tag v0.1.0        # semver; use v0.1.0-rc1 for a prerelease (see below)
   git push origin v0.1.0
   ```
3. **Watch the workflow** under the repo's *Actions → release*. It takes a few minutes
   (the arm64 Docker build runs under QEMU).
4. **Publish the draft Release.** Go to *Releases*, open the new **draft**, review the
   auto-generated notes and the attached archives + `checksums.txt`, then click
   **Publish**. Publishing makes it the *latest* release — which is what `install.sh` and
   `brew`/`scoop` resolve to.
5. **Merge the tap/bucket PRs.** GoReleaser opens a PR (or pushes a commit) to
   `homebrew-rabbot-seo` and `scoop-rabbot-seo`; merge them so `brew`/`scoop`
   install the new version.
6. **(First release only)** make the `ghcr.io` package public + linked (setup step 3).

### Verify it worked

```sh
# Linux/macOS one-liner installer
curl -fsSL https://raw.githubusercontent.com/roberto-grasiano/rabbot-seo/main/install.sh | sh
# Homebrew (macOS/Linux)
brew install roberto-grasiano/rabbot-seo/rabbot && rabbot version
# Scoop (Windows)
scoop bucket add rabbot-seo https://github.com/roberto-grasiano/scoop-rabbot-seo
scoop install rabbot
# Docker
docker run --rm ghcr.io/roberto-grasiano/rabbot-seo:latest version
```

### Light up the README badges (first public release)

The README's **Release**, **Build**, and **Go Report Card** badges read from a *public*
repo, so they show "inaccessible"/"unknown" while the repo is private. After the repo is
public and `v0.1.0` is tagged:

1. Confirm the **Release** and **Build** badges populate (shields.io reads the public repo
   automatically — no edit needed).
2. Visit `https://goreportcard.com/report/github.com/roberto-grasiano/rabbot-seo` once to
   generate the **Go Report Card** grade.
3. The **Go Reference** badge links live once the module is fetched/indexed by the proxy
   (first `go get` or release).

The badge URLs in the README are already correct; nothing to change there.

---

## Local dry run (no publishing)

```sh
make snapshot          # 6 archives + checksums + per-archive SBOMs into ./dist
goreleaser check       # validate .goreleaser.yaml
```

`make snapshot` skips Docker (no daemon needed) **and** signing — keyless cosign needs an
interactive OIDC browser flow, so the sign pipe is rehearsed by the `v0.1.0-rc1` tag in CI,
not locally. The **SBOM** pipe *does* run locally, so [syft](https://github.com/anchore/syft)
must be on your `PATH`:

```sh
# one-time: install syft (the SBOM generator), the same way the repo installs its
# other tools — or grab a pinned, checksummed binary from syft's releases page.
go install github.com/anchore/syft/cmd/syft@latest
```

To also build the Docker image locally (needs a daemon), skipping only signing:

```sh
goreleaser release --snapshot --clean --skip=publish,sign
```

---

## Prereleases

Tag with a prerelease suffix, e.g. `v0.1.0-rc1`. GoReleaser marks the GitHub Release as a
prerelease, does **not** move the Docker `:latest` tag, and **skips** the Homebrew/Scoop
auto-PRs (`skip_upload: auto`) — so testers can `docker pull …:v0.1.0-rc1` or download the
archive without it becoming the default install.

## Verify a download

Every release is **keyless-signed** with [cosign](https://docs.sigstore.dev/) (Sigstore):
the release workflow mints a GitHub OIDC token, Fulcio issues a short-lived certificate
bound to **this repo's release workflow**, and the signature is recorded in the public
**Rekor** transparency log. No private keys exist to leak. The signature on `checksums.txt`
covers every archive transitively, so verifying is two steps — verify the signature, then
the hash:

```sh
# 1. Verify the signature on checksums.txt (proves it came from our release workflow)
cosign verify-blob \
  --certificate-identity 'https://github.com/roberto-grasiano/rabbot-seo/.github/workflows/release.yaml@refs/tags/v0.1.0' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  --bundle checksums.txt.sigstore.json \
  checksums.txt

# 2. Verify your downloaded archive against the now-trusted checksums.
#    Linux (GNU coreutils) — --ignore-missing checks only the files you actually have:
sha256sum --ignore-missing -c checksums.txt
#    macOS (shasum has no --ignore-missing) — check just the archive you downloaded:
grep ' rabbot-seo_0.1.0_darwin_arm64.tar.gz$' checksums.txt | shasum -a 256 -c -
```

Use the tag you downloaded in `--certificate-identity`. Tampering with either
`checksums.txt` or the archive makes the matching step fail; a wrong identity (e.g. a
fork's workflow) fails verification — that binding is the point.

The **Docker image** is signed the same way:

```sh
cosign verify \
  --certificate-identity 'https://github.com/roberto-grasiano/rabbot-seo/.github/workflows/release.yaml@refs/tags/v0.1.0' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  ghcr.io/roberto-grasiano/rabbot-seo:0.1.0
```

Each archive also ships an SBOM (`<archive>.sbom.json`, SPDX JSON) listing its dependencies
for vulnerability scanning.

## OS code-signing (Gatekeeper / SmartScreen)

Sigstore proves **provenance** (who built it, that the bytes match) — it is **not** the
same as OS code-signing. We do **not** Apple-notarize or Windows-Authenticode-sign the
binaries, so a **raw archive** download may hit a one-time prompt:

- **macOS:** right-click the binary → **Open** (or `xattr -dr com.apple.quarantine ./rabbot`).
- **Windows:** SmartScreen → **More info → Run anyway**.

Homebrew (the cask strips the macOS quarantine bit on install) and Scoop avoid this for
their users. Adding macOS notarization / Windows signing later is a config + CI-secret
change; it does not affect how releases are cut.
