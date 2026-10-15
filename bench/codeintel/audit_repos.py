"""Audit selected direct-call edges against manually inspected pinned source.

This is a small diagnostic suite, NOT whole-repository precision/recall.
Missing source/target definitions are coverage failures, not correct negatives.
python3 bench/codeintel/audit_repos.py CORPUS CODEGRAPH_BINARY REPORT
"""
import json
import os
from pathlib import Path
import subprocess
import sys


def location(node):
    return node["filePath"] + ":" + str(node["startLine"])


def external_definition(binary, root, source, name, env, cache):
    key = (source,name)
    if key not in cache:
        try:
            result = subprocess.run([str(binary),"callees",name,"--file",source.rsplit(":",1)[0],"--path",str(root),"--json","--limit","500"],env=env,capture_output=True,text=True,timeout=60)
            data = json.loads(result.stdout)
            definitions = [d for d in data.get("definitions",[]) if location(d["definition"])==source and d["definition"]["name"]==name]
            cache[key] = {"definitions":definitions,"raw":data,"error":result.stderr if result.returncode else ""}
        except (ValueError,subprocess.TimeoutExpired) as error:
            cache[key] = {"definitions":[],"error":str(error)}
    return cache[key]


if __name__ == "__main__":
    corpus, external, output = map(Path, sys.argv[1:])
    cases = json.loads(Path(__file__).with_name("repo_edges.json").read_text())
    records = []
    env = dict(os.environ, CODEGRAPH_TELEMETRY="0", DO_NOT_TRACK="1", CODEGRAPH_NO_DAEMON="1")
    for repo, edges in cases.items():
        snapshot = json.loads((corpus/(repo+".graph.json")).read_text())
        nodes = snapshot["nodes"]
        by_location = {(n["file"]+":"+str(n["start_line"]),n["name"]):i for i,n in enumerate(nodes)}
        observed = {(e["f"],e["t"]) for e in snapshot["edges"] if e["k"] == "calls"}
        cache = {}
        for source, name, target, target_name, expected in edges:
            a, b = by_location.get((source,name)), by_location.get((target,target_name))
            row = {"repository":repo,"source":source,"name":name,"target":target,"target_name":target_name,"expected":expected}
            covered = a is not None and b is not None
            row["wingman"] = {"definitions_present":covered,"edge":(a,b) in observed,"correct":covered and ((a,b) in observed)==expected}
            response = external_definition(external,corpus/"external"/repo,source,name,env,cache)
            target_response = external_definition(external,corpus/"external"/repo,target,target_name,env,cache)
            definitions = response["definitions"]
            found = any(location(c)==target and c["name"]==target_name for d in definitions for c in d.get("callees",[]))
            complete = bool(definitions) and bool(target_response["definitions"]) and not response["error"] and not target_response["error"] and not response.get("raw",{}).get("truncated",False) and not any(d.get("truncated",False) for d in definitions)
            row["external"] = {"source_present":bool(definitions),"target_present":bool(target_response["definitions"]),"complete":complete,"edge":found,"correct":complete and found==expected,"error":response["error"] or target_response["error"]}
            records.append(row)
        (corpus/(repo+".external-edge-audit.json")).write_text(json.dumps(list(cache.values()),indent=2)+"\n")
    output.write_text(json.dumps({"methodology":"Selected direct calls and decoys inspected in pinned source. Both tools must expose source and target definitions; truncated external results are not credited. Counts diagnose these cases, not whole-repository graph precision. External raw responses retained beside corpus.","cases":records},indent=2)+"\n")
    for repo in cases:
        rows = [r for r in records if r["repository"]==repo]
        print(repo, {tool:sum(r[tool]["correct"] for r in rows) for tool in ("wingman","external")},"/",len(rows))
