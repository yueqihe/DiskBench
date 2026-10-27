"""Audit both release FPKs against an explicit file list and local sources.

Run after building both architectures. This only reads packages and writes a
local audit report; it never starts the app or performs disk tests.
"""

import hashlib
import io
import json
import pathlib
import re
import tarfile

ROOT = pathlib.Path(__file__).resolve().parent.parent
PACKAGE = ROOT / "package/diskbench"
VERSION = next(line.split("=", 1)[1] for line in (PACKAGE / "manifest").read_text(encoding="utf-8").splitlines() if line.startswith("version="))
OUTER_FILES = {
    "app.tgz", "LICENSE", "ICON.PNG", "ICON_256.PNG", "manifest",
    "cmd/main", "cmd/config_callback", "cmd/config_init",
    "cmd/install_callback", "cmd/install_init", "cmd/uninstall_callback",
    "cmd/uninstall_init", "cmd/upgrade_callback", "cmd/upgrade_init",
    "config/privilege", "config/resource", "wizard/uninstall",
}
LICENSE_FILES = {
    "fio-COPYING.txt", "fio-MORAL-LICENSE.txt", "go-LICENSE.txt",
    "gcc-COPYING.txt", "gcc-COPYING3.txt", "gcc-COPYING.LIB.txt",
    "gcc-COPYING3.LIB.txt", "gcc-COPYING.RUNTIME.txt", "libaio-COPYING.txt",
    "lucide-react-LICENSE.txt", "musl-COPYRIGHT.txt", "numactl-LICENSE.GPL2.txt",
    "numactl-LICENSE.LGPL2.1.txt", "react-LICENSE.txt", "react-dom-LICENSE.txt",
    "scheduler-LICENSE.txt", "smartmontools-AUTHORS.txt",
    "smartmontools-COPYING.txt", "zlib-LICENSE.txt",
}
SENSITIVE_PATTERNS = {
    "private-key": rb"-----BEGIN (?:RSA |EC |OPENSSH |DSA )?PRIVATE KEY-----",
    "github-token": rb"(?:gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{60,})",
    "local-workspace-path": rb"[Ee]:[\\/]feiniutest[\\/]FNsmrt",
    "windows-user-path": rb"[Cc]:[\\/]Users[\\/]Yue[\\/]",
}


def require(condition, message):
    if not condition:
        raise ValueError(message)


def archive_files(blob):
    result = {}
    with tarfile.open(fileobj=io.BytesIO(blob), mode="r:*") as archive:
        for member in archive:
            name = member.name.removeprefix("./").rstrip("/")
            path = pathlib.PurePosixPath(name)
            require(not path.is_absolute() and ".." not in path.parts, "Unsafe archive path")
            require(member.isfile() or member.isdir(), "Unexpected archive member: " + name)
            if member.isfile():
                require(name not in result, "Duplicate archive member: " + name)
                result[name] = archive.extractfile(member).read()
    return result


def fields(text):
    return dict(tuple(part.strip() for part in line.split("=", 1)) for line in text.decode("utf-8").splitlines() if "=" in line)


def elf_sections(data):
    offset = int.from_bytes(data[40:48], "little")
    entry_size = int.from_bytes(data[58:60], "little")
    count = int.from_bytes(data[60:62], "little")
    names_index = int.from_bytes(data[62:64], "little")
    names_entry = offset + entry_size * names_index
    names_offset = int.from_bytes(data[names_entry + 24:names_entry + 32], "little")
    names_size = int.from_bytes(data[names_entry + 32:names_entry + 40], "little")
    names = data[names_offset:names_offset + names_size]
    return [names[int.from_bytes(data[offset + i * entry_size:offset + i * entry_size + 4], "little"):].split(b"\0", 1)[0] for i in range(count)]


