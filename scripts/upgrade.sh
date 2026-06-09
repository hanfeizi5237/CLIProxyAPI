#!/usr/bin/env bash
# =============================================================================
# CLIProxyAPI 升级脚本
# 用途: 从 v6.9.30 (8f4a4eab) 升级到远程 origin/main 最新版本
# 特性: 自动备份、安全校验、一键回滚
# 用法: bash scripts/upgrade.sh
# =============================================================================
set -euo pipefail

# --- 配置 ---
PROJECT_DIR="/root/.openclaw/projects/_ext_targets/CLIProxyAPI"
BIN_DIR="${PROJECT_DIR}/runtime/bin"
BIN_PATH="${BIN_DIR}/CLIProxyAPI"
CONFIG_PATH="${PROJECT_DIR}/config.local.yaml"
BACKUP_DIR="${PROJECT_DIR}/.upgrade_backup"
GIT_DIR="${PROJECT_DIR}"
LOG_FILE="${PROJECT_DIR}/.upgrade.log"

# 颜色输出
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

info()  { echo -e "${BLUE}[INFO]${NC}  $*"; }
ok()    { echo -e "${GREEN}[OK]${NC}    $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC}  $*"; }
fail()  { echo -e "${RED}[FAIL]${NC}  $*"; }

log()   { echo "$(date '+%Y-%m-%d %H:%M:%S') $*" >> "$LOG_FILE"; }

# --- 前置检查 ---
echo ""
echo "========================================"
echo " CLIProxyAPI 升级脚本"
echo "========================================"
echo ""

# 检查是否在正确的目录
if [ ! -d "$GIT_DIR/.git" ]; then
    fail "项目目录不存在或不是 Git 仓库: $GIT_DIR"
    exit 1
fi

# 检查 Go 编译器
if ! command -v go &>/dev/null; then
    fail "未找到 Go 编译器 (go)，请先安装"
    exit 1
fi
GO_VERSION=$(go version)
ok "Go 环境: ${GO_VERSION}"

# 检查 git
if ! command -v git &>/dev/null; then
    fail "未找到 git"
    exit 1
fi

# --- 第 1 步: 记录当前状态 ---
info "=== 步骤 1/6: 记录当前状态 ==="

cd "$GIT_DIR"

CURRENT_COMMIT=$(git rev-parse HEAD)
CURRENT_TAG=$(git tag --points-at "$CURRENT_COMMIT" 2>/dev/null | head -1 || echo "untagged")
CURRENT_BIN_HASH=$(sha256sum "$BIN_PATH" 2>/dev/null | awk '{print $1}' || echo "unknown")
CURRENT_PID=$(pgrep -f "${BIN_PATH}" | head -1 || echo "none")

info "当前版本: ${CURRENT_TAG:-$CURRENT_COMMIT}"
info "当前 PID: ${CURRENT_PID}"
info "二进制哈希: ${CURRENT_BIN_HASH}"

# --- 第 2 步: 创建备份 ---
info "=== 步骤 2/6: 创建备份 ==="

mkdir -p "$BACKUP_DIR"

# 备份二进制文件
cp -v "$BIN_PATH" "${BACKUP_DIR}/CLIProxyAPI.bak"
ok "二进制文件已备份"

# 备份配置文件
cp -v "$CONFIG_PATH" "${BACKUP_DIR}/config.local.yaml.bak"
ok "配置文件已备份"

# 记录备份信息
cat > "${BACKUP_DIR}/backup_info.txt" <<EOF
备份时间: $(date '+%Y-%m-%d %H:%M:%S')
当前提交: ${CURRENT_COMMIT}
当前标签: ${CURRENT_TAG:-untagged}
二进制哈希: ${CURRENT_BIN_HASH}
配置文件: ${CONFIG_PATH}
EOF
ok "备份信息已记录: ${BACKUP_DIR}/backup_info.txt"

# --- 第 3 步: 拉取最新代码 ---
info "=== 步骤 3/6: 拉取远程最新代码 ==="

# 保存本地未跟踪文件列表
UNTRACKED=$(git ls-files --others --exclude-standard)

git fetch origin main --quiet
ok "远程代码已更新"

NEW_COMMIT=$(git rev-parse origin/main)
NEW_TAG=$(git tag --points-at "$NEW_COMMIT" 2>/dev/null | head -1 || echo "untagged")
COMMIT_COUNT=$(git rev-list --count "${CURRENT_COMMIT}..${NEW_COMMIT}" --first-parent)

info "远程版本: ${NEW_TAG:-$NEW_COMMIT}"
info "主线差异: ${COMMIT_COUNT} 个提交"

# 显示将要合并的提交
if [ "$COMMIT_COUNT" -gt 0 ]; then
    echo ""
    info "即将合并的提交:"
    git log --oneline --first-parent "${CURRENT_COMMIT}..${NEW_COMMIT}"
    echo ""
else
    warn "本地已经是最新，无需升级"
    exit 0
fi

# --- 第 4 步: 合并代码 ---
info "=== 步骤 4/6: 合并代码到本地 ==="

# 确保工作区干净（除了未跟踪文件）
if ! git diff --quiet 2>/dev/null; then
    fail "工作区有未提交的修改，请先提交或 stash"
    exit 1
fi

git merge origin/main --no-edit
ok "代码已合并到本地"

# --- 第 5 步: 编译新版本 ---
info "=== 步骤 5/6: 编译新版本 ==="

BUILD_OUTPUT="${BIN_PATH}.new"

# 获取构建信息
BUILD_VERSION="${NEW_TAG:-$NEW_COMMIT}"
BUILD_TIME=$(date '+%Y-%m-%dT%H:%M:%S%z')
BUILD_COMMIT="$NEW_COMMIT"

