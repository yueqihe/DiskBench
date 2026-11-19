# Disk Bench third-party notices

The bundled Linux tools are extracted from Alpine Linux 3.22 packages for
`x86_64` and `aarch64`:

- fio 3.39-r0 (GPL-2.0-only)
- smartmontools 7.5-r0 (GPL-2.0-or-later)
- musl 1.2.5-r13 (MIT and incorporated notices in musl-COPYRIGHT.txt)
- libaio 0.3.113-r2 (LGPL-2.1-or-later)
- numactl 2.0.18-r0 (LGPL-2.1-only)
- zlib 1.3.2-r1 (Zlib; includes Alpine CVE-2026-85091 patch)
- libgcc/libstdc++ 14.2.0-r6 (GNU licenses with GCC Runtime Library Exception;
  see gcc-COPYING*.txt for the applicable terms and exception)

Additional components included in the compiled application:

- React 19.2.8 and React DOM 19.2.8 (MIT, Meta Platforms, Inc. and affiliates)
- Scheduler 0.27.0 (MIT, Meta Platforms, Inc. and affiliates)
- Lucide React 0.542.0 (ISC, with portions derived from Feather under MIT)
- @trimjs/web-app 0.6.0 (official fnOS web app SDK for host theme integration;
  the published npm package does not include a license declaration or license text)
- Go standard library and runtime (BSD-3-Clause, The Go Authors;
  compiler version recorded in the binary build information)

License texts and original copyright notices are provided in server/licenses/.
The Disk Bench MIT license applies to the application's own code, not to these
separate third-party components. fio and smartctl run as separate subprocesses;
their shared libraries remain separate, replaceable files. The original Alpine
executables are not modified; launcher scripts select the bundled musl loader
and library directory. Compatible user-built shared libraries may replace them.

The release includes a companion archive named
diskbench-third-party-sources-<version>.tar.gz. Distribute it alongside the FPK
downloads with equally accessible links. It contains the corresponding upstream
source archives, Alpine patches, exact APKBUILD recipes and launcher scripts.
It does not contain Disk Bench's private project source or user data.

third-party-sources.json identifies the exact Alpine commits, package versions,
source URLs, SHA-512 source checksums and SHA-256 package checksums for x86_64
and aarch64. Source checksums are checked against those APKBUILD recipes.
Alpine package repositories: https://dl-cdn.alpinelinux.org/alpine/v3.22/
Alpine build recipes: https://github.com/alpinelinux/aports

Build-only dependencies such as Vite and source-map-js are not installed in the
FPK. The package contains the built static frontend, not a Node.js runtime or
development server. Full notices above also cover the frontend's bundled code.
