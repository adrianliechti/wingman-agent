"""Report factual correctness separately from JSON-format compliance.

Usage: python3 bench/codeintel/repository_report.py RAW_REPORT [SUMMARY_JSON]
The original strict scores and full transcripts remain unchanged. Exactly one
JSON object is extracted; contradictory/multiple objects are never cherry-picked.
"""
from collections import Counter, defaultdict
import hashlib
import gzip
import json
from pathlib import Path
import statistics
import sys

CONFIGURATIONS = ("search_read", "graph", "graph_lsp", "graph_missing_lsp", "external_graph")


class DuplicateAnswerKey(ValueError):
    pass


def extract_answer(text):
    def unique_keys(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise DuplicateAnswerKey("duplicate answer key")
            result[key] = value
        return result

    decoder = json.JSONDecoder(object_pairs_hook=unique_keys)
    answers = []
    pos = 0
    while pos < len(text):
        start = text.find("{", pos)
        if start < 0:
            break
        try:
            answer, length = decoder.raw_decode(text[start:])
        except DuplicateAnswerKey:
            return None
        except ValueError:
            pos = start + 1
            continue
        pos = start + length
        if isinstance(answer, dict):
            answers.append(answer)
    return answers[0] if len(answers) == 1 else None


def score_facts(text, expected):
    answer = extract_answer(text)
    if answer is None:
        return False, 0
    # bool is a subclass of int in Python; do not credit 1 for true or 0 for false.
    passed = sum(key in answer and type(answer[key]) is type(value) and answer[key] == value for key, value in expected.items())
    # Additional fields and prose are format concerns, scored by the original
    # strict check. This metric grades only the explicitly requested facts.
    return passed == len(expected), passed


def summarize(report):
    task_file = Path(__file__).with_name("repo_tasks.json")
    if hashlib.sha256(task_file.read_bytes()).hexdigest() != report["task_sha256"]:
        raise ValueError("task manifest differs from the measured run")
    tasks = json.loads(task_file.read_text())
    expected = {(model, repo, config, repeat)
                for model in report.get("models", report["prompt_sha256"])
                for repo in report["repositories"]
                for config in report.get("configurations", CONFIGURATIONS)
                for repeat in range(1, report["repetitions"] + 1)}
    seen = set()
    groups = defaultdict(list)
    scored = []
    for session in report["sessions"]:
        key = (session["model"],session["repository"],session["configuration"],session["repeat"])
        if key in seen:
            raise ValueError("duplicate session: " + str(key))
        if key not in expected:
            raise ValueError("unexpected session: " + str(key))
        seen.add(key)
        expected_tasks = tasks[session["repository"]]
        turns = session["turns"]
        if [t["task"] for t in turns] != [t["name"] for t in expected_tasks]:
            raise ValueError("missing or reordered turn: " + str(key))
        row = {"model":key[0],"repository":key[1],"configuration":key[2],"repeat":key[3],"answers":len(turns),"correct":0,"strict_correct":0,"facts_correct":0,"facts_total":0,
               "input_tokens":sum(t["input_tokens"] for t in turns),"output_tokens":sum(t["output_tokens"] for t in turns),"tool_calls":sum(t["tool_calls"] for t in turns),"tool_errors":sum(t["tool_errors"] for t in turns),
               "tool_output_bytes":sum(e["output_bytes"] for t in turns for e in (t.get("trace") or [])),"elapsed_ms":sum(t["elapsed_ms"] for t in turns),"last_input_tokens":turns[-1].get("last_input_tokens",0),"setup_failed":bool(session.get("failure")),"failures":[]}
        tools = Counter()
        for turn in turns:
            tools.update(turn.get("tools") or {})
        row["tools"] = dict(tools)
        for turn, task in zip(turns,expected_tasks):
            correct, passed = score_facts(turn["final"],task["answer"])
            # Transport/agent failures remain failures even if partial text exists.
            failure = turn.get("failure","")
            infrastructure = failure and not failure.startswith(("invalid answer JSON:","trailing answer data","facts "))
            correct = correct and not infrastructure and not session.get("failure")
            if infrastructure or session.get("failure"):
                passed = 0
            row["correct"] += int(correct)
            row["strict_correct"] += int(turn["correct"])
            row["facts_correct"] += passed
            row["facts_total"] += len(task["answer"])
            if not correct:
                row["failures"].append({"task":task["name"],"failure":failure,"final":turn["final"]})
        scored.append(row)
        groups[(row["model"],row["configuration"])].append(row)
    aggregates = []
    for (model,configuration), rows in sorted(groups.items()):
        aggregate = {"model":model,"configuration":configuration,"sessions":len(rows)}
        tools, usage = Counter(), Counter()
        for row in rows:
            tools.update(row["tools"])
            usage.update(name for name, count in row["tools"].items() if count)
        aggregate["tool_calls_by_name"] = dict(tools)
        aggregate["sessions_using_tool"] = dict(usage)
        for key in ("answers","correct","strict_correct","facts_correct","facts_total","tool_errors","setup_failed"):
            aggregate[key] = sum(r[key] for r in rows)
        for key in ("input_tokens","output_tokens","tool_calls","tool_output_bytes","last_input_tokens"):
            aggregate["mean_"+key] = round(statistics.mean(r[key] for r in rows),1)
        aggregate["median_seconds"] = round(statistics.median(r["elapsed_ms"] for r in rows)/1000,2)
        aggregates.append(aggregate)
    coverage = {"complete":seen == expected,"completed_sessions":len(seen),"expected_sessions":len(expected),"missing_sessions":[list(key) for key in sorted(expected-seen)]}
    return {"methodology":"Re-score unchanged raw answers with exactly one JSON object, allowing surrounding prose and additional fields. Required facts must all be correct; extra fields/prose are not graded for factual truth. Original strict JSON compliance is reported separately. Duplicate keys and multiple objects are rejected. All model/setup failures retained as incorrect. Token/call/context averages are per three-question session. Exact source fact checks are not a broad architecture-answer rubric.","coverage":coverage,"sessions":scored,"aggregates":aggregates}


if __name__ == "__main__":
    source = Path(sys.argv[1])
    raw = gzip.decompress(source.read_bytes()) if source.suffix == ".gz" else source.read_bytes()
    result = summarize(json.loads(raw))
    if len(sys.argv)>2:
        Path(sys.argv[2]).write_text(json.dumps(result,indent=2)+"\n")
    coverage = result["coverage"]
    print(f'{"Complete" if coverage["complete"] else "INCOMPLETE"}: {coverage["completed_sessions"]}/{coverage["expected_sessions"]} sessions.\n')
    print("| Model | Tools | Required facts correct | Strict | Calls/session | Input/session | Final context | Median seconds |")
    print("|---|---|---:|---:|---:|---:|---:|---:|")
    for row in result["aggregates"]:
        print(f'| {row["model"]} | {row["configuration"]} | {row["correct"]}/{row["answers"]} | {row["strict_correct"]}/{row["answers"]} | {row["mean_tool_calls"]} | {row["mean_input_tokens"]:,.0f} | {row["mean_last_input_tokens"]:,.0f} | {row["median_seconds"]} |')
    if not coverage["complete"]:
        sys.exit(2)
