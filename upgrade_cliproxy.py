#!/usr/bin/env python3
"""Skill: 升级 CLIProxyAPI (tech 分支) -> 重新编译 Docker 镜像并发布

用法: 调用此脚本即可执行升级，或 import 后手动调用 upgrade_cliproxy()

步骤:
1. git pull 拉取最新代码
2. 生成 docker-compose.yml (参考初始部署脚本 03-deploy-cliproxyapi.sh)
3. 构建带版本信息的 Docker 镜像 (local/cli-proxy-api:tech)
4. 重启容器并健康检查
5. 返回升级报告

⚠️ 重要: AUTH_DIR (/opt/midrelay/cliproxyapi/auths) 包含 Claude 认证文件，
   升级前后必须保护，严禁删除或清空！
"""

import subprocess
import sys
import os
import time
import shutil
import re
from datetime import datetime

PROJECT_DIR = '/home/ubuntu/streamlit_app/CLIProxyAPI'
BRANCH = 'tech'
IMAGE_TAG = f'local/cliproxyapi:{BRANCH}'

# 部署目录 (实际使用的路径)
DEPLOY_DIR = '/opt/midrelay/cliproxyapi'
CONTAINER_NAME = 'cliproxyapi'
CONFIG_DIR = '/opt/midrelay/cliproxyapi/config'
CONFIG_FILE = os.path.join(CONFIG_DIR, 'config.yaml')
AUTH_DIR = '/opt/midrelay/cliproxyapi/auths'
LOG_DIR = '/opt/midrelay/cliproxyapi/logs'
HEALTH_PATH = '/healthz'
BUILD_TIMEOUT_SECONDS = 1800
DEPLOY_TIMEOUT_SECONDS = 300


def run(cmd, cwd=None, capture=True, timeout=300):
    try:
        result = subprocess.run(
            cmd,
            shell=True,
            cwd=cwd or PROJECT_DIR,
            capture_output=capture,
            text=True,
            timeout=timeout,
        )
        return result.returncode, result.stdout.strip(), result.stderr.strip()
    except subprocess.TimeoutExpired as exc:
        return 124, (exc.stdout or '').strip(), f'Command timed out after {timeout}s: {cmd}'
    except Exception as exc:  # noqa: BLE001
        return 1, '', f'Command failed unexpectedly: {exc}'


def read_server_port(config_path, default_port=3456):
    """Read top-level server port from config.yaml; fallback to default_port."""
    if not os.path.isfile(config_path):
        return default_port

    # Prefer top-level line: `port: 8317` (no indentation).
    top_level_port_pattern = re.compile(r'^port:\s*(\d+)\s*(?:#.*)?$')
    try:
        with open(config_path, 'r', encoding='utf-8') as f:
            for line in f:
                if line.startswith(' ') or line.startswith('\t'):
                    continue
                stripped = line.strip()
                if not stripped or stripped.startswith('#'):
                    continue
                m = top_level_port_pattern.match(stripped)
                if not m:
                    continue
                port = int(m.group(1))
                if 1 <= port <= 65535:
                    return port
    except Exception:
        pass
    return default_port


def current_container_image_id():
    rc, out, _ = run(f'docker inspect --format "{{{{.Image}}}}" {CONTAINER_NAME}', timeout=30)
    if rc == 0 and out:
        return out.strip()
    return ''


def wait_for_health(port, path=HEALTH_PATH, attempts=20, interval_seconds=3):
    """Wait until API responds HTTP 200 on health endpoint."""
    last = 'N/A'
    for _ in range(attempts):
        rc, out, _ = run(
            f'curl -sS -o /dev/null -w "%{{http_code}}" http://127.0.0.1:{port}{path}',
            timeout=15
        )
        code = out.strip() if out else ''
        last = code or 'N/A'
        if rc == 0 and code == '200':
            return True, code
        time.sleep(interval_seconds)
    return False, last


def rollback_deployment(previous_image_id, report_lines):
    if not previous_image_id:
        report_lines.append('[WARN] No previous container image found, rollback skipped')
        return False

    report_lines.append(f'[INFO] Rolling back to previous image: {previous_image_id}')
    rc, _, err = run(f'docker tag {previous_image_id} {IMAGE_TAG}', timeout=60)
    if rc != 0:
        report_lines.append(f'[ERROR] Rollback failed during image tag restore: {err}')
        return False

    rc, _, err = run(
        'docker compose up -d --pull never --force-recreate',
        cwd=DEPLOY_DIR,
        timeout=DEPLOY_TIMEOUT_SECONDS
    )
    if rc != 0:
        report_lines.append(f'[ERROR] Rollback compose up failed: {err}')
        return False

    report_lines.append('[OK] Rollback deployment started')
    return True


def generate_compose():
    """生成 docker-compose.yml (与 /opt/midrelay/cliproxyapi/docker-compose.yml 保持一致)"""
    compose = f"""services:
  {CONTAINER_NAME}:
    build:
      context: {PROJECT_DIR}
      dockerfile: Dockerfile
    image: {IMAGE_TAG}
    container_name: {CONTAINER_NAME}
    restart: always
    network_mode: host
    environment:
      TZ: Asia/Shanghai
      CLIPROXY_LOG_LEVEL: info
    volumes:
      - ./auths:/opt/midrelay/cliproxyapi/auths
      - ./logs:/opt/midrelay/cliproxyapi/logs
      - ./config/config.yaml:/CLIProxyAPI/config.yaml:ro
    logging:
      driver: "json-file"
      options:
        max-size: "50m"
        max-file: "3"
"""
    compose_path = os.path.join(DEPLOY_DIR, 'docker-compose.yml')
    os.makedirs(DEPLOY_DIR, exist_ok=True)
    with open(compose_path, 'w') as f:
        f.write(compose)
    return compose_path


