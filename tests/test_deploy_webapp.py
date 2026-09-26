"""Offline tests for the one-action web app deployer."""

import importlib.util
import io
import json
import subprocess
import sys
import unittest
from contextlib import redirect_stderr, redirect_stdout
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch


SCRIPT = Path(__file__).resolve().parents[1] / "scripts" / "deploy_webapp.py"
spec = importlib.util.spec_from_file_location("deploy_webapp", SCRIPT)
deploy = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = deploy
spec.loader.exec_module(deploy)


def result(args, stdout="", stderr="", code=0):
    return subprocess.CompletedProcess(args, code, stdout, stderr)


class FakeDocker:
    def __init__(self, previous=False):
        self.calls = []
        self.containers = {}
        if previous:
            self.containers["spillway-deploy-hello"] = "spillway-demo/hello:old"

    def __call__(self, *args, check=True):
        self.calls.append(args)
        action = args[0]
        if action == "inspect":
            name = args[1]
            if name not in self.containers:
                return result(args, stderr="No such object", code=1)
            if ".Config.Labels" in args[-1]:
                return result(args, json.dumps({"spillway.deploy": "hello"}))
            return result(args, self.containers[name])
        if action == "run":
            name = args[args.index("--name") + 1]
            self.containers[name] = args[-1]
            return result(args, "container-id")
        if action == "port":
            return result(args, "127.0.0.1:32768\n")
        if action in ("rm", "stop"):
            name = args[-1]
            if action == "rm":
                self.containers.pop(name, None)
            return result(args)
        return result(args)


def local_args():
    return SimpleNamespace(name="hello", source=SCRIPT.parents[1] / "examples" / "hello",
                           port=8080, host_port=18081, health="/healthz")


class DeployWebAppTests(unittest.TestCase):
    def test_dry_run_has_no_external_commands(self):
        with patch.object(deploy, "command", side_effect=AssertionError("external command")), redirect_stdout(io.StringIO()):
            self.assertEqual(deploy.main(["--dry-run"]), 0)

    def test_rejects_invalid_name_and_missing_project(self):
        with redirect_stderr(io.StringIO()):
            with self.assertRaises(SystemExit):
                deploy.parse(["--name", "../../oops"])
            with self.assertRaises(SystemExit):
                deploy.parse(["--target", "cloudrun"])

    def test_local_deploy_stages_and_promotes(self):
        fake = FakeDocker()
        checks = []
        with patch.object(deploy, "docker", side_effect=fake), patch.object(deploy, "wait_http", side_effect=lambda url: checks.append(url)):
            url = deploy.deploy_local(local_args(), "spillway-demo/hello:new", "release123")
        self.assertEqual(url, "http://127.0.0.1:18081")
        self.assertEqual(fake.containers, {"spillway-deploy-hello": "spillway-demo/hello:new"})
        self.assertEqual(len(checks), 2)
        self.assertIn(":32768/healthz", checks[0])
        self.assertIn(":18081/healthz", checks[1])

    def test_refuses_unmanaged_container_name(self):
        calls = []

        def unmanaged(*args, check=True):
            calls.append(args)
            return result(args, json.dumps({"other.owner": "someone-else"}))

        with patch.object(deploy, "docker", side_effect=unmanaged):
            with self.assertRaisesRegex(deploy.DeployError, "건드리지 않았습니다"):
                deploy.deploy_local(local_args(), "spillway-demo/hello:new", "release123")
        self.assertEqual([call[0] for call in calls], ["inspect"])

    def test_local_failure_restores_previous_image(self):
        fake = FakeDocker(previous=True)
        checks = []

        def health(url):
            checks.append(url)
            if len(checks) == 2:
                raise deploy.DeployError("new version unhealthy")

        with patch.object(deploy, "docker", side_effect=fake), patch.object(deploy, "wait_http", side_effect=health):
            with self.assertRaisesRegex(deploy.DeployError, "이전 버전으로 복구"):
                deploy.deploy_local(local_args(), "spillway-demo/hello:new", "release123")
        self.assertEqual(fake.containers, {"spillway-deploy-hello": "spillway-demo/hello:old"})
        self.assertEqual(len(checks), 3)

    def test_stop_failure_keeps_previous_image(self):
        fake = FakeDocker(previous=True)

        def fail_stop(*args, check=True):
            if args[0] == "stop":
                raise deploy.DeployError("stop failed")
            return fake(*args, check=check)

        with patch.object(deploy, "docker", side_effect=fail_stop), patch.object(deploy, "wait_http"):
            with self.assertRaisesRegex(deploy.DeployError, "이전 버전으로 복구"):
                deploy.deploy_local(local_args(), "spillway-demo/hello:new", "release123")
        self.assertEqual(fake.containers, {"spillway-deploy-hello": "spillway-demo/hello:old"})

    def test_cloudrun_build_push_deploy_and_ready(self):
        args = SimpleNamespace(name="hello", source=local_args().source, port=8080, project="test-project",
                               region="asia-northeast3", registry="spillway", public=True, health="/healthz")
        calls = []

        def fake_command(argv, check=True):
            calls.append(argv)
            if argv[:4] == ["gcloud", "run", "services", "describe"]:
                return result(argv, json.dumps({"status": {"url": "https://demo.example",
                    "conditions": [{"type": "Ready", "status": "True"}]}}))
            return result(argv)

        with patch.object(deploy, "command", side_effect=fake_command), patch.object(deploy, "wait_http") as health:
            self.assertEqual(deploy.deploy_cloudrun(args, "release123"), "https://demo.example")
        self.assertTrue(any(c[:3] == ["docker", "buildx", "build"] and "--push" in c for c in calls))
        self.assertTrue(any(c[:3] == ["gcloud", "run", "deploy"] and "--allow-unauthenticated" in c for c in calls))
        health.assert_called_once_with("https://demo.example/healthz")


if __name__ == "__main__":
    unittest.main()
