#!/usr/bin/env python3
"""Summarize all attempts from one or more TestModelToolMatrix JSON reports."""
import argparse
import json
import statistics
from pathlib import Path

CONFIGS = ["search_read", "graph", "lsp", "graph_lsp", "graph_missing_lsp"]
LABELS = ["Search/read", "+ code_graph", "+ LSP", "+ code_graph + LSP", "+ code_graph + unavailable LSP"]


def summarize(trials):
    return {
        "attempts": len(trials),
        "correct": sum(t["correct"] for t in trials),
        "mean_calls": statistics.mean(t["tool_calls"] for t in trials),
        "mean_input_tokens": statistics.mean(t["input_tokens"] for t in trials),
        "mean_output_tokens": statistics.mean(t["output_tokens"] for t in trials),
        "median_seconds": statistics.median(t["elapsed_ms"] for t in trials) / 1000,
        "tool_errors": sum(t["tool_errors"] for t in trials),
        "graph_used_trials": sum(t["tools"].get("code_graph", 0) > 0 for t in trials),
        "lsp_used_trials": sum(t["tools"].get("lsp", 0) > 0 for t in trials),
    }


def render(trials):
    lines = ["| Available tools | Correct | Mean calls | Mean input tokens | Mean output tokens | Median seconds | Tool errors |",
             "|---|---:|---:|---:|---:|---:|---:|"]
    for name, label in zip(CONFIGS, LABELS):
        selected = [t for t in trials if t["configuration"] == name]
        if not selected:
            continue
        row = summarize(selected)
        lines.append(f"| {label} | {row['correct']}/{row['attempts']} | {row['mean_calls']:.1f} | {row['mean_input_tokens']:,.0f} | {row['mean_output_tokens']:,.0f} | {row['median_seconds']:.1f} | {row['tool_errors']} |")
    lines.extend(["", "| Task | Search/read | + graph | + LSP | + both | + graph, LSP missing |",
                  "|---|---:|---:|---:|---:|---:|"])
    for task in sorted({t["task"] for t in trials}):
        cells = []
        for config in CONFIGS:
            subset = [t for t in trials if t["configuration"] == config and t["task"] == task]
            if subset:
                row = summarize(subset)
                cells.append(f"{row['mean_calls']:.1f} / {row['mean_input_tokens']/1000:.1f}k")
            else:
                cells.append("—")
        lines.append("| " + task + " | " + " | ".join(cells) + " |")
    lines.append("\nTask cells: mean tool calls / mean input tokens. All attempts are included.")
    return "\n".join(lines) + "\n"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("reports", nargs="+")
    parser.add_argument("--merge", help="write a combined JSON report, preserving batch metadata")
    args = parser.parse_args()
    reports = [json.loads(Path(path).read_text()) for path in args.reports]
    if len({(r["model"], r["effort"]) for r in reports}) != 1:
        parser.error("compare each model and reasoning effort separately")
    prompts = {r["prompt_sha256"] for r in reports if r.get("prompt_sha256")}
    if len(prompts) > 1:
        parser.error("cannot merge runs with different prompts")
    fixtures = {}
    for report in reports:
        for task, digest in report.get("fixture_sha256", {}).items():
            if task in fixtures and fixtures[task] != digest:
                parser.error(f"cannot merge different fixtures for {task}")
            fixtures[task] = digest
    trials = [t for r in reports for t in r["trials"]]
    keys = [(t["task"], t["configuration"], t["repeat"]) for t in trials]
    if len(keys) != len(set(keys)):
        parser.error("duplicate task/configuration/repeat; retain separate experiment reports")
    cells = {}
    for task, config, repeat in keys:
        cells.setdefault((task, config), set()).add(repeat)
    expected = set(range(1, max(r["repetitions"] for r in reports) + 1))
    for task in {t["task"] for t in trials}:
        for config in CONFIGS:
            if cells.get((task, config)) != expected:
                parser.error(f"incomplete matrix: {task}/{config}")
    trials.sort(key=lambda t: (t["repeat"], t["task"], t["configuration"]))
    if args.merge:
        merged = dict(reports[0])
        merged["batches"] = [batch for r in reports for batch in r.get("batches", [{k: v for k, v in r.items() if k not in {"trials", "summary", "batches"}}])]
        merged["trials"] = trials
        merged["summary"] = {c: summarize([t for t in trials if t["configuration"] == c]) for c in CONFIGS}
        Path(args.merge).write_text(json.dumps(merged, indent=2) + "\n")
    print(render(trials), end="")


if __name__ == "__main__":
    main()
