#!/usr/bin/env python3
"""Capture agy's Gemini requests using an isolated config and localhost mock.

No real API key or model inference is required. Responses are synthetic and
contain no tool calls. The capture is evidence of request construction, not an
evaluation of model behavior. Requires Antigravity CLI with the Gemini provider.
"""

import argparse
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


def save_json(path, data):
    path.write_text(json.dumps(data, indent=2) + "\n", encoding="utf-8")


def capture(binary, root, model, mode):
    config = root / "gemini" / "antigravity-cli"
    config.mkdir(parents=True)
    save_json(config / "settings.json", {
        "modelProvider": "gemini", "enableTelemetry": False,
    })
    workspace = root / "workspace"
    workspace.mkdir()
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
            # Do not forward traffic or retain authentication headers/query strings.
            if not route.startswith(("/v1beta/models/", "/v1/models/")) or not route.endswith(
                (":streamGenerateContent", ":generateContent")
            ):
                self.send_error(502, "Offline capture: unexpected endpoint")
                return
            data = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))))
            system = "\n\n".join(
                part.get("text", "") for part in data.get("systemInstruction", {}).get("parts", [])
            )
            with lock:
                index = len(requests) + 1
                stem = f"request-{index}"
                save_json(root / f"{stem}.json", data)
                (root / f"{stem}.system.txt").write_text(system, encoding="utf-8")
                save_json(root / f"{stem}.tools.json", data.get("tools", []))
                requests.append({
                    "file": f"{stem}.json", "route": route,
                    "system_words": len(system.split()),
                    "system_sha256": hashlib.sha256(system.encode()).hexdigest(),
                    "has_tools": bool(data.get("tools")),
                })
            response = {
                "candidates": [{"content": {"role": "model", "parts": [{"text": "Capture complete."}]}, "finishReason": "STOP"}],
                "usageMetadata": {"promptTokenCount": 1, "candidatesTokenCount": 2, "totalTokenCount": 3},
            }
            streaming = route.endswith(":streamGenerateContent")
            payload = json.dumps(response)
            if streaming:
                payload = "data: " + payload + "\n\n"
            payload = payload.encode()
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream" if streaming else "application/json")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)

    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    endpoint = f"http://127.0.0.1:{server.server_port}"
    # Keep HOME unchanged; agy's explicit directory flags isolate its settings.
    # Do not inherit credentials, plugins, or endpoint overrides from the shell.
    env = {key: os.environ[key] for key in ("HOME", "PATH", "USER", "TMPDIR") if key in os.environ}
    env.update({
        "AGY_CLI_DISABLE_AUTO_UPDATE": "1",
        "GOOGLE_GEMINI_BASE_URL": endpoint,
        "GEMINI_API_KEY": "local-capture-dummy-key",
        "HTTP_PROXY": endpoint, "HTTPS_PROXY": endpoint,
        "NO_PROXY": "127.0.0.1,localhost",
    })
    command = [
        binary, "--gemini_dir", str(root / "gemini"),
        "--app_data_dir", "antigravity-cli", "--log-file", str(root / "cli.log"),
        "--print", "Reply with the word ready. Do not use tools.",
        "--output-format", "json", "--disable-slash-commands", "--print-timeout", "20s",
    ]
    if model:
        command += ["--model", model]
    if mode:
        command += ["--mode", mode]
    try:
        with (root / "stdout.txt").open("w") as stdout, (root / "stderr.txt").open("w") as stderr:
            result = subprocess.run(command, cwd=workspace, env=env, stdout=stdout, stderr=stderr, timeout=30)
        success = result.returncode == 0 and any(request["has_tools"] for request in requests)
        manifest = {
            "model_selection": model or "default", "mode_selection": mode or "default",
            "exit_code": result.returncode, "success": success, "requests": requests,
        }
        save_json(root / "manifest.json", manifest)
        return manifest
    finally:
        server.shutdown()
        server.server_close()
        thread.join()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--agy", default="agy", help="CLI executable name or path")
    parser.add_argument("--output", type=Path, help="new directory for captures; defaults to a temporary directory")
    parser.add_argument("--model", action="append", help="CLI model ID; repeat to compare models")
    parser.add_argument("--mode", choices=["auto", "accept-edits", "plan"])
    args = parser.parse_args()
    binary = shutil.which(args.agy)
    if not binary:
        parser.error(f"executable not found: {args.agy}")
    binary = str(Path(binary).resolve())
    if args.output:
        root = args.output.resolve()
        root.mkdir(parents=True, exist_ok=False)
    else:
        root = Path(tempfile.mkdtemp(prefix="wingman-agy-capture-")).resolve()
    save_json(root / "source.json", {
        "binary": binary,
        "binary_sha256": hashlib.sha256(Path(binary).read_bytes()).hexdigest(),
        "synthetic_response": True,
    })
    print(f"Captures: {root}", flush=True)
    failed = False
    for index, model in enumerate(args.model or [None], 1):
        run = root / f"run-{index}"
        try:
            manifest = capture(binary, run, model, args.mode)
            failed |= not manifest["success"]
            print(f"{model or 'default'}: {len(manifest['requests'])} requests; success={manifest['success']}; {run}", flush=True)
        except (OSError, subprocess.TimeoutExpired) as error:
            failed = True
            print(f"Capture failed: {error}; inspect {run}", flush=True)
    return int(failed)


if __name__ == "__main__":
    raise SystemExit(main())
