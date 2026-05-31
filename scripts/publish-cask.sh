#!/usr/bin/env bash
#
# Render the Homebrew cask for the Wingman Agent desktop app and push it to the
# tap. Invoked by GoReleaser's `after` hook (.goreleaser.app.yml) with the
# release version, after the .app zip has been built and its checksum computed.
#
# Why this exists: GoReleaser's OSS `homebrew_casks` pipe can only emit CLI
# `binary` stanzas. The `app "X.app"` stanza that installs a bundle into
# /Applications is a GoReleaser Pro feature, so we author the cask ourselves.
#
# Requires: GITHUB_TOKEN (write access to the tap) — the same token GoReleaser
# uses for the release.
set -euo pipefail

VERSION="${1:?usage: publish-cask.sh <version>}"

# No-op on snapshot / dry-run builds (version looks like 0.8.0-SNAPSHOT-abc123).
case "$VERSION" in
  *SNAPSHOT* | *snapshot* | *-dirty)
    echo "publish-cask: snapshot build ($VERSION), skipping tap update"
    exit 0
    ;;
esac

TAP_OWNER="adrianliechti"
TAP_REPO="homebrew-tap"
CASK_NAME="wingman-desktop"
APP_NAME="Wingman Agent.app"
BUNDLE_ID="com.wails.Wingman Agent"
REPO_URL="https://github.com/adrianliechti/wingman-agent"

ARCHIVE="dist/app/${CASK_NAME}_${VERSION}_macOS_arm64.zip"
if [ ! -f "$ARCHIVE" ]; then
  echo "publish-cask: archive not found: $ARCHIVE" >&2
  exit 1
fi

SHA256="$(shasum -a 256 "$ARCHIVE" | awk '{print $1}')"
echo "publish-cask: ${CASK_NAME} ${VERSION} sha256=${SHA256}"

# CASK_DRY_RUN=1 renders the cask to stdout and exits — no tap clone/push.
if [ "${CASK_DRY_RUN:-0}" = "1" ]; then
  CASK_FILE="/dev/stdout"
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

# Note: ${VERSION}/${SHA256} are expanded by the shell; #{version}/#{appdir}
# are literal Ruby string interpolations evaluated by Homebrew at install time.
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

  app "${APP_NAME}"

  # The app is not code-signed / notarized, so macOS quarantines the download
  # and refuses to open it. Strip the quarantine attribute on install.
  postflight do
    system_command "/usr/bin/xattr",
                   args: ["-dr", "com.apple.quarantine", "#{appdir}/${APP_NAME}"]
  end

  uninstall quit: "${BUNDLE_ID}"

  zap trash: [
    "~/Library/Caches/${BUNDLE_ID}",
    "~/Library/HTTPStorages/${BUNDLE_ID}",
    "~/Library/Saved Application State/${BUNDLE_ID}.savedState",
    "~/Library/WebKit/${BUNDLE_ID}",
  ]
end
EOF

if [ "${CASK_DRY_RUN:-0}" = "1" ]; then
  exit 0
fi

cd "$WORKDIR/tap"

if git diff --quiet -- "Casks/${CASK_NAME}.rb"; then
  echo "publish-cask: cask already up to date, nothing to push"
  exit 0
fi

git add "Casks/${CASK_NAME}.rb"
git \
  -c user.name="Adrian Liechti" \
  -c user.email="adrian@localhost" \
  commit -m "${CASK_NAME} ${VERSION}" >/dev/null
git push origin HEAD >/dev/null 2>&1

echo "publish-cask: pushed ${CASK_NAME} ${VERSION} to ${TAP_OWNER}/${TAP_REPO}"