# 尝试带 ldflags 编译
LDFLAGS="-s -w \
  -X main.Version=${BUILD_VERSION} \
  -X main.Commit=${BUILD_COMMIT} \
  -X main.BuildTime=${BUILD_TIME}"

info "开始编译..."
if CGO_ENABLED=0 go build -ldflags "$LDFLAGS" -o "$BUILD_OUTPUT" .; then
    ok "编译成功"
    NEW_BIN_HASH=$(sha256sum "$BUILD_OUTPUT" | awk '{print $1}')
    info "新二进制哈希: ${NEW_BIN_HASH}"
else
    fail "编译失败！"
    rm -f "$BUILD_OUTPUT"
    exit 1
fi

# 验证新二进制可执行
if [ ! -x "$BUILD_OUTPUT" ]; then
    chmod +x "$BUILD_OUTPUT"
    ok "已添加执行权限"
fi

# 快速版本检查
if "$BUILD_OUTPUT" --version 2>/dev/null || "$BUILD_OUTPUT" -v 2>/dev/null; then
    ok "新二进制版本检查通过"
else
    info "无法通过 --version 验证（可能不支持该标志），跳过"
    # 不阻断，继续
fi

# --- 第 6 步: 替换并重启 ---
info "=== 步骤 6/6: 替换二进制并重启服务 ==="

# 获取当前进程详情
if [ "$CURRENT_PID" != "none" ]; then
    CURRENT_USER=$(ps -o user= -p "$CURRENT_PID" 2>/dev/null || echo "unknown")
    info "正在停止进程 PID=${CURRENT_PID} (用户: ${CURRENT_USER})"
    
    # 优雅停止 (SIGTERM)
    kill -TERM "$CURRENT_PID" 2>/dev/null
    
    # 等待进程退出（最多 10 秒）
    TIMEOUT=10
    for i in $(seq 1 $TIMEOUT); do
        if ! kill -0 "$CURRENT_PID" 2>/dev/null; then
            ok "进程已停止 (等待 ${i} 秒)"
            break
        fi
        if [ $i -eq $TIMEOUT ]; then
            warn "优雅停止超时，发送 SIGKILL"
            kill -KILL "$CURRENT_PID" 2>/dev/null || true
            sleep 1
        fi
        sleep 1
    done
else
    info "未检测到运行中的进程，跳过停止步骤"
    CURRENT_PID=""
fi

# 替换二进制文件
mv -f "$BUILD_OUTPUT" "$BIN_PATH"
ok "新二进制已替换到 ${BIN_PATH}"

# 重新启动服务
info "启动新服务..."
if [ -n "$CURRENT_PID" ] && [ "$CURRENT_USER" != "unknown" ]; then
    # 以原来的用户启动
    if [ "$CURRENT_USER" != "root" ]; then
        su - "$CURRENT_USER" -c "nohup ${BIN_PATH} -config ${CONFIG_PATH} >> ${PROJECT_DIR}/runtime/logs/upgrade_restart.log 2>&1 &"
    else
        nohup "${BIN_PATH}" -config "${CONFIG_PATH}" >> "${PROJECT_DIR}/runtime/logs/upgrade_restart.log" 2>&1 &
    fi
else
    # 以前台进程启动
    nohup "${BIN_PATH}" -config "${CONFIG_PATH}" >> "${PROJECT_DIR}/runtime/logs/upgrade_restart.log" 2>&1 &
fi

NEW_PID=$!

# 等待启动（最多 5 秒）
sleep 3
if kill -0 "$NEW_PID" 2>/dev/null; then
    ok "✅ 服务已启动，新 PID: ${NEW_PID}"
    
    # 验证健康检查
    sleep 2
    if command -v curl &>/dev/null; then
        # 尝试从配置中读取端口，默认 8080
        HEALTH_PORT=$(grep -oP 'port:\s*\K\d+' "$CONFIG_PATH" 2>/dev/null | head -1 || echo "8080")
        HEALTH_URL="http://127.0.0.1:${HEALTH_PORT}/healthz"
        
        if HEALTH_RESP=$(curl -sf --max-time 3 "$HEALTH_URL" 2>/dev/null); then
            ok "健康检查通过: ${HEALTH_URL} → ${HEALTH_RESP}"
        else
            warn "健康检查未响应 (端口可能不是 ${HEALTH_PORT})，请手动验证"
        fi
    fi
else
    fail "❌ 服务启动失败！"
    fail "查看日志: tail -f ${PROJECT_DIR}/runtime/logs/upgrade_restart.log"
    fail ""
    fail "=== 回滚命令 ==="
    fail "cp ${BACKUP_DIR}/CLIProxyAPI.bak ${BIN_PATH}"
    fail "nohup ${BIN_PATH} -config ${CONFIG_PATH} &"
    exit 1
fi

# --- 完成 ---
echo ""
echo "========================================"
echo " ✅ 升级完成！"
echo "========================================"
echo ""
echo "  旧版本: ${CURRENT_TAG:-$CURRENT_COMMIT}"
echo "  新版本: ${NEW_TAG:-$NEW_COMMIT}"
echo "  新 PID: ${NEW_PID}"
echo "  备份目录: ${BACKUP_DIR}"
echo ""
echo "=== 回滚命令 (如需) ==="
echo "  cp ${BACKUP_DIR}/CLIProxyAPI.bak ${BIN_PATH}"
echo "  kill ${NEW_PID}"
echo "  nohup ${BIN_PATH} -config ${CONFIG_PATH} &"
echo ""

log "升级成功: ${CURRENT_COMMIT} -> ${NEW_COMMIT}, 新PID=${NEW_PID}"
