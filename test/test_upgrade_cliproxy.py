import importlib.util
import os
import signal
import tempfile
import unittest
from collections import namedtuple
from contextlib import ExitStack
from unittest import mock


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

    def test_prebuilt_requires_exact_commit_and_confirmation(self):
        commit = "a" * 40
        args = MODULE.parse_args(
            ["deploy", "--ref", commit, "--use-prebuilt", "--yes"]
        )
        self.assertTrue(args.use_prebuilt)
        for arguments in (
            ["deploy", "--ref", commit, "--use-prebuilt"],
            ["deploy", "--ref", "origin/tech", "--use-prebuilt", "--yes"],
            ["deploy", "--ref", commit, "--use-prebuilt", "--pull-base", "--yes"],
        ):
            with self.assertRaises(SystemExit):
                MODULE.parse_args(arguments)

    def test_prebuilt_image_rejects_wrong_platform(self):
        result = namedtuple("Result", "stdout")("linux/arm64\n")
        with mock.patch.object(MODULE, "run", return_value=result):
            with self.assertRaises(MODULE.DeploymentError):
                MODULE.verify_prebuilt_image("local/cliproxyapi:test", "a" * 40)

    def test_prebuilt_image_checks_embedded_commit(self):
        result = namedtuple("Result", "stdout")("linux/amd64\n")
        with mock.patch.object(MODULE, "run", return_value=result):
            with mock.patch.object(MODULE, "verify_built_image") as verify:
                MODULE.verify_prebuilt_image("local/cliproxyapi:test", "a" * 40)
        verify.assert_called_once_with("local/cliproxyapi:test", "a" * 40)

    def test_prebuilt_preflight_does_not_require_git_or_source(self):
        with tempfile.TemporaryDirectory() as directory:
            settings = MODULE.Settings()
            settings.project_dir = os.path.join(directory, "missing-source")
            settings.config_file = os.path.join(directory, "config.yaml")
            settings.auth_dir = os.path.join(directory, "auths")
            settings.log_dir = os.path.join(directory, "logs")
            with open(settings.config_file, "w") as handle:
                handle.write("port: 3456\n")
            os.mkdir(settings.auth_dir)
            os.mkdir(settings.log_dir)
            with mock.patch.object(MODULE.shutil, "which", side_effect=lambda x: None if x == "git" else x):
                with mock.patch.object(MODULE, "run"):
                    MODULE.preflight(settings, use_prebuilt=True)

    def test_image_metadata_returns_embedded_version(self):
        result = namedtuple("Result", "returncode stdout stderr")(
            0, "CLIProxyAPI Version: local-v1, Commit: " + "a" * 40 + ", BuiltAt: now", ""
        )
        with mock.patch.object(MODULE, "run", return_value=result):
            self.assertEqual(MODULE.verify_built_image("image", "a" * 40)["version"], "local-v1")

    def test_prebuilt_healthy_manifest_uses_image_version_without_git(self):
        self.run_prebuilt_transaction(False)

    def test_prebuilt_unhealthy_switch_records_rollback(self):
        self.run_prebuilt_transaction(True)

    def run_prebuilt_transaction(self, fail_health):
        commit = "a" * 40
        with tempfile.TemporaryDirectory() as directory, ExitStack() as stack:
            settings = MODULE.Settings()
            settings.deploy_dir = directory
            settings.release_dir = os.path.join(directory, "releases")
            settings.backup_dir = os.path.join(directory, "backups")
            settings.deployment_dir = os.path.join(directory, "deployments")
            settings.current_manifest = os.path.join(settings.deployment_dir, "current.json")
            settings.config_file = os.path.join(directory, "config.yaml")
            settings.auth_dir = os.path.join(directory, "auths")
            os.mkdir(settings.auth_dir)
            with open(settings.config_file, "w") as handle:
                handle.write("port: 3456\n")
            old = {"Image": "sha256:old", "Config": {"Image": "old"}, "State": {"Status": "running"}}
            new = {"Image": "sha256:new", "Config": {"Image": "local/cliproxyapi:" + commit}, "State": {"Status": "running"}}
            for name in ("require_root", "preflight", "require_deploy_capacity", "validate_staged_compose", "activate_image", "rollback_to_image"):
                stack.enter_context(mock.patch.object(MODULE, name))
            stack.enter_context(mock.patch.object(MODULE, "resolve_commit", side_effect=AssertionError("Git must not be used")))
            stack.enter_context(mock.patch.object(MODULE, "describe_commit", side_effect=AssertionError("Git must not be used")))
            stack.enter_context(mock.patch.object(MODULE, "run", return_value=None))
            stack.enter_context(mock.patch.object(MODULE, "verify_prebuilt_image", return_value={"version": "local-v1"}))
            stack.enter_context(mock.patch.object(MODULE, "container_inspect", side_effect=[old, new]))
            stack.enter_context(mock.patch.object(MODULE, "check_health", return_value=(not fail_health, "health", "HTTP 503" if fail_health else "HTTP 200")))
            if fail_health:
                with self.assertRaises(MODULE.DeploymentError):
                    MODULE.deploy(settings, commit, False, use_prebuilt=True)
            else:
                MODULE.deploy(settings, commit, False, use_prebuilt=True)
            manifest = MODULE.load_manifest(settings.current_manifest)
            self.assertEqual(manifest["status"], "rolled_back" if fail_health else "healthy")
            self.assertEqual(manifest["target_version"], "local-v1")
            self.assertEqual(manifest["config_sha256"], MODULE.sha256_file(settings.config_file))
            MODULE.verify_backup_archive(manifest["backup_archive"])

    def test_prebuilt_failure_does_not_back_up_or_switch(self):
        settings = MODULE.Settings()
        commit = "a" * 40
        with mock.patch.object(MODULE, "require_root"):
            with mock.patch.object(MODULE, "preflight"):
                with mock.patch.object(MODULE, "require_deploy_capacity"):
                    with mock.patch.object(MODULE, "resolve_commit", return_value=commit):
                        with mock.patch.object(
                            MODULE, "verify_prebuilt_image",
                            side_effect=MODULE.DeploymentError("missing image"),
                            create=True,
                        ):
                            with mock.patch.object(MODULE, "run") as run:
                                with mock.patch.object(MODULE, "create_persistent_backup") as backup:
                                    with mock.patch.object(MODULE, "activate_image") as activate:
                                        with self.assertRaises(MODULE.DeploymentError):
                                            MODULE.deploy(settings, commit, False, use_prebuilt=True)
        run.assert_not_called()
        backup.assert_not_called()
        activate.assert_not_called()

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
