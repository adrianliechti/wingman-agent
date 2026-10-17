"""Verify benchmark source copies against the downloaded, pinned archives.

Usage: python3 bench/codeintel/verify_repos.py CORPUS [CANDIDATE_CORPUS ...]
Prints an inventory with a deterministic content digest. Every regular archive
entry must match byte-for-byte; extra files outside .codegraph are rejected.
Links omitted by prepare_repos.py are omitted here too. No repository code runs.
"""
import hashlib
import json
from pathlib import Path, PurePosixPath
import sys
import tarfile


def file_digest(data):
    digest = hashlib.sha256()
    for block in iter(lambda: data.read(1024 * 1024), b""):
        digest.update(block)
    return digest.hexdigest()


def archive_inventory(archive):
    inventory = {}
    with tarfile.open(archive) as source:
        for member in source:
            path = PurePosixPath(member.name)
            if path.is_absolute() or ".." in path.parts:
                raise ValueError("unsafe archive entry: " + member.name)
            if member.isfile() and len(path.parts) > 1:
                relative = str(PurePosixPath(*path.parts[1:]))
                with source.extractfile(member) as data:
                    inventory[relative] = file_digest(data)
    return inventory


def verify_tree(root, expected):
    if not root.is_dir():
        raise ValueError("missing source root: " + str(root))
    seen = set()
    for file in root.rglob("*"):
        relative = file.relative_to(root)
        if relative.parts[0] == ".codegraph":
            continue
        if file.is_symlink():
            raise ValueError("unexpected symlink: " + str(file))
        if not file.is_file():
            continue
        name = str(relative)
        with file.open("rb") as data:
            digest = file_digest(data)
        if expected.get(name) != digest:
            raise ValueError("unexpected or changed file: " + str(file))
        seen.add(name)
    if seen != set(expected):
        raise ValueError("missing files in " + str(root) + ": " + str(sorted(set(expected)-seen)))
    return hashlib.sha256(json.dumps(expected,sort_keys=True,separators=(",",":")).encode()).hexdigest()


if __name__ == "__main__":
    corpus = Path(sys.argv[1])
    repositories = json.loads(Path(__file__).with_name("repos.json").read_text())
    result = {}
    for name, spec in repositories.items():
        archive = corpus/(name+".tar.gz")
        receipt = json.loads((corpus/(name+".json")).read_text())
        with archive.open("rb") as data:
            digest = file_digest(data)
        if receipt["commit"] != spec["commit"] or receipt["archive_sha256"] != digest:
            raise ValueError("archive receipt mismatch: " + name)
        expected = archive_inventory(archive)
        copies = {}
        for directory in sys.argv[1:]:
            root = Path(directory)/name
            copies[str(root)] = verify_tree(root,expected)
            for root in (Path(directory)/"external"/name,):
                if root.exists():
                    copies[str(root)] = verify_tree(root,expected)
        result[name] = dict(commit=spec["commit"],archive_sha256=digest,files=len(expected),copies=copies)
    print(json.dumps(result,indent=2))
