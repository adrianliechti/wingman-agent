#!/usr/bin/env python3
"""Capture Codex or Claude Code requests with dummy credentials and a local mock.

The mock never forwards requests and returns only synthetic text, with no tool
calls. Captures verify request construction, not model access or task quality.
"""

import argparse
import gzip
import hashlib
import http.server
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import threading
from urllib.parse import urlsplit


def save_json(path, value):
    path.write_text(json.dumps(value, indent=2) + "\n", encoding="utf-8")


def text_blocks(value):
    if isinstance(value, str):
        return value
    return "\n\n".join(block.get("text", "") for block in value or [])


def mock_response(provider, model):
    text = "Capture complete."
    if provider == "claude":
        message = {
            "id": "msg_capture", "type": "message", "role": "assistant", "model": model,
            "content": [{"type": "text", "text": text}], "stop_reason": "end_turn",
            "stop_sequence": None, "usage": {"input_tokens": 1, "output_tokens": 2},
        }
        start = dict(message, content=[], stop_reason=None)
        events = [
            {"type": "message_start", "message": start},
            {"type": "content_block_start", "index": 0, "content_block": {"type": "text", "text": ""}},
            {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": text}},
            {"type": "content_block_stop", "index": 0},
            {"type": "message_delta", "delta": {"stop_reason": "end_turn", "stop_sequence": None}, "usage": {"output_tokens": 2}},
            {"type": "message_stop"},
        ]
        return message, events
    part = {"type": "output_text", "text": text, "annotations": []}
    item = {"id": "msg_capture", "type": "message", "role": "assistant", "status": "completed", "content": [part]}
    response = {
        "id": "resp_capture", "object": "response", "created_at": 0, "status": "completed",
        "model": model, "output": [item], "usage": {"input_tokens": 1, "output_tokens": 2, "total_tokens": 3},
    }
    events = [
        {"type": "response.created", "response": dict(response, status="in_progress", output=[])},
        {"type": "response.output_item.added", "output_index": 0, "item": dict(item, status="in_progress", content=[])},
        {"type": "response.content_part.added", "output_index": 0, "item_id": item["id"], "content_index": 0, "part": dict(part, text="")},
        {"type": "response.output_text.delta", "output_index": 0, "item_id": item["id"], "content_index": 0, "delta": text},
        {"type": "response.output_text.done", "output_index": 0, "item_id": item["id"], "content_index": 0, "text": text},
        {"type": "response.content_part.done", "output_index": 0, "item_id": item["id"], "content_index": 0, "part": part},
        {"type": "response.output_item.done", "output_index": 0, "item": item},
        {"type": "response.completed", "response": response},
    ]
    return response, events


def launch_config(provider, binary, root, endpoint, model):
    # Preserve HOME and CODEX_HOME. Explicit flags exclude user configuration.
    env = {key: os.environ[key] for key in ("HOME", "CODEX_HOME", "PATH", "USER", "TMPDIR") if key in os.environ}
    env.update({"HTTP_PROXY": endpoint, "HTTPS_PROXY": endpoint, "NO_PROXY": "127.0.0.1,localhost"})
    prompt = "Reply with the word ready. Do not use tools."
    if provider == "claude":
        env.update({
            "CLAUDE_CONFIG_DIR": str(root / "claude-config"),
            "ANTHROPIC_BASE_URL": endpoint, "ANTHROPIC_API_KEY": "local-capture-dummy-key",
            "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1", "DISABLE_AUTOUPDATER": "1",
            "DISABLE_TELEMETRY": "1", "DISABLE_ERROR_REPORTING": "1",
            "CLAUDE_CODE_DISABLE_AGENT_VIEW": "1", "CLAUDE_CODE_ATTRIBUTION_HEADER": "0",
        })
        return [
            binary, "--print", prompt, "--model", model, "--output-format", "json",
            "--safe-mode", "--setting-sources", "", "--strict-mcp-config",
            "--mcp-config", '{"mcpServers":{}}', "--no-session-persistence", "--no-chrome",
        ], env
    env["WINGMAN_CAPTURE_TOKEN"] = "local-capture-dummy-key"
    config = {
        "model_provider": "capture",
        "model_providers.capture.name": "Local capture",
        "model_providers.capture.base_url": endpoint + "/v1",
        "model_providers.capture.env_key": "WINGMAN_CAPTURE_TOKEN",
        "model_providers.capture.wire_api": "responses",
        "model_providers.capture.requires_openai_auth": False,
        "model_providers.capture.supports_websockets": False,
        "model_providers.capture.request_max_retries": 0,
        "model_providers.capture.stream_max_retries": 0,
        "cli_auth_credentials_store": "ephemeral",
        "project_doc_max_bytes": 0, "developer_instructions": "",
        "web_search": "disabled",
        "features.api_key_model_discovery": False,
        "features.enable_request_compression": False,
        "features.apps": False, "features.plugins": False, "features.remote_plugin": False,
        "features.hooks": False, "features.shell_snapshot": False,
        "features.skip_host_skill_discovery": True,
        "feedback.enabled": False, "analytics.enabled": False,
        "history.persistence": "none", "check_for_update_on_startup": False,
        "otel.exporter": "none", "otel.trace_exporter": "none", "otel.metrics_exporter": "none",
        "log_dir": str(root / "codex-logs"), "sqlite_home": str(root / "codex-state"),
    }
    command = [binary, "exec", "--ignore-user-config", "--ignore-rules", "--ephemeral", "--skip-git-repo-check", "--sandbox", "read-only", "--model", model, "--json"]
    for key, value in config.items():
        command += ["-c", key + "=" + json.dumps(value)]
    command.append(prompt)
    return command, env


def capture(provider, binary, root, model):
    workspace = root / "workspace"
    workspace.mkdir(parents=True)
    requests = []
    lock = threading.Lock()

    class Handler(http.server.BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass

        def do_CONNECT(self):
            self.send_error(502, "Offline capture: external requests disabled")

        def do_GET(self):
            self.send_error(404)

        def do_POST(self):
            route = urlsplit(self.path).path
            if route not in ("/v1/responses", "/v1/messages", "/v1/messages/count_tokens"):
                self.send_error(502, "Offline capture: unexpected endpoint")
                return
            raw = self.rfile.read(int(self.headers.get("Content-Length", 0)))
            if self.headers.get("Content-Encoding") == "gzip":
                raw = gzip.decompress(raw)
            data = json.loads(raw)
            if route.endswith("/count_tokens"):
                payload = json.dumps({"input_tokens": 1}).encode()
                content_type = "application/json"
            else:
                developer_messages = [
                    text_blocks(item.get("content")) for item in data.get("input", [])
                    if isinstance(item, dict) and item.get("role") in ("developer", "system") and item.get("content")
                ]
                developer = "\n\n".join(developer_messages)
                system = "\n\n".join(part for part in (
                    text_blocks(data.get("system", data.get("instructions", ""))), developer,
                ) if part)
                base = text_blocks(data.get("instructions", "")) or (developer_messages[0] if developer_messages else system)
                declared_tools = list(data.get("tools", []))
                for item in data.get("input", []):
                    if isinstance(item, dict) and item.get("type") == "additional_tools":
                        declared_tools.extend(item.get("tools", []))
                with lock:
                    stem = f"request-{len(requests) + 1}"
                    save_json(root / f"{stem}.json", data)
                    (root / f"{stem}.system.txt").write_text(system, encoding="utf-8")
                    (root / f"{stem}.base.txt").write_text(base, encoding="utf-8")
                    (root / f"{stem}.developer.txt").write_text(developer, encoding="utf-8")
                    save_json(root / f"{stem}.tools.json", declared_tools)
                    requests.append({
                        "file": f"{stem}.json", "route": route, "model": data.get("model"),
                        "system_words": len(system.split()), "has_tools": bool(declared_tools),
                        "system_sha256": hashlib.sha256(system.encode()).hexdigest(),
                    })
                response, events = mock_response(provider, data.get("model", model))
                if data.get("stream"):
                    payload = "".join(f"event: {event['type']}\ndata: {json.dumps(event)}\n\n" for event in events).encode()
                    content_type = "text/event-stream"
                else:
                    payload = json.dumps(response).encode()
                    content_type = "application/json"
            self.send_response(200)
            self.send_header("Content-Type", content_type)
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)

    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    endpoint = f"http://127.0.0.1:{server.server_port}"
    command, env = launch_config(provider, binary, root, endpoint, model)
    try:
        with (root / "stdout.txt").open("w") as stdout, (root / "stderr.txt").open("w") as stderr:
            result = subprocess.run(command, cwd=workspace, env=env, stdin=subprocess.DEVNULL, stdout=stdout, stderr=stderr, timeout=40)
        diagnostics = (root / "stdout.txt").read_text() + (root / "stderr.txt").read_text()
        fallback = provider == "codex" and "Defaulting to fallback metadata" in diagnostics
        success = result.returncode == 0 and not fallback and any(r["has_tools"] and r["model"] == model for r in requests)
        manifest = {
            "model_selection": model, "exit_code": result.returncode, "success": success,
            "fallback_metadata": fallback, "requests": requests,
        }
        save_json(root / "manifest.json", manifest)
        return manifest
    finally:
        server.shutdown()
        server.server_close()
        thread.join()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("provider", choices=["codex", "claude"])
    parser.add_argument("--binary", help="CLI executable name or path")
    parser.add_argument("--model", action="append", required=True, help="model ID; repeat to compare variants")
    parser.add_argument("--output", type=Path, help="new capture directory; defaults to a temporary directory")
    args = parser.parse_args()
    binary = shutil.which(args.binary or args.provider)
    if not binary:
        parser.error("CLI executable not found")
    binary = str(Path(binary).resolve())
    if args.output:
        root = args.output.resolve()
        root.mkdir(parents=True, exist_ok=False)
    else:
        root = Path(tempfile.mkdtemp(prefix=f"wingman-{args.provider}-capture-")).resolve()
    save_json(root / "source.json", {
        "provider": args.provider, "binary": binary,
        "binary_sha256": hashlib.sha256(Path(binary).read_bytes()).hexdigest(),
        "version": subprocess.run([binary, "--version"], capture_output=True, text=True, check=True).stdout.strip(),
        "synthetic_response": True,
    })
    print(f"Captures: {root}", flush=True)
    failed = False
    for index, model in enumerate(args.model, 1):
        run = root / f"run-{index}"
        try:
            manifest = capture(args.provider, binary, run, model)
            failed |= not manifest["success"]
            print(f"{model}: {len(manifest['requests'])} requests; success={manifest['success']}; {run}", flush=True)
        except (OSError, subprocess.TimeoutExpired) as error:
            failed = True
            print(f"Capture failed: {error}; inspect {run}", flush=True)
    return int(failed)


if __name__ == "__main__":
    raise SystemExit(main())
