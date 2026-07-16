# Self-hosted Brave sync server

_On a hiatus..._ waiting for the Android Brave browser to gain the feature of
setting a custom sync server URL, without which continuing is pretty pointless.

A simplified version of the [Brave sync server](https://github.com/brave/go-sync),
made more suitable for self-hosting by replacing the AWS Dynamo and Redis
dependencies with SQLite3 and a local memory cache.

```
brave-browser --sync-url=http://localhost:8295/litesync
```

---

## Publishing a Release

Releases are built and published automatically by the
[GitHub Actions workflow](.github/workflows/release.yml) when a version tag is
pushed. The workflow produces statically-linked binaries for `linux/amd64` and
`linux/arm64`, a `checksums.txt` file, and a GitHub Release with auto-generated
release notes.

### Steps

1. **Ensure the `main` branch is in a releasable state** — all tests pass, changes
   are committed and pushed.

   ```bash
   git checkout main
   git pull
   ```

2. **Choose a version number** following [Semantic Versioning](https://semver.org/)
   (`vMAJOR.MINOR.PATCH`):

   | Change type                       | Example  |
   | --------------------------------- | -------- |
   | Bug fix / patch                   | `v1.0.1` |
   | New feature, backwards-compatible | `v1.1.0` |
   | Breaking change                   | `v2.0.0` |

3. **Create and push an annotated tag:**

   ```bash
   VERSION="v1.0.0"

   git tag -a "${VERSION}" -m "Release ${VERSION}"
   git push origin "${VERSION}"
   ```

4. **Monitor the workflow** on the
   [Actions tab](https://github.com/mikaelhg/litesync/actions) — the
   `Release` job will build both binaries, verify checksums, and create the
   GitHub Release automatically.

5. **Verify the release** on the
   [Releases page](https://github.com/mikaelhg/litesync/releases) — confirm
   that `litesync-linux-amd64`, `litesync-linux-arm64`, and `checksums.txt`
   are all attached.

### If something goes wrong

To delete a tag and re-run (e.g. the build failed before assets were uploaded):

```bash
# Delete the tag locally and remotely
git tag -d "${VERSION}"
git push origin --delete "${VERSION}"

# Fix the issue, then re-tag and push
git tag -a "${VERSION}" -m "Release ${VERSION}"
git push origin "${VERSION}"
```

> **Note:** GitHub does not allow re-uploading assets to an existing release.
> Delete the draft/failed release on the Releases page before re-pushing the tag.

---

## Deployment

See [DEPLOY.md](DEPLOY.md) for full instructions on deploying litesync to an
Ubuntu server using the pre-built release binaries.
