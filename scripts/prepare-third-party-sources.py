"""Prepare the exact sources and Alpine build recipes for bundled Linux tools.

Python 3.10+ and network access are only needed when preparing a release.
Downloads are verified against the pinned aports SHA-512 checksums; recipes
are data, never executed. The output contains third-party sources only.
"""

import concurrent.futures
import hashlib
import json
import pathlib
import tarfile
import time
import urllib.request

ROOT = pathlib.Path(__file__).resolve().parent.parent
CATALOG = ROOT / "runtime-deps/sources.json"
CACHE = ROOT / ".tools/third-party-sources"


def download(url, target, expected, algorithm):
    if target.is_file() and hashlib.new(algorithm, target.read_bytes()).hexdigest() == expected:
        return
    target.parent.mkdir(parents=True, exist_ok=True)
    partial = target.with_name(target.name + ".partial")
    for attempt in range(3):
        try:
            digest = hashlib.new(algorithm)
            with urllib.request.urlopen(url, timeout=45) as response, partial.open("wb") as output:
                while chunk := response.read(1024 * 1024):
                    digest.update(chunk)
                    output.write(chunk)
            if digest.hexdigest() != expected:
                raise ValueError("Source checksum differs: " + target.name)
            partial.replace(target)
            return
        except Exception:
            partial.unlink(missing_ok=True)
            if attempt == 2:
                raise
            time.sleep(1)


def prepare():
    catalog = json.loads(CATALOG.read_text(encoding="utf-8"))
    tasks = []
    for source in catalog["sources"]:
        folder = CACHE / source["origin"]
        tasks.append((source["recipeURL"], folder / "APKBUILD", source["recipeSHA256"], "sha256"))
        for entry in source["files"]:
            assert pathlib.PurePosixPath(entry["name"]).name == entry["name"]
            tasks.append((entry["url"], folder / entry["name"], entry["sha512"], "sha512"))
    with concurrent.futures.ThreadPoolExecutor(max_workers=6) as workers:
        futures = {workers.submit(download, *task): task for task in tasks}
        for future in concurrent.futures.as_completed(futures):
            future.result()
            print("Verified " + futures[future][1].relative_to(CACHE).as_posix(), flush=True)
    readme = CACHE / "README.txt"
    readme.write_text(
        "Disk Bench - sources for bundled third-party Linux tools\n\n"
        "Distribute this archive alongside the FPK downloads, with equally accessible download links.\n"
        "It contains upstream source archives, Alpine patches and the exact aports APKBUILD recipes.\n"
        "sources.json identifies revisions, download locations and checksums for both architectures.\n"
        "These sources are unmodified by Disk Bench. Only the distributed launch wrappers and library paths differ.\n"
        "The wrappers are included under diskbench-wrappers/. No private project source or user data is included.\n\n"
        "To rebuild, unpack a component's source archive and apply its patches as specified by APKBUILD,\n"
        "or use Alpine v3.22 abuild (abuild -r) with that recipe and its listed build dependencies.\n"
        "The upstream build and license files remain inside each original source archive.\n"
        "fio and smartctl are separate programs, invoked as subprocesses by Disk Bench.\n"
        "Shared libraries remain separate files and can be replaced by compatible user builds.\n",
        encoding="utf-8",
    )
    version = next(line.split("=", 1)[1] for line in (ROOT / "package/diskbench/manifest").read_text(encoding="utf-8").splitlines() if line.startswith("version="))
    destination = ROOT / "dist" / ("diskbench-third-party-sources-" + version + ".tar.gz")
    destination.parent.mkdir(parents=True, exist_ok=True)
    with tarfile.open(destination, "w:gz", compresslevel=1) as archive:
        for _, target, _, _ in tasks:
            archive.add(target, arcname=target.relative_to(CACHE).as_posix(), recursive=False)
        archive.add(CATALOG, arcname="sources.json", recursive=False)
        archive.add(readme, arcname="README.txt", recursive=False)
        for arch in ("amd64", "arm64"):
            for tool in ("fio", "smartctl"):
                archive.add(ROOT / "runtime-deps" / arch / "bin" / tool, arcname="diskbench-wrappers/" + arch + "/" + tool, recursive=False)
    print(json.dumps({"archive": str(destination), "files": len(tasks), "bytes": destination.stat().st_size,
                      "sha256": hashlib.sha256(destination.read_bytes()).hexdigest()}), flush=True)


if __name__ == "__main__":
    prepare()
