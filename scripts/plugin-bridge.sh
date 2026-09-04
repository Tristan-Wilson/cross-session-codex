#!/bin/sh
# The plugin uses the independently installed Go binary.
set -eu
if command -v cross-session-codex >/dev/null 2>&1; then
  exec cross-session-codex "$@"
fi
if [ -x "$HOME/.local/bin/cross-session-codex" ]; then
  exec "$HOME/.local/bin/cross-session-codex" "$@"
fi
echo 'Cross Session Codex is not installed. Follow the README and run make install from its source directory.' >&2
exit 1
