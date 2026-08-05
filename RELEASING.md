# Releasing

Release distribution is handled by GoReleaser from `.github/workflows/release.yml` when a `v*` tag is pushed or the workflow is run manually.

## Owner Prerequisites

- Generate the Capstan release GPG key.
- Commit the armored public key to `RELEASE-SIGNING-KEY.asc`. Never commit the private key.
- Replace the placeholder in `RELEASE-SIGNING-FINGERPRINT` with the generated key fingerprint. This is the only authoritative pinned fingerprint location; workflow enforcement and docs should read from this file.
- Populate GitHub Actions secrets: `GPG_PRIVATE_KEY_BASE64`, `GPG_PASSPHRASE`, `CAPSTAN_BUCKET_KEY`, `CAPSTAN_BUCKET_SECRET`, `CAPSTAN_BUCKET_NAME`, `CAPSTAN_BUCKET_ENDPOINT`, optional `CAPSTAN_BUCKET_REGION`, and `HOMEBREW_TAP_TOKEN`.
- Confirm or create the `artisan-build/homebrew-tap` repository and grant `HOMEBREW_TAP_TOKEN` write access.
- Confirm the S3-compatible artifacts bucket, endpoint, region if required, and public base URL for `cli/<version>/...` downloads.

## Release

```sh
git tag v0.1.0
git push origin v0.1.0
```

GoReleaser builds `capstan` for macOS, Linux, and Windows, uploads archives to `cli/<version>/...` in the configured bucket, updates the Homebrew tap when `HOMEBREW_TAP_TOKEN` is present, and mirrors the archives, checksum file, and detached armored signature to the GitHub release.

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
