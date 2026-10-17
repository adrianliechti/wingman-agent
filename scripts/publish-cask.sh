#!/usr/bin/env bash
#
# Render a Wingman Homebrew cask and push it to the tap. Invoked by GoReleaser's
# publishers with the release version and optional cask name (wingman-app by
# default), after the release artifacts have been built.
#
# Why this exists: GoReleaser's OSS `homebrew_casks` pipe can only emit CLI
# `binary` stanzas and still generates deprecated postflight hooks. We append
# postflight_steps to its CLI cask and render the desktop app cask ourselves.
#
# Requires: GITHUB_TOKEN (write access to the tap) — the same token GoReleaser
# uses for the release.
set -euo pipefail

VERSION="${1:?usage: publish-cask.sh <version> [wingman-app|wingman-cli]}"
CASK_NAME="${2:-wingman-app}"

# No-op on snapshot / dry-run builds (version looks like 0.8.0-SNAPSHOT-abc123).
case "$VERSION" in
  *SNAPSHOT* | *snapshot* | *-dirty)
    echo "publish-cask: snapshot build ($VERSION), skipping tap update"
    exit 0
    ;;
esac

TAP_OWNER="adrianliechti"
TAP_REPO="homebrew-tap"
APP_NAME="Wingman Agent.app"
BUNDLE_ID="com.adrianliechti.wingman-agent"
LEGACY_BUNDLE_ID="com.wails.Wingman Agent"
REPO_URL="https://github.com/adrianliechti/wingman-agent"

case "$CASK_NAME" in
  wingman-app)
    ARCHIVE="dist/app/${CASK_NAME}_${VERSION}_macOS_arm64.zip"
    if [ ! -f "$ARCHIVE" ]; then
      echo "publish-cask: archive not found: $ARCHIVE" >&2
      exit 1
    fi
    SHA256="$(shasum -a 256 "$ARCHIVE" | awk '{print $1}')"
    echo "publish-cask: ${CASK_NAME} ${VERSION} sha256=${SHA256}" >&2
    ;;
  wingman-cli)
    CLI_CASK="dist/homebrew/Casks/${CASK_NAME}.rb"
    if [ ! -f "$CLI_CASK" ] || [ "$(tail -n 1 "$CLI_CASK")" != "end" ]; then
      echo "publish-cask: missing or incomplete generated cask: $CLI_CASK" >&2
      exit 1
    fi
    if ! grep -Fqx "  version \"${VERSION}\"" "$CLI_CASK" || grep -Eq '^  postflight(_steps)? do' "$CLI_CASK"; then
      echo "publish-cask: regenerate $CLI_CASK with the current GoReleaser configuration for $VERSION" >&2
      exit 1
    fi
    ;;
  *)
    echo "publish-cask: unsupported cask: $CASK_NAME" >&2
    exit 1
    ;;
esac

# CASK_DRY_RUN=1 renders the cask to stdout and exits — no tap clone/push.
if [ "${CASK_DRY_RUN:-0}" = "1" ]; then
  CASK_FILE="$(mktemp)"
  trap 'rm -f "$CASK_FILE"' EXIT
else
  : "${GITHUB_TOKEN:?GITHUB_TOKEN is required to push the cask to the tap}"
  WORKDIR="$(mktemp -d)"
  trap 'rm -rf "$WORKDIR"' EXIT
  git clone --depth 1 \
    "https://x-access-token:${GITHUB_TOKEN}@github.com/${TAP_OWNER}/${TAP_REPO}.git" \
    "$WORKDIR/tap" >/dev/null 2>&1
  mkdir -p "$WORKDIR/tap/Casks"
  CASK_FILE="$WORKDIR/tap/Casks/${CASK_NAME}.rb"
fi

if [ "$CASK_NAME" = "wingman-cli" ]; then
  # Keep GoReleaser's platform URLs and checksums, then close the cask after
  # adding the unsigned macOS binary's install step.
  sed '$d' "$CLI_CASK" > "$CASK_FILE"
  cat >> "$CASK_FILE" <<'EOF'

  postflight_steps do
    on_macos do
      run "/usr/bin/xattr", args: ["-dr", "com.apple.quarantine", "{{staged_path}}/wingman"]
    end
  end
end
EOF
else
  # ${VERSION}/${SHA256} are expanded by the shell; #{version} is Ruby
  # interpolation and {{appdir}} is resolved by Homebrew's install-step runner.
  cat > "$CASK_FILE" <<EOF
cask "${CASK_NAME}" do
  version "${VERSION}"
  sha256 "${SHA256}"

  url "${REPO_URL}/releases/download/v#{version}/${CASK_NAME}_#{version}_macOS_arm64.zip"
  name "Wingman Agent"
  desc "AI-powered coding assistant desktop app"
  homepage "${REPO_URL}"

  livecheck do
    url :url
    strategy :github_latest
  end

  depends_on arch: :arm64
  depends_on macos: :monterey

  app "${APP_NAME}"

  # The app is not code-signed / notarized, so macOS quarantines the download
  # and refuses to open it. Strip the quarantine attribute on install.
  postflight_steps do
    run "/usr/bin/xattr",
        args: ["-dr", "com.apple.quarantine", "{{appdir}}/${APP_NAME}"]
  end

  uninstall quit: ["${BUNDLE_ID}", "${LEGACY_BUNDLE_ID}"]

  zap trash: [
    "~/Library/Caches/${BUNDLE_ID}",
    "~/Library/Caches/${LEGACY_BUNDLE_ID}",
    "~/Library/HTTPStorages/${BUNDLE_ID}",
    "~/Library/HTTPStorages/${LEGACY_BUNDLE_ID}",
    "~/Library/Saved Application State/${BUNDLE_ID}.savedState",
    "~/Library/Saved Application State/${LEGACY_BUNDLE_ID}.savedState",
    "~/Library/WebKit/${BUNDLE_ID}",
    "~/Library/WebKit/${LEGACY_BUNDLE_ID}",
  ]
end
EOF
fi

if [ "${CASK_DRY_RUN:-0}" = "1" ]; then
  cat "$CASK_FILE"
  exit 0
fi

cd "$WORKDIR/tap"

# Stage first, then diff the index — `git diff` alone ignores new/untracked files.
git add "Casks/${CASK_NAME}.rb"
if git diff --cached --quiet; then
  echo "publish-cask: cask already up to date, nothing to push"
  exit 0
fi

git \
  -c user.name="Adrian Liechti" \
  -c user.email="adrian@localhost" \
  commit -m "${CASK_NAME} ${VERSION}" >/dev/null
git push origin HEAD >/dev/null 2>&1

echo "publish-cask: pushed ${CASK_NAME} ${VERSION} to ${TAP_OWNER}/${TAP_REPO}"
