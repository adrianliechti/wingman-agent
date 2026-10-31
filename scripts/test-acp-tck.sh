#!/usr/bin/env bash
set -euo pipefail

# Pin the experimental suite so CI and local results use the same requirements.
revision=b7f40fb3c003c45926547be5a1f7e2b374035815
root=$(cd "$(dirname "$0")/.." && pwd)
live=false
if [[ ${1:-} == --live ]]; then
  live=true
  shift
fi
build_dir=$(mktemp -d "${TMPDIR:-/tmp}/wingman-acp-tck.XXXXXX")
trap 'rm -rf "$build_dir"' EXIT
reports=${ACP_TCK_REPORT_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/wingman-acp-reports.XXXXXX")}
mkdir -p "$reports" "$build_dir/workspace"
reports=$(cd "$reports" && pwd)
tck=${ACP_TCK_DIR:-$build_dir/tck}
if [[ -z ${ACP_TCK_DIR:-} ]]; then
  mkdir -p "$tck"
  curl -fLsS "https://codeload.github.com/agentclientprotocol/acp-tck/tar.gz/$revision" -o "$build_dir/tck.tar.gz"
  tar -xzf "$build_dir/tck.tar.gz" -C "$tck" --strip-components=1
fi

cd "$root"
if [[ $# -eq 0 ]]; then
  set -- claude codex pi server
fi
timeout=10
test_timeout=45
cancel_prompt=ACP_CONTRACT_CANCEL
if $live; then
  go build -o "$build_dir/wingman" ./cmd/wingman
  timeout=90
  test_timeout=300
  cancel_prompt='Use a shell tool to sleep for 30 seconds, then reply DONE.'
fi
status=0
read -r -a versions <<< "${ACP_TCK_VERSIONS:-1 2}"
for version in "${versions[@]}"; do
  case "$version" in
    1|2) ;;
    *) echo "Unknown ACP version: $version" >&2; exit 2 ;;
  esac
done
for bridge in "$@"; do
  case "$bridge" in
    claude|codex|pi|server) ;;
    *) echo "Unknown bridge: $bridge" >&2; exit 2 ;;
  esac
  tck_args=(--agent-cwd "$build_dir/workspace")
  if $live; then
    if [[ $bridge == server ]]; then
      command=("$build_dir/wingman" acp)
    else
      command=("$build_dir/wingman" acp "$bridge" --backend "${ACP_TCK_BACKEND:-native}")
    fi
  else
    go test -c -o "$build_dir/$bridge.test" "./pkg/acp/$bridge"
    command=("$build_dir/$bridge.test" '-test.run=^TestACPTCK$')
    tck_args+=(--agent-env ACP_TCK_AGENT=1)
  fi
  for version in "${versions[@]}"; do
    uv run --project "$tck" --locked acp-tck \
      --protocol-version "$version" \
      "${tck_args[@]}" \
      --cancel-prompt "$cancel_prompt" \
      --timeout "$timeout" --test-timeout "$test_timeout" \
      --report-json "$reports/$bridge-v$version.json" \
      -- "${command[@]}" >"$reports/$bridge-v$version.log" 2>&1 || status=1
    echo "Finished $bridge ACP v$version; report: $reports/$bridge-v$version.json"
  done
done
echo "ACP TCK reports: $reports"
exit "$status"
