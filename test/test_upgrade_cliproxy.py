import importlib.util
import os
import signal
import tempfile
import unittest
from collections import namedtuple


REPOSITORY_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
MODULE_PATH = os.path.join(REPOSITORY_ROOT, "upgrade_cliproxy.py")
SPEC = importlib.util.spec_from_file_location("upgrade_cliproxy", MODULE_PATH)
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


class DeploymentToolTest(unittest.TestCase):
    """Verify the production deployment tool's safety-critical contracts."""

    def test_read_server_port_uses_top_level_value(self):
        with tempfile.NamedTemporaryFile(mode="w", delete=False) as handle:
            handle.write("nested:\n  port: 9999\nport: 3456 # API\n")
            path = handle.name
        try:
            self.assertEqual(MODULE.read_server_port(path), 3456)
        finally:
            os.unlink(path)

    def test_read_server_port_rejects_invalid_value(self):
        with tempfile.NamedTemporaryFile(mode="w", delete=False) as handle:
            handle.write("port: 70000\n")
            path = handle.name
        try:
            self.assertEqual(MODULE.read_server_port(path, default_port=8317), 8317)
        finally:
            os.unlink(path)

    def test_render_compose_contains_canonical_runtime_paths(self):
        settings = MODULE.Settings()
        compose = MODULE.render_compose(settings)
        self.assertIn("CLIPROXY_IMAGE must be set", compose)
        self.assertIn(settings.config_file + ":/CLIProxyAPI/config.yaml:rw", compose)
        self.assertIn(settings.auth_dir + ":/opt/midrelay/cliproxyapi/auths:rw", compose)
        self.assertIn(settings.log_dir + ":/CLIProxyAPI/logs:rw", compose)
        self.assertIn("restart: unless-stopped", compose)

    def test_identifier_validation_rejects_shell_syntax(self):
        with self.assertRaises(MODULE.DeploymentError):
            MODULE.validate_identifier("container", "cliproxyapi;rm")

    def test_deploy_requires_explicit_confirmation(self):
        with self.assertRaises(SystemExit):
            MODULE.parse_args(["deploy"])

    def test_interruption_guard_translates_termination_signal(self):
        with self.assertRaises(MODULE.DeploymentInterrupted):
            MODULE.DeploymentInterruptionGuard.interrupt(signal.SIGTERM, None)

    def test_deploy_capacity_rejects_small_filesystem(self):
        settings = MODULE.Settings()
        original_disk_usage = MODULE.shutil.disk_usage
        disk_usage = namedtuple("disk_usage", "total used free")
        MODULE.shutil.disk_usage = lambda unused: disk_usage(100, 96, 4)
        try:
            with self.assertRaises(MODULE.DeploymentError):
                MODULE.require_deploy_capacity(settings)
        finally:
            MODULE.shutil.disk_usage = original_disk_usage

    def test_validate_active_container_rejects_wrong_image(self):
        summary = {
            "exists": True,
            "status": "running",
            "oom_killed": False,
            "image_reference": "local/cliproxyapi:old",
        }
        with self.assertRaises(MODULE.DeploymentError):
            MODULE.validate_active_container(summary, "local/cliproxyapi:new")

    def test_verify_backup_archive_reads_config(self):
        with tempfile.TemporaryDirectory() as directory:
            config_dir = os.path.join(directory, "config")
            os.makedirs(config_dir)
            config_path = os.path.join(config_dir, "config.yaml")
            with open(config_path, "w") as handle:
                handle.write("port: 3456\n")
            archive_path = os.path.join(directory, "backup.tar.gz")
            with MODULE.tarfile.open(archive_path, "w:gz") as archive:
                archive.add(config_path, arcname="config/config.yaml")
            MODULE.verify_backup_archive(archive_path)

    def test_verify_built_image_requires_expected_commit(self):
        class Result(object):
            returncode = 0
            stdout = "CLIProxyAPI Version: test, Commit: expected, BuiltAt: now"
            stderr = ""

        original_run = MODULE.run
        MODULE.run = lambda *args, **kwargs: Result()
        try:
            MODULE.verify_built_image("local/cliproxyapi:test", "expected")
            with self.assertRaises(MODULE.DeploymentError):
                MODULE.verify_built_image("local/cliproxyapi:test", "different")
        finally:
            MODULE.run = original_run

    def test_apt_workaround_only_changes_build_dockerfile(self):
        with tempfile.TemporaryDirectory() as directory:
            dockerfile_path = os.path.join(directory, "Dockerfile")
            with open(dockerfile_path, "w") as handle:
                handle.write(
                    "FROM golang:1.26-bookworm AS builder\n"
                    "RUN apt-get update && apt-get install -y --no-install-recommends "
                    "build-essential git && rm -rf /var/lib/apt/lists/*\n\n"
                    "RUN go mod download\n"
                    "FROM debian:bookworm\n\n"
                    "RUN apt-get update && apt-get install -y --no-install-recommends "
                    "tzdata ca-certificates && rm -rf /var/lib/apt/lists/*\n\n"
                    "ENV TZ=Asia/Shanghai\n"
                    "RUN cp /usr/share/zoneinfo/${TZ} /etc/localtime && "
                    "echo \"${TZ}\" > /etc/timezone\n"
                )
            MODULE.apply_apt_transport_workaround(directory)
            with open(dockerfile_path, "r") as handle:
                updated = handle.read()
            self.assertNotIn("apt-get update", updated)
            self.assertIn("COPY --from=builder /etc/ssl/certs/ca-certificates.crt", updated)
            self.assertIn("COPY --from=builder /usr/share/zoneinfo/Asia/Shanghai", updated)
            self.assertIn("RUN GODEBUG=http2client=0 go mod download", updated)


if __name__ == "__main__":
    unittest.main()
