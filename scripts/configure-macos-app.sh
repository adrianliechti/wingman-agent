#!/usr/bin/env bash
# Add protected-resource metadata after appbundle generates Info.plist, then
# restore the ad-hoc signature invalidated by changing the bundle.
set -euo pipefail

APP_PATH="${1:?usage: configure-macos-app.sh <path-to-app-bundle>}"
PLIST_PATH="${APP_PATH}/Contents/Info.plist"
DESCRIPTION="Wingman Agent uses the microphone to record prompts for transcription by your configured AI provider."

if ! /usr/libexec/PlistBuddy \
  -c "Set :NSMicrophoneUsageDescription ${DESCRIPTION}" \
  "${PLIST_PATH}" 2>/dev/null; then
  /usr/libexec/PlistBuddy \
    -c "Add :NSMicrophoneUsageDescription string ${DESCRIPTION}" \
    "${PLIST_PATH}"
fi

/usr/bin/codesign --force --sign - "${APP_PATH}"

