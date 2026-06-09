#!/usr/bin/env sh
set -eu

PROJECT_ROOT="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
UPSTREAM_REPO="${UPSTREAM_REPO:-https://github.com/router-for-me/CLIProxyAPI.git}"
UPSTREAM_REF="${UPSTREAM_REF:-main}"
BUILD_IMAGE="${BUILD_IMAGE:-golang:1.26}"
GOPROXY_VALUE="${GOPROXY_VALUE:-https://goproxy.cn,direct}"
MODE="${1:-apply}"
TMPDIR="$(mktemp -d /tmp/cliproxyapi-upstream-XXXXXX)"
REPO_DIR="$TMPDIR/repo"

cleanup() {
  rm -rf "$TMPDIR"
}
trap cleanup EXIT INT TERM

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "missing required command: $1" >&2
    exit 1
  }
}

require_cmd git
require_cmd rsync
require_cmd docker
require_cmd python3

printf '==> cloning upstream %s (%s)\n' "$UPSTREAM_REPO" "$UPSTREAM_REF"
git clone --depth 1 --branch "$UPSTREAM_REF" "$UPSTREAM_REPO" "$REPO_DIR" >/dev/null 2>&1
UPSTREAM_COMMIT="$(git -C "$REPO_DIR" rev-parse HEAD)"
UPSTREAM_TAG="$(git -C "$REPO_DIR" describe --tags --exact-match 2>/dev/null || true)"

printf '==> upstream commit: %s\n' "$UPSTREAM_COMMIT"
[ -n "$UPSTREAM_TAG" ] && printf '==> upstream tag: %s\n' "$UPSTREAM_TAG"

if [ "$MODE" = "check" ]; then
  python3 - "$REPO_DIR" "$PROJECT_ROOT" <<'PY'
from pathlib import Path
import filecmp
import sys
src = Path(sys.argv[1])
dst = Path(sys.argv[2])
ignore = {'runtime', 'logs', 'auths', '.git'}
diffs = []
for p in src.rglob('*'):
    if p.is_dir():
        continue
    rel = p.relative_to(src)
    if rel.parts[0] in ignore or rel.as_posix() == 'config.local.yaml':
        continue
    q = dst / rel
    if not q.exists() or not filecmp.cmp(p, q, shallow=False):
        diffs.append(rel.as_posix())
print(f'check_mode_diffs={len(diffs)}')
for item in diffs[:50]:
    print(item)
PY
  exit 0
fi

printf '==> syncing source tree\n'
rsync -a --delete \
  --exclude 'config.local.yaml' \
  --exclude 'runtime/' \
  --exclude 'logs/' \
  --exclude 'auths/' \
  "$REPO_DIR/" "$PROJECT_ROOT/"

printf '==> building with %s\n' "$BUILD_IMAGE"
docker run --rm \
  -e GOPROXY="$GOPROXY_VALUE" \
  -v "$PROJECT_ROOT:/src" \
  -w /src \
  "$BUILD_IMAGE" \
  sh -lc '/usr/local/go/bin/go build -o /src/runtime/bin/CLIProxyAPI.new ./cmd/server'

printf '==> rotating binary and restarting service\n'
BACKUP="runtime/bin/CLIProxyAPI.bak-$(date +%Y%m%d-%H%M%S)"
cp "$PROJECT_ROOT/runtime/bin/CLIProxyAPI" "$PROJECT_ROOT/$BACKUP"
mv "$PROJECT_ROOT/runtime/bin/CLIProxyAPI.new" "$PROJECT_ROOT/runtime/bin/CLIProxyAPI"
pkill -f '^/root/.openclaw/projects/_ext_targets/CLIProxyAPI/runtime/bin/CLIProxyAPI -config /root/.openclaw/projects/_ext_targets/CLIProxyAPI/config.local.yaml$' || true
sleep 2
nohup "$PROJECT_ROOT/runtime/bin/CLIProxyAPI" -config "$PROJECT_ROOT/config.local.yaml" >"$PROJECT_ROOT/runtime/logs/server.out" 2>&1 &
sleep 3

printf '==> validating runtime\n'
pgrep -af '^/root/.openclaw/projects/_ext_targets/CLIProxyAPI/runtime/bin/CLIProxyAPI -config /root/.openclaw/projects/_ext_targets/CLIProxyAPI/config.local.yaml$'
ss -ltnp | grep ':8317' >/dev/null
curl -fsS http://127.0.0.1:8317/api/auth/status >/dev/null || true

echo "==> updating metadata"
python3 - "$PROJECT_ROOT/.upstream/metadata.json" "$UPSTREAM_COMMIT" "$UPSTREAM_TAG" <<'PY'
import json
import sys
path, commit, tag = sys.argv[1:4]
with open(path, 'r', encoding='utf-8') as f:
    data = json.load(f)
data['upstream']['last_synced_commit'] = commit
if tag:
    data['upstream']['last_synced_tag'] = tag
with open(path, 'w', encoding='utf-8') as f:
    json.dump(data, f, indent=2)
    f.write('\n')
PY

echo "done: commit=$UPSTREAM_COMMIT tag=${UPSTREAM_TAG:-unknown} backup=$BACKUP"
