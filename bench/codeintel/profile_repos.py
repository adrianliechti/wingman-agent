"""Profile both indexers on prepared, disposable snapshots (no model calls).

python3 bench/codeintel/profile_repos.py CORPUS GO_TEST_BINARY CODEGRAPH_BINARY REPORT
Runs serially, records subprocess failures and macOS peak RSS, preserves raw logs.
"""
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import time
import uuid


def run(command, env, log):
    started = time.monotonic()
    if sys.platform == "darwin":
        command = ["/usr/bin/time", "-l"] + command
    try:
        result = subprocess.run(command, env=env, capture_output=True, text=True, timeout=600)
        output = result.stdout + "\n" + result.stderr
        code = result.returncode
    except subprocess.TimeoutExpired as error:
        output = "TIMEOUT\n" + str(error)
        code = 124
    log.write_text(output)
    rss = re.search(r"(\d+)\s+maximum resident set size", output)
    return {"elapsed_ms": round(1000 * (time.monotonic() - started)), "exit_code": code,
            "peak_rss_bytes": int(rss[1]) if rss else None, "log": str(log)}


if __name__ == "__main__":
    corpus, binary, external, report = map(Path, sys.argv[1:5])
    env = dict(os.environ, CODEINTEL_CORPUS=str(corpus), CODEGRAPH_TELEMETRY="0", DO_NOT_TRACK="1", CODEGRAPH_NO_DAEMON="1")
    repos = json.loads(Path(__file__).with_name("repos.json").read_text())
    edit_files = {"gin":"context.go", "django":"django/db/models/query.py", "excalidraw":"src/renderer/renderScene.ts", "vscode":"src/vs/workbench/services/extensions/common/rpcProtocol.ts"}
    records = []
    cold_only = bool(os.environ.get("CODEINTEL_COLD_ONLY"))
    test_name = "TestRepositoryColdIndexBenchmark" if cold_only else "TestRepositoryIndexBenchmark"
    external_version = None
    if os.environ.get("CODEINTEL_PROFILE_BACKEND") != "wingman":
        external_version = subprocess.run([str(external),"--version"],env=env,capture_output=True,text=True,check=True,timeout=30).stdout.strip()
    logs = corpus / "profiles" / (time.strftime("%Y%m%dT%H%M%SZ",time.gmtime())+"-"+uuid.uuid4().hex[:8])
    logs.mkdir(parents=True)
    for name in repos:
        if len(sys.argv) > 5 and name not in sys.argv[5].split(","):
            continue
        record = {"repository":name, "commit":repos[name]["commit"]}
        record["wingman"] = run([str(binary), "-test.run", "^"+test_name+"$", "-test.v", "-test.timeout", "10m"], dict(env,CODEINTEL_REPO=name), logs/(name+("-wingman-cold.log" if cold_only else "-wingman.log")))
        metrics = corpus/(name+(".cold-index.json" if cold_only else ".index.json"))
        if record["wingman"]["exit_code"] == 0:
            record["wingman"]["metrics"] = json.loads(metrics.read_text())
        if os.environ.get("CODEINTEL_PROFILE_BACKEND") == "wingman":
            records.append(record)
            report.write_text(json.dumps({"methodology":"Wingman-only profile. "+("Exactly one cold index per process; source and shared indexes unchanged." if cold_only else "Four builds per process, including edit/restore; external implementation unchanged."),"repositories":records},indent=2)+"\n")
            print(name, "wingman",record["wingman"]["exit_code"],flush=True)
            continue
        target = corpus/"external"/name
        if not target.exists():
            shutil.copytree(corpus/name, target)
        operation = "index" if (target/".codegraph").exists() else "init"
        command = [str(external), operation]
        if operation == "init":
            command.append("--yes")
        record["external_cold"] = run(command+[str(target)],env,logs/(name+"-external.log"))
        if record["external_cold"]["exit_code"] == 0 and not cold_only:
            record["external_unchanged_sync"] = run([str(external),"sync",str(target)],env,logs/(name+"-sync.log"))
            file = target/edit_files[name]
            original, stat = file.read_bytes(), file.stat()
            try:
                file.write_bytes(original+b"\n")
                record["external_edit_sync"] = run([str(external),"sync",str(target)],env,logs/(name+"-edit.log"))
            finally:
                file.write_bytes(original)
                os.utime(file, ns=(stat.st_atime_ns,stat.st_mtime_ns))
                record["external_restore"] = run([str(external),"sync",str(target)],env,logs/(name+"-restore.log"))
        records.append(record)
        methodology = "Serial subprocesses; cold process wall time includes startup. macOS RSS covers entire process lifetime. No dependencies installed in target repositories. "
        methodology += "Exactly one index per process." if cold_only else "Wingman internally reports initial index, unchanged rebuild, and one-file newline-edit rebuild. External edit uses sync; source and timestamps restored."
        report.write_text(json.dumps({"external_version":external_version, "methodology":methodology,"repositories":records},indent=2)+"\n")
        print(name, "wingman",record["wingman"]["exit_code"], "external", record["external_cold"]["exit_code"],flush=True)
