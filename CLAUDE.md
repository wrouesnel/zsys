# zsys-compat

Fork of ubuntu/zsys, packaged as `zsys-compat` (Provides/Conflicts/Replaces `zsys`). It drives ZFS through
the `zfs` and `zpool` commands (`internal/zfs/libzfs/cli.go`) instead of libzfs, so one package works with
Ubuntu's ZFS packages and upstream OpenZFS packages. Keep it that way: don't reintroduce cgo or libzfs.

## Remotes

- `upstream`: https://github.com/ubuntu/zsys (archived; fetch only)
- `origin`: `~/git/will/zsys.git` (local backup; push after every commit)
- `github`: https://github.com/wrouesnel/zsys (HTTPS; SSH isn't set up for GitHub on this machine)

## Packaging

- PPA: `ppa:w-rouesnel/zsys` (https://launchpad.net/~w-rouesnel/+archive/ubuntu/zsys), series noble and
  resolute. jammy isn't supported: its golang-go (1.18) is older than go.mod's `go 1.21`.
- Uploads are signed with the shared Launchpad key `2A128435A6FE8BD751AA578720959AB807096ADB`, from the
  `PACKAGE_SIGNING_KEY` / `PACKAGE_SIGNING_KEY_PASSPHRASE` secrets and the
  `PACKAGE_SIGNING_KEY_FINGERPRINT` variable on wrouesnel/zsys.
- Releasing: bump `debian/changelog`, then push a `v<version>` tag. `.github/workflows/release.yaml` uploads
  `<version>~ubuntu<release>.1` for each series and creates a code-only GitHub release.
- `.github/ci/build-package` builds the source package, then the binaries from it with `GOPROXY=off`, as
  Launchpad's offline builders do. Run it in an `ubuntu:<series>` podman container to test locally.

## Testing

- `go test ./...` uses the in-memory libzfs mock. `cli_test.go` tests the CLI adapter with fake `zfs`/`zpool`
  commands.
- `sudo go test ./... -with-system-zfs` runs the suite on file-backed pools through the CLI adapter. It
  needs root, so it runs in CI (`system-zfs-tests`) rather than locally.
- When changing how properties are read, compare the CLI adapter against go-libzfs on a live system: the
  adapter must report the same types, native properties and sources, user properties and sources, and
  children for every dataset.
