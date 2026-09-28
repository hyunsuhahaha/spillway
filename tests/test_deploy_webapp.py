"""Offline tests for the one-action web app deployer."""

import importlib.util
import io
import json
import subprocess
import sys
import tempfile
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


class SpillwayRuntime:
    """A running simulation whose local-app serves spillway-demo/guestbook:old."""

    def __init__(self):
        self.calls = []
        self.compose_calls = []

    def docker(self, *args, check=True):
        self.calls.append(args)
        if args[0] == "inspect" and "{{.State.Running}}" in args:
            return result(args, "true\n")
        if args[0] == "inspect" and "{{.Config.Image}}" in args:
            return result(args, "spillway-demo/guestbook:old\n")
        if args[0] == "port":
            return result(args, "127.0.0.1:32768\n")
        return result(args)

    def compose(self, project, *args):
        self.compose_calls.append((project, args))
        return result(args)


def spillway_args():
    return SimpleNamespace(name="guestbook", source=SCRIPT.parents[1] / "examples" / "guestbook", port=8080,
                           health="/healthz", sim_project="spillway-sim", edge_port=8080, control_port=8090)


class DeploySpillwayTests(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.sim_dir = Path(tmp.name)
        (self.sim_dir / ".env").write_text("WORK_MS=25\nSPILLWAY_APP_IMAGE=spillway-demo/guestbook:old\n", encoding="utf-8")
        self.rt = SpillwayRuntime()

    def run_deploy(self, health=None, wait_local=None):
        with patch.object(deploy, "SIM_DIR", self.sim_dir), \
             patch.object(deploy, "docker", side_effect=self.rt.docker), \
             patch.object(deploy, "compose", side_effect=self.rt.compose), \
             patch.object(deploy, "wait_http", side_effect=health), \
             patch.object(deploy, "wait_local_app", side_effect=wait_local), \
             redirect_stdout(io.StringIO()):
            return deploy.deploy_spillway(spillway_args(), "spillway-demo/guestbook:new", "release123")

    def env(self):
        return (self.sim_dir / ".env").read_text(encoding="utf-8")

    def test_default_source_is_guestbook(self):
        args = deploy.parse(["--target", "spillway"])
        self.assertEqual(args.source.name, "guestbook")
        self.assertEqual(args.name, "guestbook")

    def test_candidate_then_app_and_burst_image_swap(self):
        self.run_deploy()
        run = next(c for c in self.rt.calls if c[0] == "run")
        self.assertIn("spillway-sim_lan", run)
        self.assertTrue(any(a.startswith("DB_URL=") and "@local-db:5432/" in a for a in run))
        self.assertIn(("rm", "-f", "spillway-sim-candidate-release123"), self.rt.calls)
        self.assertEqual(self.rt.compose_calls, [("spillway-sim", ("up", "-d", "--no-deps", "local-app", "control"))])
        self.assertEqual(self.env(), "WORK_MS=25\nSPILLWAY_APP_IMAGE=spillway-demo/guestbook:new\n")

    def test_unhealthy_candidate_changes_nothing(self):
        def unhealthy(url):
            raise deploy.DeployError("candidate unhealthy")

        with self.assertRaisesRegex(deploy.DeployError, "candidate unhealthy"):
            self.run_deploy(health=unhealthy)
        self.assertIn(("rm", "-f", "spillway-sim-candidate-release123"), self.rt.calls)
        self.assertEqual(self.rt.compose_calls, [])
        self.assertIn("SPILLWAY_APP_IMAGE=spillway-demo/guestbook:old", self.env())

    def test_failed_swap_restores_previous_image(self):
        attempts = []

        def local_app(url):
            attempts.append(url)
            if len(attempts) == 1:
                raise deploy.DeployError("edge never saw it healthy")

        with self.assertRaisesRegex(deploy.DeployError, "guestbook:old\\)으로 복구"):
            self.run_deploy(wait_local=local_app)
        self.assertEqual(len(self.rt.compose_calls), 2)
        self.assertIn("SPILLWAY_APP_IMAGE=spillway-demo/guestbook:old", self.env())


if __name__ == "__main__":
    unittest.main()
