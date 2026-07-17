#!/usr/bin/env python3
"""Transactional Docker deployment tool for the production CLIProxyAPI server.

The tool is intentionally dependency-free and compatible with Python 3.6.
Run ``inspect`` and ``plan`` for read-only checks. ``deploy`` and ``rollback``
require root privileges and an explicit ``--yes`` confirmation.
"""

import argparse
import datetime
import fcntl
import hashlib
import json
import os
import re
import signal
import shutil
import subprocess
import sys
import tarfile
import time
import urllib.error
import urllib.request


DEFAULT_PROJECT_DIR = "/home/ubuntu/streamlit_app/CLIProxyAPI"
DEFAULT_DEPLOY_DIR = "/opt/midrelay/cliproxyapi"
DEFAULT_CONTAINER_NAME = "cliproxyapi"
DEFAULT_COMPOSE_PROJECT = "cliproxyapi"
DEFAULT_IMAGE_REPOSITORY = "local/cliproxyapi"
DEFAULT_GIT_REF = "origin/tech"
DEFAULT_HEALTH_PATH = "/healthz"

COMMAND_TIMEOUT_SECONDS = 300
BUILD_TIMEOUT_SECONDS = 1800
DEPLOY_TIMEOUT_SECONDS = 300
HEALTH_ATTEMPTS = 20
HEALTH_INTERVAL_SECONDS = 3
MINIMUM_FREE_BYTES = 5 * 1024 * 1024 * 1024


class DeploymentError(RuntimeError):
    """Raised when a deployment step cannot be completed safely."""


class DeploymentInterrupted(DeploymentError):
    """Raised when a deploy process receives an operating-system interrupt."""


class DeploymentInterruptionGuard(object):
    """Convert termination signals into deploy errors that can be audited safely."""

    SIGNALS = (signal.SIGINT, signal.SIGTERM)

    def __init__(self):
        self.previous_handlers = {}

    def __enter__(self):
        for signum in self.SIGNALS:
            self.previous_handlers[signum] = signal.getsignal(signum)
            signal.signal(signum, self.interrupt)
        return self

    def __exit__(self, exc_type, exc_value, traceback):
        for signum, previous in self.previous_handlers.items():
            signal.signal(signum, previous)

    @staticmethod
    def interrupt(signum, frame):
        del frame
        raise DeploymentInterrupted("Received signal {0}".format(signum))


