#!/usr/bin/env bash
# Compatibility entrypoint for the transactional Docker deployment tool.
set -euo pipefail

REPOSITORY_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

exec python3 "${REPOSITORY_DIR}/upgrade_cliproxy.py" deploy "$@"