def upgrade_cliproxy():
    report_lines = []
    previous_image_id = current_container_image_id()

    try:
        # Step 0: 确保必要目录和配置文件存在，并备份认证文件
        os.makedirs(CONFIG_DIR, exist_ok=True)
        os.makedirs(AUTH_DIR, exist_ok=True)
        os.makedirs(LOG_DIR, exist_ok=True)
        os.makedirs(DEPLOY_DIR, exist_ok=True)

        # 🔒 保护认证文件：升级前备份 auths 目录
        auth_backup_dir = f"{AUTH_DIR}.backup_{datetime.now().strftime('%Y%m%d_%H%M%S')}"
        auth_files = os.listdir(AUTH_DIR) if os.path.isdir(AUTH_DIR) else []
        if auth_files:
            shutil.copytree(AUTH_DIR, auth_backup_dir)
            report_lines.append(f'[OK] Auth files backed up to {auth_backup_dir} ({len(auth_files)} files)')
        else:
            report_lines.append('[INFO] Auth directory is empty, no backup needed')

        if not os.path.isfile(CONFIG_FILE):
            report_lines.append(f'[ERROR] Config file not found: {CONFIG_FILE}')
            report_lines.append('  Please run 03-deploy-cliproxyapi.sh first to generate config.')
            return '\n'.join(report_lines)

        # Step 1: git pull
        rc, out, err = run(f'git pull origin {BRANCH}')
        if rc == 0 and 'Already up to date' not in out:
            report_lines.append(f'[OK] Git pull: {out}')
        elif 'Already up to date' in out:
            report_lines.append('[INFO] Git already up to date')
        else:
            report_lines.append(f'[ERROR] Git pull failed: {err or out}')
            return '\n'.join(report_lines)

        # Get version info
        _, version, _ = run('git describe --tags --always --dirty')
        _, commit, _ = run('git rev-parse --short HEAD')
        build_date = datetime.utcnow().strftime('%Y-%m-%dT%H:%M:%SZ')
        report_lines.append(f'Version: {version}, Commit: {commit}')

        # Step 2: 生成 docker-compose.yml
        compose_path = generate_compose()
        report_lines.append(f'[OK] Generated docker-compose.yml at {compose_path}')

        # Step 3: docker compose build
        build_cmd = (
            f'docker compose build '
            f'--build-arg VERSION={version} '
            f'--build-arg COMMIT={commit} '
            f'--build-arg BUILD_DATE={build_date}'
        )
        rc, _, err = run(build_cmd, cwd=DEPLOY_DIR, timeout=BUILD_TIMEOUT_SECONDS)
        if rc != 0:
            report_lines.append(f'[ERROR] Docker build failed: {err}')
            return '\n'.join(report_lines)
        report_lines.append('[OK] Docker image built successfully')

        # Step 4: 启动新容器 (失败时回滚到旧镜像)
        rc, _, err = run(
            'docker compose up -d --pull never --force-recreate',
            cwd=DEPLOY_DIR,
            timeout=DEPLOY_TIMEOUT_SECONDS
        )
        if rc != 0:
            report_lines.append(f'[ERROR] Docker compose up failed: {err}')
            rollback_deployment(previous_image_id, report_lines)
            return '\n'.join(report_lines)
        report_lines.append('[OK] Container started')

        # Step 5: 健康检查 (失败则回滚并返回失败)
        server_port = read_server_port(CONFIG_FILE, default_port=3456)
        ok, code = wait_for_health(server_port, HEALTH_PATH)
        report_lines.append(f'Health check (http://127.0.0.1:{server_port}{HEALTH_PATH}): HTTP {code}')
        if not ok:
            report_lines.append('[ERROR] Health check failed after deployment, starting rollback')
            rollback_deployment(previous_image_id, report_lines)
            return '\n'.join(report_lines)

        rc, out, _ = run(f'docker ps --filter name={CONTAINER_NAME} --format "{{{{.Status}}}}"')
        if 'Up' in out:
            report_lines.append(f'[OK] Container running: {out}')
        else:
            report_lines.append(f'[WARN] Container status: {out}')

        # 读取容器日志确认无报错
        _, logs, _ = run(f'docker logs {CONTAINER_NAME} --tail 5')
        if 'error' in logs.lower() or 'failed' in logs.lower():
            report_lines.append('[WARN] Container logs contain suspicious words:')
            for line in logs.split('\n')[-3:]:
                report_lines.append(f'  {line}')
        else:
            report_lines.append('[OK] Container logs look clean')

        report_lines.append('\n[OK] CLIProxyAPI upgraded successfully.')
        report_lines.append(f'     Source  : {PROJECT_DIR}')
        report_lines.append(f'     Branch  : {BRANCH}')
        report_lines.append(f'     Image   : {IMAGE_TAG}')
        report_lines.append(f'     API     : http://127.0.0.1:{server_port}')
        report_lines.append(f'     Health  : http://127.0.0.1:{server_port}{HEALTH_PATH}')
        report_lines.append(f'     Config  : {CONFIG_FILE}')

        return '\n'.join(report_lines)
    except Exception as exc:  # noqa: BLE001
        report_lines.append(f'[ERROR] Unexpected exception: {exc}')
        rollback_deployment(previous_image_id, report_lines)
        return '\n'.join(report_lines)


if __name__ == '__main__':
    import os
    # Auto-elevate to sudo if not running as root, needed for /opt/midrelay/ operations
    if os.geteuid() != 0:
        os.execvp('sudo', ['sudo', sys.executable] + sys.argv)
    print(upgrade_cliproxy())