def audit_fpk(arch, label, platform, machine):
    fpk = ROOT / f"diskbench-{label}-{VERSION}.fpk"
    outer = archive_files(fpk.read_bytes())
    require(outer.keys() == OUTER_FILES, "Unexpected outer files: " + str(set(outer) ^ OUTER_FILES))
    manifest = fields(outer["manifest"])
    require(manifest["version"] == VERSION and manifest["platform"] == platform, "Wrong version/platform")
    require(manifest["maintainer"] == manifest["distributor"] == "情深深雨蒙蒙", "Wrong developer")
    require(manifest["maintainer_url"] == manifest["distributor_url"] == "https://github.com/yueqihe", "Wrong developer URL")
    require(hashlib.md5(outer["app.tgz"]).hexdigest() == manifest["checksum"], "App checksum mismatch")
    for name, data in outer.items():
        if name not in {"manifest", "app.tgz"}:
            require(data == (PACKAGE / name).read_bytes(), "Stale outer file: " + name)

    payload = archive_files(outer["app.tgz"])
    expected = {}
    for prefix, folder in [("server/public/", ROOT / "frontend/dist"), ("ui/", PACKAGE / "app/ui")]:
        for file in folder.rglob("*"):
            if file.is_file():
                relative = file.relative_to(folder).as_posix()
                require(not file.name.startswith(".") and not relative.endswith((".map", ".test.js")), "Development file in static assets")
                expected[prefix + relative] = file.read_bytes()
    library_names = [
        f"ld-musl-{'x86_64' if arch == 'amd64' else 'aarch64'}.so.1",
        f"libc.musl-{'x86_64' if arch == 'amd64' else 'aarch64'}.so.1",
        "libaio.so.1", "libgcc_s.so.1", "libnuma.so.1", "libstdc++.so.6", "libz.so.1",
    ]
    runtime = ROOT / "runtime-deps" / arch
    for relative in ["bin/fio", "bin/fio.bin", "bin/smartctl", "bin/smartctl.bin"] + ["lib/" + name for name in library_names]:
        expected["server/" + relative] = (runtime / relative).read_bytes()
    for name in LICENSE_FILES:
        expected["server/licenses/" + name] = (ROOT / "runtime-deps/licenses" / name).read_bytes()
    expected["server/THIRD_PARTY_NOTICES.txt"] = (ROOT / "runtime-deps/THIRD_PARTY.md").read_bytes()
    expected["server/third-party-sources.json"] = (ROOT / "runtime-deps/sources.json").read_bytes()
    for name in ("config/resource", "config/privilege"):
        expected[name] = (PACKAGE / name).read_bytes()
    require(payload.keys() == expected.keys() | {"server/diskbench"}, "Unexpected payload files: " + str(set(payload) ^ (expected.keys() | {"server/diskbench"})))
    for name, data in expected.items():
        require(payload[name] == data, "Stale payload file: " + name)
    elf_files = []
    for name, data in payload.items():
        if data.startswith(b"\x7fELF"):
            require(data[4:6] == b"\x02\x01" and int.from_bytes(data[18:20], "little") == machine, "Wrong ELF architecture: " + name)
            elf_files.append(name)
    require(len(elf_files) == 10, "Missing runtime ELF files")
    binary = payload["server/diskbench"]
    require(b"-trimpath=true" in binary, "Non-trimmed Go build")
    require(not any(name.startswith(b".debug_") or name == b".symtab" for name in elf_sections(binary)), "Go debugging symbols were not stripped")
    require(b"vcs.revision=" not in binary and b"vcs.modified=" not in binary, "Private Git build metadata")
    for name, data in {**outer, **payload}.items():
        if name == "app.tgz":
            continue
        for kind, pattern in SENSITIVE_PATTERNS.items():
            require(not re.search(pattern, data), f"Content audit: {kind} in {name}")
    require(b"LOCAL_DEV=0 MOCK_DEV=0" in outer["cmd/main"], "Development mode not explicitly disabled")
    require(b'<script src="/app/diskbench/theme-init.js"></script>' in payload["server/public/index.html"], "Theme script is not compatible with production CSP")
    return {
        "file": fpk.name, "version": VERSION, "architecture": arch, "bytes": fpk.stat().st_size,
        "sha256": hashlib.sha256(fpk.read_bytes()).hexdigest(), "outerFileCount": len(outer),
        "payloadFileCount": len(payload), "elfFileCount": len(elf_files),
        "licenseFileCount": len(LICENSE_FILES), "checks": "PASS",
    }


def audit_sources():
    source = ROOT / f"dist/diskbench-third-party-sources-{VERSION}.tar.gz"
    catalog_data = (ROOT / "runtime-deps/sources.json").read_bytes()
    catalog = json.loads(catalog_data)
    expected = {"sources.json": ("sha256", hashlib.sha256(catalog_data).hexdigest()), "README.txt": None}
    for component in catalog["sources"]:
        origin = component["origin"] + "/"
        expected[origin + "APKBUILD"] = ("sha256", component["recipeSHA256"])
        for item in component["files"]:
            expected[origin + item["name"]] = ("sha512", item["sha512"])
    for arch in ("amd64", "arm64"):
        for tool in ("fio", "smartctl"):
            data = (ROOT / "runtime-deps" / arch / "bin" / tool).read_bytes()
            expected[f"diskbench-wrappers/{arch}/{tool}"] = ("sha256", hashlib.sha256(data).hexdigest())
    seen = set()
    with tarfile.open(source, "r:gz") as archive:
        for member in archive:
            require(member.isfile() and member.name in expected and member.name not in seen, "Unexpected source archive member")
            seen.add(member.name)
            check = expected[member.name]
            if check:
                algorithm, digest = check
                require(hashlib.new(algorithm, archive.extractfile(member).read()).hexdigest() == digest, "Source archive checksum: " + member.name)
    require(seen == expected.keys(), "Incomplete source archive")
    return {"file": source.name, "bytes": source.stat().st_size, "sha256": hashlib.sha256(source.read_bytes()).hexdigest(), "files": len(seen), "checks": "PASS"}


def main():
    report = {
        "packages": [audit_fpk("amd64", "x86_64", "x86", 62), audit_fpk("arm64", "arm64", "arm", 183)],
        "thirdPartySources": audit_sources(),
        "scope": "Exact file allowlist, current static assets/runtime/licenses, manifest, checksums, ELF architectures, build privacy flags, credential patterns and source archive hashes. No hardware I/O performed.",
    }
    destination = ROOT / ".tools" / f"release-{VERSION}-content-audit.json"
    destination.parent.mkdir(parents=True, exist_ok=True)
    destination.write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(json.dumps(report, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
