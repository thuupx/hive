# Releasing

A release is a tag. Pushing one runs `.github/workflows/release.yml`, which
builds every platform, packages each one, and publishes a GitHub release.

```sh
git tag -a v0.1.0 -m "v0.1.0"
git push origin v0.1.0
```

The workflow publishes the release itself. To review the assets first, add
`--draft` to the `gh release create` call in the workflow, publish by hand, and
remove it again.

## What a release contains

| Asset | What it is |
|---|---|
| `hive_<version>_<os>_<arch>.tar.gz` | `hive`, `hive-plugin-acp`, `hive-plugin-slack`, and `hive-plugin-zalo`, side by side |
| `checksums.txt` | SHA-256 of every tarball |
| `install.sh` | the install script, at the version it installs |

The plugins are in the same archive as `hive` because the daemon resolves them
from its own directory. A tarball with `hive` alone installs a gateway with no
agents.

These assets are what both `scripts/install.sh` and `hive update` consume: the
asset name carries the version, the platform, and the architecture, and
`checksums.txt` covers every tarball. `hive update` mirrors the installer's
verification and placement, so a self-update lands the same shape as a fresh
install.

The documented install command points at
`https://github.com/thuupx/hive/releases/latest/download/install.sh`, not at the
copy on a branch: a branch is mutable, so a `curl | sh` from one would not be the
script any release shipped.

## Why the builds are split

CGO cannot be cross-compiled practically, and `go-libsql` ships prebuilt native
libraries for linux and darwin on amd64 and arm64 only. A single runner therefore
cannot produce a release. Each target is built on its own native runner:

```text
ubuntu-latest      linux/amd64
ubuntu-24.04-arm   linux/arm64
macos-latest       darwin/arm64
```

The matrix is the same one `ci.yml` uses, so a platform that cannot build fails
on a pull request rather than on a tag.

**darwin/amd64 is not published.** GitHub retired the amd64 macOS runner, so
there is no native host left to build it on. `install.sh` reports this rather
than failing on a missing download; a source build (`make build`) works there.

`ubuntu-24.04-arm` needs arm64 hosted runners enabled for the repository. If the
job cannot be scheduled, that is why.

## Signing and notarization

The darwin job signs every binary with a **Developer ID Application**
certificate and submits them to Apple's notarization service, so a tarball
downloaded through a browser clears Gatekeeper instead of being quarantined.
`scripts/sign-macos.sh` imports the certificate into a temporary keychain and
signs with the hardened runtime and a timestamp; `scripts/notarize-macos.sh`
zips the binaries and waits for `notarytool`. A plain binary cannot be stapled,
so the ticket lives with each binary's cdhash — the tarball does not need one.

The job needs six repository secrets:

| Secret | What it is |
|---|---|
| `APPLE_CERTIFICATE_P12` | base64 of the "Developer ID Application" certificate + private key, exported from Keychain as a .p12 (`base64 -i cert.p12`) |
| `APPLE_CERTIFICATE_PASSWORD` | the .p12 export password |
| `APPLE_KEYCHAIN_PASSWORD` | any password; guards only the throwaway keychain the build creates |
| `APPLE_API_KEY` | base64 of an App Store Connect API .p8 private key |
| `APPLE_API_KEY_ID` | the key's id |
| `APPLE_API_ISSUER` | the issuer id, from App Store Connect → Users and Access → Integrations |

The signing certificate requires a paid Apple Developer membership. The API key
replaces an Apple-ID-plus-app-specific-password: it works the same for
`notarytool`, and it is not tied to one person's account.

## Why the checksums are made in one place

Each runner uploads only its tarball. `checksums.txt` is written by the single
release job, after every artifact is downloaded, so the file that the install
script verifies against cannot disagree with the artifacts it covers.

The install script verifies the checksum **before** the archive is opened, and
copies three named files rather than extracting whatever the archive happens to
hold.

## Testing a release without publishing one

`make dist` produces exactly what a runner uploads, for the host it runs on:

```sh
make dist VERSION=0.0.1-test
tar -tzf dist/hive_0.0.1-test_*.tar.gz   # hive, hive-plugin-acp, hive-plugin-slack, hive-plugin-zalo
./bin/hive version                        # 0.0.1-test
```

`VERSION` is stamped in with `-ldflags "-X main.Version=..."`, which is why
`Version` in `cmd/hive/main.go` is a variable and not a constant.

`install.sh` downloads from GitHub, so a fully local dry run means putting a fake
`curl` earlier on `PATH` that serves a local release directory. The checksum
verification, the extraction, and the placement are all exercised that way.

## Not done yet

- **Provenance.** There is no SBOM and no SLSA provenance, and the linux builds
  are unsigned. `checksums.txt` proves integrity against the release, not who
  made it. Adding cosign keyless signing is the next step that matters most.
- **Homebrew.** No tap yet.
- **Reproducibility.** The build is not bit-for-bit reproducible, and the
  release does not claim to be.
