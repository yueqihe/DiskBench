# Bundled runtime tools

The FPK build can include architecture-matched Linux tools in this directory:

```text
runtime-deps/
  amd64/bin/{fio,smartctl}
  arm64/bin/{fio,smartctl}
```

`scripts/stage.js` copies the selected directory into `app/server/` when
`TARGET_ARCH=amd64` or `TARGET_ARCH=arm64` is set. The lifecycle script puts
that `bin` directory first in `PATH`, so the backend does not depend on a
package being installed globally on fnOS. Core system tools (`lsblk`,
`findmnt`, `dd`, and `fstrim`) remain provided by fnOS.

The binaries are extracted from Alpine Linux 3.22 runtime packages.
See `THIRD_PARTY.md` for package versions, licenses, and source locations.
