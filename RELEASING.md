# Releasing

Release distribution is handled by GoReleaser from `.github/workflows/release.yml` when a `v*` tag is pushed or the workflow is run manually.

This is a **public** repository, so release artifacts are distributed as **GitHub Release** assets (like the `bg-remover` binary) — no separate download host or object-storage bucket is required.

## Owner Prerequisites

- Generate the Capstan release GPG key.
- Commit the armored public key to `RELEASE-SIGNING-KEY.asc`. Never commit the private key.
- Replace the placeholder in `RELEASE-SIGNING-FINGERPRINT` with the generated key fingerprint. This is the only authoritative pinned fingerprint location; workflow enforcement and docs should read from this file.
- Populate GitHub Actions secrets: `GPG_PRIVATE_KEY_BASE64`, `GPG_PASSPHRASE`, and `HOMEBREW_TAP_TOKEN`.
- Confirm or create the `artisan-build/homebrew-tap` repository and grant `HOMEBREW_TAP_TOKEN` write access. (Without this token the release still succeeds — the Homebrew cask step is skipped.)

## Release

```sh
git tag v0.1.0
git push origin v0.1.0
```

GoReleaser builds `capstan` for macOS, Linux, and Windows, publishes the archives, checksum file, and detached armored signature as assets on the GitHub Release, and updates the Homebrew tap cask when `HOMEBREW_TAP_TOKEN` is present.

## Verify A Download

Import the public release key once:

```sh
gpg --import RELEASE-SIGNING-KEY.asc
```

Verify the checksum signature:

```sh
gpg --verify checksums.txt.asc checksums.txt
```

Pin the expected signing key by checking the `VALIDSIG` fingerprint against `RELEASE-SIGNING-FINGERPRINT`:

```sh
expected_fingerprint="$(tr -d '[:space:]' < RELEASE-SIGNING-FINGERPRINT)"
gpg --status-fd 1 --verify checksums.txt.asc checksums.txt 2>/dev/null | grep -F "[GNUPG:] VALIDSIG ${expected_fingerprint}"
```

Then verify the downloaded archive checksum:

```sh
sha256sum --check --ignore-missing checksums.txt
```
