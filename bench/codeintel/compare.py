#!/usr/bin/env python3
"""Compare complete before/after tool matrices without discarding failed attempts."""
import argparse
import json
import statistics
from pathlib import Path

from report import CONFIGS, LABELS, summarize


def keys(report):
    return [(t["task"], t["configuration"], t["repeat"]) for t in report["trials"]]


def digests(report):
    result = {}
    for batch in report.get("batches", [report]):
        result.update(batch.get("fixture_sha256", {}))
    return result


def tool_bytes(trials):
    return statistics.mean(sum(e["output_bytes"] for e in t["trace"]) for t in trials)


def compare(before, after):
    if (before["model"], before["effort"]) != (after["model"], after["effort"]):
        raise ValueError("model and reasoning effort must match")
    for report in (before, after):
        trials = keys(report)
        expected = {(task, config, repeat)
                    for task in {t["task"] for t in report["trials"]}
                    for config in CONFIGS
                    for repeat in range(1, report["repetitions"] + 1)}
        if not expected or len(trials) != len(set(trials)) or set(trials) != expected:
            raise ValueError("each report must contain a complete matrix without duplicate trials")
    if set(keys(before)) != set(keys(after)):
        raise ValueError("task/configuration/repeat cells must match")
    left, right = digests(before), digests(after)
    for task in {t["task"] for t in after["trials"]}:
        if task not in left or task not in right or left[task] != right[task]:
            raise ValueError(f"fixture fingerprint missing or changed: {task}")
    lines = ["All values: before → after. Each row includes every attempt.", "",
             "| Tools | Correct | Mean calls | Mean input tokens | Mean tool output bytes |",
             "|---|---:|---:|---:|---:|"]
    for config, label in zip(CONFIGS, LABELS):
        groups = [[t for t in r["trials"] if t["configuration"] == config] for r in (before, after)]
        a, b = map(summarize, groups)
        lines.append(f"| {label} | {a['correct']}/{a['attempts']} → {b['correct']}/{b['attempts']} | {a['mean_calls']:.1f} → {b['mean_calls']:.1f} | {a['mean_input_tokens']:,.0f} → {b['mean_input_tokens']:,.0f} | {tool_bytes(groups[0]):,.0f} → {tool_bytes(groups[1]):,.0f} |")
    lines += ["", "| Task / tools | Correct | Mean calls | Mean input tokens |", "|---|---:|---:|---:|"]
    for task in sorted({t["task"] for t in after["trials"]}):
        for config, label in zip(CONFIGS, LABELS):
            groups = [[t for t in r["trials"] if t["task"] == task and t["configuration"] == config] for r in (before, after)]
            a, b = map(summarize, groups)
            lines.append(f"| {task} / {label} | {a['correct']}/{a['attempts']} → {b['correct']}/{b['attempts']} | {a['mean_calls']:.1f} → {b['mean_calls']:.1f} | {a['mean_input_tokens']:,.0f} → {b['mean_input_tokens']:,.0f} |")
    return "\n".join(lines)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("before")
    parser.add_argument("after")
    args = parser.parse_args()
    try:
        print(compare(json.loads(Path(args.before).read_text()), json.loads(Path(args.after).read_text())))
    except ValueError as error:
        parser.error(str(error))


if __name__ == "__main__":
    main()
