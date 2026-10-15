"""Fetch pinned benchmark sources without executing repository code.

Usage: python3 bench/codeintel/prepare_repos.py /tmp/wingman-codeintel-corpus
Archives are fetched with curl; links and non-regular archive entries are omitted.
An inventory outside each source tree records the archive hash and source commit.
"""
import concurrent.futures
import hashlib
import json
from pathlib import Path, PurePosixPath
import shutil
import subprocess
import sys
import tarfile
import tempfile

from verify_repos import archive_inventory, verify_tree


def prepare(name, spec, destination):
    root = destination / name
    receipt = destination / (name + ".json")
    if root.exists():
        archive = destination / (name + ".tar.gz")
        if receipt.exists() and archive.exists() and json.loads(receipt.read_text())["commit"] == spec["commit"]:
            saved = json.loads(receipt.read_text())
            if hashlib.sha256(archive.read_bytes()).hexdigest() != saved["archive_sha256"]:
                raise ValueError("archive hash changed: " + str(archive))
            verify_tree(root, archive_inventory(archive))
            print(name + ": already prepared", flush=True)
            return
        raise ValueError("refusing to replace existing unverified directory: " + str(root))
    archive = destination / (name + ".tar.gz")
    subprocess.run([
        "curl", "-fLsS", "--retry", "2", "--max-time", "180",
        "https://codeload.github.com/" + spec["repository"] + "/tar.gz/" + spec["commit"],
        "-o", str(archive),
    ], check=True)
    skipped = []
    with tempfile.TemporaryDirectory(prefix=name + "-", dir=destination) as tmp:
        with tarfile.open(archive) as source:
            for member in source:
                parts = PurePosixPath(member.name).parts[1:]
                if not parts:
                    continue
                if ".." in parts or PurePosixPath(member.name).is_absolute():
                    raise ValueError("unsafe archive member: " + member.name)
                if member.isdir():
                    continue
                if not member.isfile():
                    skipped.append("/".join(parts))
                    continue
                output = Path(tmp).joinpath(*parts)
                output.parent.mkdir(parents=True, exist_ok=True)
                with source.extractfile(member) as reader, output.open("wb") as writer:
                    shutil.copyfileobj(reader, writer)
        Path(tmp).rename(root)
    receipt.write_text(json.dumps(dict(spec, archive_sha256=hashlib.sha256(archive.read_bytes()).hexdigest(), omitted_entries=skipped), indent=2) + "\n")
    print(name + ": " + spec["commit"], flush=True)


if __name__ == "__main__":
    destination = Path(sys.argv[1]).resolve()
    destination.mkdir(parents=True, exist_ok=True)
    repos = json.loads(Path(__file__).with_name("repos.json").read_text())
    with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
        futures = [pool.submit(prepare, name, spec, destination) for name, spec in repos.items()]
        for future in futures:
            future.result()