class DeploymentLock(object):
    """Prevent concurrent deploy or rollback operations."""

    def __init__(self, settings):
        self.path = os.path.join(settings.deploy_dir, ".deployment.lock")
        self.handle = None

    def __enter__(self):
        self.handle = open(self.path, "a+")
        os.chmod(self.path, 0o600)
        try:
            fcntl.flock(self.handle.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError:
            self.handle.close()
            self.handle = None
            raise DeploymentError("Another deployment operation is already running")
        self.handle.seek(0)
        self.handle.truncate()
        self.handle.write("pid={0} started={1}\n".format(os.getpid(), utc_iso_timestamp()))
        self.handle.flush()
        return self

    def __exit__(self, exc_type, exc_value, traceback):
        if self.handle is not None:
            fcntl.flock(self.handle.fileno(), fcntl.LOCK_UN)
            self.handle.close()


class Settings(object):
    """Validated deployment paths and identifiers."""

    def __init__(self):
        self.project_dir = os.environ.get("CLIPROXY_PROJECT_DIR", DEFAULT_PROJECT_DIR)
        self.deploy_dir = os.environ.get("CLIPROXY_DEPLOY_DIR", DEFAULT_DEPLOY_DIR)
        self.container_name = os.environ.get(
            "CLIPROXY_CONTAINER_NAME", DEFAULT_CONTAINER_NAME
        )
        self.compose_project = os.environ.get(
            "CLIPROXY_COMPOSE_PROJECT", DEFAULT_COMPOSE_PROJECT
        )
        self.image_repository = os.environ.get(
            "CLIPROXY_IMAGE_REPOSITORY", DEFAULT_IMAGE_REPOSITORY
        )
        self.build_network = os.environ.get("CLIPROXY_BUILD_NETWORK", "")
        self.build_http_proxy = os.environ.get("CLIPROXY_BUILD_HTTP_PROXY", "")
        self.skip_fetch = os.environ.get("CLIPROXY_SKIP_FETCH", "").lower() in (
            "1",
            "true",
            "yes",
        )
        self.build_apt_workaround = os.environ.get(
            "CLIPROXY_BUILD_APT_WORKAROUND", ""
        ).lower() in ("1", "true", "yes")
        self.config_file = os.path.join(self.deploy_dir, "config", "config.yaml")
        self.auth_dir = os.path.join(self.deploy_dir, "auths")
        self.log_dir = os.path.join(self.deploy_dir, "logs")
        self.compose_file = os.path.join(self.deploy_dir, "docker-compose.yml")
        self.environment_file = os.path.join(self.deploy_dir, "deployment.env")
        self.release_dir = os.path.join(self.deploy_dir, "releases")
        self.backup_dir = os.path.join(self.deploy_dir, "backups")
        self.deployment_dir = os.path.join(self.deploy_dir, "deployments")
        self.current_manifest = os.path.join(self.deployment_dir, "current.json")

        validate_identifier("container name", self.container_name)
        validate_identifier("Compose project", self.compose_project)
        validate_image_repository(self.image_repository)
        if self.build_network not in ("", "default", "host", "none"):
            raise DeploymentError(
                "Invalid Docker build network: {0!r}".format(self.build_network)
            )


def validate_identifier(label, value):
    if not re.match(r"^[A-Za-z0-9][A-Za-z0-9_.-]*$", value or ""):
        raise DeploymentError("Invalid {0}: {1!r}".format(label, value))


def validate_image_repository(value):
    if not re.match(r"^[A-Za-z0-9][A-Za-z0-9_./-]*$", value or ""):
        raise DeploymentError("Invalid image repository: {0!r}".format(value))


def run(command, cwd=None, timeout=COMMAND_TIMEOUT_SECONDS, check=True):
    """Run a command without a shell and return stripped stdout."""
    result = subprocess.run(
        command,
        cwd=cwd,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        universal_newlines=True,
        timeout=timeout,
    )
    if check and result.returncode != 0:
        stderr = (result.stderr or result.stdout).strip()
        raise DeploymentError(
            "Command failed ({0}): {1}\n{2}".format(
                result.returncode, " ".join(command), stderr
            )
        )
    return result


def utc_timestamp():
    return datetime.datetime.utcnow().strftime("%Y%m%dT%H%M%SZ")


def utc_iso_timestamp():
    return datetime.datetime.utcnow().replace(microsecond=0).isoformat() + "Z"


def sha256_file(path):
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def atomic_write(path, content, mode=0o600):
    parent = os.path.dirname(path)
    if parent:
        os.makedirs(parent, mode=0o700, exist_ok=True)
    temporary = path + ".tmp-{0}".format(os.getpid())
    with open(temporary, "w") as handle:
        handle.write(content)
        handle.flush()
        os.fsync(handle.fileno())
    os.chmod(temporary, mode)
    os.replace(temporary, path)


def atomic_write_json(path, value):
    atomic_write(path, json.dumps(value, indent=2, sort_keys=True) + "\n")


def read_server_port(config_path, default_port=3456):
    """Read only the top-level port key without parsing secret configuration."""
    pattern = re.compile(r"^port:\s*(\d+)\s*(?:#.*)?$")
    try:
        with open(config_path, "r") as handle:
            for line in handle:
                if line.startswith((" ", "\t")):
                    continue
                match = pattern.match(line.strip())
                if match:
                    port = int(match.group(1))
                    if 1 <= port <= 65535:
                        return port
    except OSError:
        pass
    return default_port


def render_compose(settings):
    """Render the canonical runtime Compose configuration."""
    return """name: {project}
services:
  cli-proxy-api:
    image: "${{CLIPROXY_IMAGE:?CLIPROXY_IMAGE must be set}}"
    container_name: {container}
    restart: unless-stopped
    network_mode: host
    environment:
      TZ: Asia/Shanghai
      CLIPROXY_LOG_LEVEL: info
    volumes:
      - "{auth_dir}:/opt/midrelay/cliproxyapi/auths:rw"
      - "{log_dir}:/CLIProxyAPI/logs:rw"
      - "{config_file}:/CLIProxyAPI/config.yaml:rw"
    logging:
      driver: json-file
      options:
        max-size: 50m
        max-file: "3"
""".format(
        project=settings.compose_project,
        container=settings.container_name,
        auth_dir=settings.auth_dir,
        log_dir=settings.log_dir,
        config_file=settings.config_file,
    )


def compose_command(settings, compose_file=None, environment_file=None):
    return [
        "docker",
        "compose",
        "--project-name",
        settings.compose_project,
        "--file",
        compose_file or settings.compose_file,
        "--env-file",
        environment_file or settings.environment_file,
    ]


def require_commands():
    missing = [name for name in ("docker", "git") if shutil.which(name) is None]
    if missing:
        raise DeploymentError("Missing required commands: {0}".format(", ".join(missing)))
    run(["docker", "compose", "version"])


def require_root():
    if os.geteuid() != 0:
        raise DeploymentError("deploy and rollback must run as root")


def preflight(settings):
    require_commands()
    if not os.path.isdir(os.path.join(settings.project_dir, ".git")):
        raise DeploymentError("Source repository is missing: {0}".format(settings.project_dir))
    if not os.path.isfile(settings.config_file):
        raise DeploymentError("Configuration is missing: {0}".format(settings.config_file))
    if not os.path.isdir(settings.auth_dir):
        raise DeploymentError("Authentication directory is missing: {0}".format(settings.auth_dir))
    if not os.path.isdir(settings.log_dir):
        raise DeploymentError("Log directory is missing: {0}".format(settings.log_dir))
    run(["docker", "info"])


def require_deploy_capacity(settings):
    free_bytes = shutil.disk_usage(settings.deploy_dir).free
    if free_bytes < MINIMUM_FREE_BYTES:
        raise DeploymentError(
            "At least 5 GiB free space is required; available bytes: {0}".format(
                free_bytes
            )
        )


def resolve_commit(settings, git_ref):
    result = run(
        ["git", "rev-parse", "--verify", "{0}^{{commit}}".format(git_ref)],
        cwd=settings.project_dir,
    )
    return result.stdout.strip()


def describe_commit(settings, commit):
    result = run(
        ["git", "describe", "--tags", "--always", commit],
        cwd=settings.project_dir,
    )
    return result.stdout.strip()


def container_inspect(settings):
    result = run(
        ["docker", "inspect", settings.container_name], check=False
    )
    if result.returncode != 0:
        return None
    values = json.loads(result.stdout)
    return values[0] if values else None


def check_health(settings, attempts=1):
    port = read_server_port(settings.config_file)
    url = "http://127.0.0.1:{0}{1}".format(port, DEFAULT_HEALTH_PATH)
    last_error = "no response"
    for attempt in range(attempts):
        try:
            request = urllib.request.Request(url, method="GET")
            response = urllib.request.urlopen(request, timeout=5)
            status = response.getcode()
            response.close()
            if status == 200:
                return True, url, "HTTP 200"
            last_error = "HTTP {0}".format(status)
        except urllib.error.HTTPError as exc:
            last_error = "HTTP {0}".format(exc.code)
        except Exception as exc:  # Network errors are reported, not hidden.
            last_error = str(exc)
        if attempt + 1 < attempts:
            time.sleep(HEALTH_INTERVAL_SECONDS)
    return False, url, last_error


def safe_container_summary(inspect_value):
    if inspect_value is None:
        return {"exists": False}
    state = inspect_value.get("State", {})
    config = inspect_value.get("Config", {})
    host_config = inspect_value.get("HostConfig", {})
    labels = config.get("Labels") or {}
    return {
        "exists": True,
        "image_id": inspect_value.get("Image"),
        "image_reference": config.get("Image"),
        "status": state.get("Status"),
        "started_at": state.get("StartedAt"),
        "restart_count": inspect_value.get("RestartCount"),
        "oom_killed": state.get("OOMKilled"),
        "network_mode": host_config.get("NetworkMode"),
        "restart_policy": (host_config.get("RestartPolicy") or {}).get("Name"),
        "compose_project": labels.get("com.docker.compose.project"),
        "compose_config_files": labels.get("com.docker.compose.project.config_files"),
    }


def inspect_environment(settings):
    preflight(settings)
    source_head = run(
        ["git", "rev-parse", "HEAD"], cwd=settings.project_dir
    ).stdout.strip()
    source_status = run(
        ["git", "status", "--porcelain"], cwd=settings.project_dir
    ).stdout.strip()
    healthy, health_url, health_result = check_health(settings)
    value = {
        "checked_at": utc_iso_timestamp(),
        "source": {
            "directory": settings.project_dir,
            "head": source_head,
            "worktree_clean": not bool(source_status),
        },
        "deployment": {
            "directory": settings.deploy_dir,
            "compose_file_exists": os.path.isfile(settings.compose_file),
            "environment_file_exists": os.path.isfile(settings.environment_file),
            "config_sha256": sha256_file(settings.config_file),
            "auth_file_count": len(
                [
                    name
                    for name in os.listdir(settings.auth_dir)
                    if os.path.isfile(os.path.join(settings.auth_dir, name))
                ]
            ),
        },
        "container": safe_container_summary(container_inspect(settings)),
        "health": {"healthy": healthy, "url": health_url, "result": health_result},
    }
    print(json.dumps(value, indent=2, sort_keys=True))


def plan_deployment(settings, git_ref):
    preflight(settings)
    commit = resolve_commit(settings, git_ref)
    image_tag = "{0}:{1}".format(settings.image_repository, commit)
    current = safe_container_summary(container_inspect(settings))
    value = {
        "action": "plan",
        "git_ref": git_ref,
        "target_commit": commit,
        "target_image": image_tag,
        "current_container": current,
        "health_url": "http://127.0.0.1:{0}{1}".format(
            read_server_port(settings.config_file), DEFAULT_HEALTH_PATH
        ),
        "mutations": [
            "fetch the configured Git remote",
            "create a detached release worktree",
            "build an immutable Docker image",
            "back up config and auth data with mode 0600",
            "replace the current container through canonical Compose",
            "automatically restore the previous image if health fails",
        ],
    }
    print(json.dumps(value, indent=2, sort_keys=True))


def create_persistent_backup(settings, deployment_id):
    backup_path = os.path.join(settings.backup_dir, deployment_id)
    os.makedirs(backup_path, mode=0o700)
    archive_path = os.path.join(backup_path, "persistent-data.tar.gz")
    with tarfile.open(archive_path, "w:gz") as archive:
        archive.add(settings.config_file, arcname="config/config.yaml", recursive=False)
        archive.add(settings.auth_dir, arcname="auths", recursive=True)
    os.chmod(archive_path, 0o600)
    verify_backup_archive(archive_path)
    return archive_path


def verify_backup_archive(archive_path):
    """Read every regular member so gzip and tar corruption fail the deploy."""
    config_found = False
    with tarfile.open(archive_path, "r:gz") as archive:
        for member in archive.getmembers():
            if member.name == "config/config.yaml":
                config_found = True
            if not member.isfile():
                continue
            handle = archive.extractfile(member)
            if handle is None:
                raise DeploymentError(
                    "Backup member cannot be read: {0}".format(member.name)
                )
            for unused in iter(lambda: handle.read(1024 * 1024), b""):
                pass
    if not config_found:
        raise DeploymentError("Backup archive does not contain config/config.yaml")


def create_release_worktree(settings, deployment_id, commit):
    release_path = os.path.join(settings.release_dir, deployment_id)
    source_path = os.path.join(release_path, "source")
    os.makedirs(release_path, mode=0o755)
    run(
        ["git", "worktree", "add", "--detach", source_path, commit],
        cwd=settings.project_dir,
    )
    return release_path, source_path


def apply_apt_transport_workaround(source_path):
    """Create an apt-free Dockerfile in an isolated build worktree only."""
    dockerfile_path = os.path.join(source_path, "Dockerfile")
    with open(dockerfile_path, "r") as handle:
        dockerfile = handle.read()
    builder_setup = (
        "RUN apt-get update && apt-get install -y --no-install-recommends "
        "build-essential git && rm -rf /var/lib/apt/lists/*\n\n"
    )
    runtime_setup = (
        "RUN apt-get update && apt-get install -y --no-install-recommends "
        "tzdata ca-certificates && rm -rf /var/lib/apt/lists/*\n\n"
    )
    timezone_setup = (
        "RUN cp /usr/share/zoneinfo/${TZ} /etc/localtime && "
        "echo \"${TZ}\" > /etc/timezone\n"
    )
    if (
        builder_setup not in dockerfile
        or runtime_setup not in dockerfile
        or timezone_setup not in dockerfile
        or "RUN go mod download\n" not in dockerfile
    ):
        raise DeploymentError("Dockerfile does not match the apt-free build workaround")
    updated = dockerfile.replace(builder_setup, "")
    updated = updated.replace(runtime_setup, "")
    updated = updated.replace(
        "RUN go mod download\n", "RUN GODEBUG=http2client=0 go mod download\n", 1
    )
    updated = updated.replace(
        "FROM debian:bookworm\n\n",
        "FROM debian:bookworm\n\n"
        "COPY --from=builder /etc/ssl/certs/ca-certificates.crt "
        "/etc/ssl/certs/ca-certificates.crt\n"
        "COPY --from=builder /usr/share/zoneinfo/Asia/Shanghai /etc/localtime\n\n",
        1,
    )
    updated = updated.replace(
        timezone_setup, "RUN echo \"${TZ}\" > /etc/timezone\n"
    )
    atomic_write(dockerfile_path, updated, mode=0o644)


def validate_staged_compose(settings, release_path, image_tag):
    compose_path = os.path.join(release_path, "docker-compose.yml")
    environment_path = os.path.join(release_path, "deployment.env")
    atomic_write(compose_path, render_compose(settings), mode=0o644)
    atomic_write(environment_path, "CLIPROXY_IMAGE={0}\n".format(image_tag))
    run(compose_command(settings, compose_path, environment_path) + ["config", "--quiet"])
    return compose_path, environment_path


def verify_built_image(image_tag, commit):
    """Verify that the image starts and exposes the expected build commit."""
    result = run(
        [
            "docker",
            "run",
            "--rm",
            "--network",
            "none",
            image_tag,
            "./CLIProxyAPI",
            "-h",
        ],
        timeout=60,
        check=False,
    )
    output = (result.stdout or "") + "\n" + (result.stderr or "")
    if result.returncode != 0:
        raise DeploymentError(
            "Built image executable check failed ({0})".format(result.returncode)
        )
    if "Commit: {0}".format(commit) not in output:
        raise DeploymentError("Built image does not report target commit {0}".format(commit))


def validate_active_container(summary, expected_image):
    if not summary.get("exists"):
        raise DeploymentError("Expected container was not created")
    if summary.get("status") != "running":
        raise DeploymentError(
            "Container is not running: {0}".format(summary.get("status"))
        )
    if summary.get("oom_killed"):
        raise DeploymentError("Container was OOM-killed")
    if summary.get("image_reference") != expected_image:
        raise DeploymentError(
            "Active image mismatch: expected {0}, got {1}".format(
                expected_image, summary.get("image_reference")
            )
        )


def stop_and_remove_container(settings):
    if container_inspect(settings) is None:
        return
    run(["docker", "stop", "--time", "30", settings.container_name], timeout=60)
    run(["docker", "rm", settings.container_name], timeout=60)


def activate_image(settings, image_tag, compose_content):
    atomic_write(settings.compose_file, compose_content, mode=0o644)
    atomic_write(settings.environment_file, "CLIPROXY_IMAGE={0}\n".format(image_tag))
    run(compose_command(settings) + ["config", "--quiet"])
    stop_and_remove_container(settings)
    run(
        compose_command(settings)
        + ["up", "-d", "--force-recreate", "--remove-orphans", "--pull", "never"],
        timeout=DEPLOY_TIMEOUT_SECONDS,
    )


def rollback_to_image(settings, image_tag, compose_content):
    print("Rolling back to {0}".format(image_tag))
    activate_image(settings, image_tag, compose_content)
    healthy, health_url, health_result = check_health(settings, HEALTH_ATTEMPTS)
    if not healthy:
        raise DeploymentError(
            "Rollback started but health failed at {0}: {1}".format(
                health_url, health_result
            )
        )
    validate_active_container(
        safe_container_summary(container_inspect(settings)), image_tag
    )
    print("Rollback health check passed: {0}".format(health_url))


def deploy(settings, git_ref, pull_base):
    require_root()
    preflight(settings)
    require_deploy_capacity(settings)
    if not settings.skip_fetch:
        run(["git", "fetch", "--prune", "origin"], cwd=settings.project_dir)
    commit = resolve_commit(settings, git_ref)
    short_commit = commit[:12]
    deployment_id = "{0}-{1}".format(utc_timestamp(), short_commit)
    image_tag = "{0}:{1}".format(settings.image_repository, commit)
    current = container_inspect(settings)
    current_summary = safe_container_summary(current)
    previous_image_id = current_summary.get("image_id")
    previous_image_tag = None
    if previous_image_id:
        previous_image_tag = "{0}:rollback-{1}".format(
            settings.image_repository, deployment_id
        )
        run(["docker", "tag", previous_image_id, previous_image_tag])

    archive_path = create_persistent_backup(settings, deployment_id)
    release_path, source_path = create_release_worktree(
        settings, deployment_id, commit
    )
    if settings.build_apt_workaround:
        apply_apt_transport_workaround(source_path)
    version = describe_commit(settings, commit)
    compose_content = render_compose(settings)
    validate_staged_compose(settings, release_path, image_tag)

    manifest_path = os.path.join(settings.deployment_dir, deployment_id + ".json")
    manifest = {
        "deployment_id": deployment_id,
        "created_at": utc_iso_timestamp(),
        "status": "building",
        "git_ref": git_ref,
        "target_commit": commit,
        "target_version": version,
        "target_image": image_tag,
        "previous_image_id": previous_image_id,
        "previous_image_tag": previous_image_tag,
        "previous_container": current_summary,
        "backup_archive": archive_path,
        "backup_sha256": sha256_file(archive_path),
        "release_path": release_path,
        "config_sha256": sha256_file(settings.config_file),
    }
    atomic_write_json(manifest_path, manifest)

    build_command = ["docker", "build"]
    if settings.build_network:
        build_command.extend(["--network", settings.build_network])
    if settings.build_http_proxy:
        build_command.extend(
            [
                "--build-arg",
                "HTTP_PROXY={0}".format(settings.build_http_proxy),
                "--build-arg",
                "HTTPS_PROXY={0}".format(settings.build_http_proxy),
            ]
        )
    if pull_base:
        build_command.append("--pull")
    build_command.extend(
        [
            "--build-arg",
            "VERSION={0}".format(version),
            "--build-arg",
            "COMMIT={0}".format(commit),
            "--build-arg",
            "BUILD_DATE={0}".format(utc_iso_timestamp()),
            "--tag",
            image_tag,
            source_path,
        ]
    )

    switch_started = False
    try:
        print("Building {0} from {1}".format(image_tag, commit))
        run(build_command, timeout=BUILD_TIMEOUT_SECONDS)
        verify_built_image(image_tag, commit)
        manifest["status"] = "switching"
        atomic_write_json(manifest_path, manifest)

        switch_started = True
        activate_image(settings, image_tag, compose_content)
        healthy, health_url, health_result = check_health(settings, HEALTH_ATTEMPTS)
        manifest["health_url"] = health_url
        manifest["health_result"] = health_result
        if not healthy:
            raise DeploymentError(
                "New deployment failed health at {0}: {1}".format(
                    health_url, health_result
                )
            )

        active_container = safe_container_summary(container_inspect(settings))
        validate_active_container(active_container, image_tag)
        manifest["status"] = "healthy"
        manifest["completed_at"] = utc_iso_timestamp()
        manifest["active_container"] = active_container
        atomic_write_json(manifest_path, manifest)
        atomic_write_json(settings.current_manifest, manifest)
        print("Deployment healthy: {0}".format(image_tag))
        print("Manifest: {0}".format(manifest_path))
    except Exception as exc:
        if isinstance(exc, DeploymentInterrupted) and not switch_started:
            manifest["status"] = "aborted_before_switch"
            manifest["interruption"] = str(exc)
            manifest["aborted_at"] = utc_iso_timestamp()
        else:
            manifest["status"] = "failed"
            manifest["failure"] = str(exc)
            manifest["failed_at"] = utc_iso_timestamp()
        atomic_write_json(manifest_path, manifest)
        if previous_image_tag and switch_started:
            try:
                rollback_to_image(settings, previous_image_tag, compose_content)
                manifest["status"] = "rolled_back"
                manifest["rolled_back_at"] = utc_iso_timestamp()
                atomic_write_json(manifest_path, manifest)
                atomic_write_json(settings.current_manifest, manifest)
            except Exception as rollback_error:
                manifest["rollback_failure"] = str(rollback_error)
                atomic_write_json(manifest_path, manifest)
                raise DeploymentError(
                    "Deployment failed and rollback failed: {0}; rollback: {1}".format(
                        exc, rollback_error
                    )
                )
        raise DeploymentError(str(exc))


def load_manifest(path):
    with open(path, "r") as handle:
        return json.load(handle)


def rollback(settings, manifest_path):
    require_root()
    preflight(settings)
    selected_path = manifest_path or settings.current_manifest
    if not os.path.isfile(selected_path):
        raise DeploymentError("Deployment manifest is missing: {0}".format(selected_path))
    manifest = load_manifest(selected_path)
    previous_image_tag = manifest.get("previous_image_tag")
    if not previous_image_tag:
        raise DeploymentError("Manifest has no previous image tag: {0}".format(selected_path))
    run(["docker", "image", "inspect", previous_image_tag])
    rollback_to_image(settings, previous_image_tag, render_compose(settings))
    result = {
        "action": "manual_rollback",
        "rolled_back_at": utc_iso_timestamp(),
        "source_manifest": selected_path,
        "active_image": previous_image_tag,
        "active_container": safe_container_summary(container_inspect(settings)),
    }
    atomic_write_json(settings.current_manifest, result)
    print("Rollback completed: {0}".format(previous_image_tag))


def parse_args(argv):
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="command")

    subparsers.add_parser("inspect", help="Print a read-only deployment inventory")

    plan_parser = subparsers.add_parser("plan", help="Resolve and print a read-only plan")
    plan_parser.add_argument("--ref", default=DEFAULT_GIT_REF, help="Git ref to deploy")

    deploy_parser = subparsers.add_parser("deploy", help="Build and deploy transactionally")
    deploy_parser.add_argument("--ref", default=DEFAULT_GIT_REF, help="Git ref to deploy")
    deploy_parser.add_argument(
        "--pull-base", action="store_true", help="Refresh mutable Docker base tags"
    )
    deploy_parser.add_argument("--yes", action="store_true", help="Confirm production mutation")

    rollback_parser = subparsers.add_parser("rollback", help="Restore the previous image")
    rollback_parser.add_argument("--manifest", help="Deployment manifest to roll back")
    rollback_parser.add_argument("--yes", action="store_true", help="Confirm production mutation")

    args = parser.parse_args(argv)
    if not args.command:
        parser.print_help()
        parser.exit(2)
    if args.command in ("deploy", "rollback") and not args.yes:
        parser.error("{0} requires --yes".format(args.command))
    return args


def main(argv=None):
    try:
        args = parse_args(argv or sys.argv[1:])
        settings = Settings()
        if args.command == "inspect":
            inspect_environment(settings)
        elif args.command == "plan":
            plan_deployment(settings, args.ref)
        elif args.command == "deploy":
            require_root()
            with DeploymentLock(settings):
                with DeploymentInterruptionGuard():
                    deploy(settings, args.ref, args.pull_base)
        elif args.command == "rollback":
            require_root()
            with DeploymentLock(settings):
                rollback(settings, args.manifest)
        return 0
    except (DeploymentError, OSError, ValueError, subprocess.TimeoutExpired) as exc:
        print("ERROR: {0}".format(exc), file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
