#!/usr/bin/env python3
"""One action: build a local web app and deploy it.

  --target local     standalone container on 127.0.0.1 (stateless apps)
  --target cloudrun  standalone Cloud Run service (stateless apps)
  --target spillway  into the Spillway hybrid runtime: the image becomes the
                     local app AND the burst image, behind the edge, with its
                     database reached through the switchable DB router.
                     The app must follow the app contract (README "앱 계약").
"""

import argparse
import datetime as dt
import json
import os
import re
import shutil
import subprocess
import sys
import time
import urllib.error
import urllib.request
import uuid
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
NAME = re.compile(r"^[a-z](?:[a-z0-9-]{0,28}[a-z0-9])?$")
for stream in (sys.stdout, sys.stderr):
    if hasattr(stream, "reconfigure"):
        stream.reconfigure(encoding="utf-8", errors="replace")


class DeployError(Exception):
    pass


def command(argv, *, check=True, cwd=None):
    print("+", " ".join(argv), flush=True)
    try:
        result = subprocess.run(argv, cwd=cwd, text=True, encoding="utf-8", errors="replace", capture_output=True, check=False)
    except FileNotFoundError as exc:
        raise DeployError(f"필요한 도구를 찾을 수 없습니다: {argv[0]}") from exc
    if check and result.returncode:
        raise DeployError(f"명령 실패 ({result.returncode}): {' '.join(argv)}\n{result.stderr.strip()}")
    return result


def docker(*args, check=True):
    return command(["docker", *args], check=check)


def wait_http(url, timeout=45):
    deadline = time.monotonic() + timeout
    last = "응답 없음"
    while time.monotonic() < deadline:
        try:
            with urllib.request.urlopen(url, timeout=2) as response:
                if response.status == 200:
                    return
                last = f"HTTP {response.status}"
        except (urllib.error.URLError, TimeoutError) as exc:
            last = str(exc)
        time.sleep(0.5)
    raise DeployError(f"헬스체크 실패: {url} ({last})")


def inspect_managed(name, app_name):
    result = docker("inspect", name, "--format", "{{json .Config.Labels}}", check=False)
    if result.returncode:
        if "no such object" in result.stderr.lower() or "no such container" in result.stderr.lower():
            return None
        raise DeployError(f"기존 컨테이너 확인 실패: {result.stderr.strip()}")
    labels = json.loads(result.stdout)
    if not isinstance(labels, dict) or labels.get("spillway.deploy") != app_name:
        raise DeployError(f"{name}은 Spillway가 관리하는 컨테이너가 아닙니다. 건드리지 않았습니다.")
    image = docker("inspect", name, "--format", "{{.Config.Image}}").stdout.strip()
    if not image:
        raise DeployError(f"{name}의 이전 이미지를 확인할 수 없습니다")
    return image


def run_container(name, app_name, release, image, container_port, host_port=None):
    published = f"127.0.0.1:{host_port}:{container_port}" if host_port else f"127.0.0.1::{container_port}"
    docker("run", "-d", "--name", name, "--restart", "unless-stopped" if host_port else "no",
           "--label", f"spillway.deploy={app_name}", "--label", f"spillway.release={release}",
           "-p", published, image)


def remove_container(name):
    docker("rm", "-f", name, check=False)


def remove_managed_container(name, app_name):
    if inspect_managed(name, app_name):
        remove_container(name)


def deploy_local(args, image, release):
    stable = f"spillway-deploy-{args.name}"
    candidate = f"{stable}-candidate-{release}"
    old_image = inspect_managed(stable, args.name)
    docker("build", "-t", image, "-f", str(args.source / "Dockerfile"), str(args.source))
    candidate_started = False
    try:
        run_container(candidate, args.name, release, image, args.port)
        candidate_started = True
        port_output = docker("port", candidate, f"{args.port}/tcp").stdout.strip().splitlines()
        if not port_output:
            raise DeployError("후보 컨테이너의 임시 포트를 확인할 수 없습니다")
        candidate_port = int(port_output[0].rsplit(":", 1)[1])
        wait_http(f"http://127.0.0.1:{candidate_port}{args.health}")

        try:
            if old_image:
                docker("stop", stable)
                docker("rm", stable)
            run_container(stable, args.name, release, image, args.port, args.host_port)
            wait_http(f"http://127.0.0.1:{args.host_port}{args.health}")
        except (DeployError, ValueError) as exc:
            if old_image:
                try:
                    current = inspect_managed(stable, args.name)
                    if current == old_image:
                        docker("start", stable, check=False)
                    else:
                        if current:
                            remove_managed_container(stable, args.name)
                        run_container(stable, args.name, "rollback", old_image, args.port, args.host_port)
                    wait_http(f"http://127.0.0.1:{args.host_port}{args.health}")
                except DeployError as rollback_error:
                    raise DeployError(f"새 배포 실패: {exc}; 이전 버전 복구도 실패: {rollback_error}") from exc
                raise DeployError(f"새 배포 실패: {exc}; 이전 버전으로 복구했습니다") from exc
            remove_managed_container(stable, args.name)
            raise
    finally:
        if candidate_started:
            remove_managed_container(candidate, args.name)
    url = f"http://127.0.0.1:{args.host_port}"
    print(f"LOCAL READY {url}  image={image}")
    return url


SIM_DIR = ROOT / "deploy" / "sim"
APP_IMAGE_VAR = "SPILLWAY_APP_IMAGE"


def sim_env(project):
    """Environment for docker compose, matching scripts/sim.sh."""
    prefix = "spillway-burst" if project == "spillway-sim" else f"{project}-burst"
    env = {**os.environ, "SPILLWAY_SIM_PROJECT": project, "SPILLWAY_SIM_BURST_PREFIX": prefix}
    env.pop(APP_IMAGE_VAR, None)  # a shell export would override .env, the source of truth
    return env


def compose(project, *args):
    argv = ["docker", "compose", *args]
    print("+", " ".join(argv), flush=True)
    result = subprocess.run(argv, cwd=SIM_DIR, env=sim_env(project), text=True, encoding="utf-8", errors="replace", capture_output=True, check=False)
    if result.returncode:
        raise DeployError(f"명령 실패 ({result.returncode}): {' '.join(argv)}\n{result.stderr.strip()}")
    return result


def set_app_image(env_file, image):
    """Record the app image in deploy/sim/.env so a later `sim.sh up` keeps it."""
    lines = env_file.read_text(encoding="utf-8").splitlines() if env_file.exists() else []
    lines = [line for line in lines if not line.startswith(APP_IMAGE_VAR + "=")]
    lines.append(f"{APP_IMAGE_VAR}={image}")
    env_file.write_text("\n".join(lines) + "\n", encoding="utf-8")


def container_running(name):
    result = docker("inspect", name, "--format", "{{.State.Running}}", check=False)
    return result.returncode == 0 and result.stdout.strip() == "true"


def wait_local_app(control_url, timeout=90):
    """Wait until the edge sees the local app healthy (control /api/state)."""
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            with urllib.request.urlopen(control_url + "/api/state", timeout=2) as response:
                if json.load(response).get("local", {}).get("app_healthy"):
                    return
        except (urllib.error.URLError, TimeoutError, ValueError):
            pass
        time.sleep(1)
    raise DeployError("엣지가 로컬 앱을 정상으로 인식하지 못했습니다 (GET /healthz 확인)")


def swap_app(project, env_file, image, control_url):
    # local-app serves the new image; control is recreated so new burst
    # instances use it too (it restores its mode from the DB roles).
    set_app_image(env_file, image)
    compose(project, "up", "-d", "--no-deps", "local-app", "control")
    wait_local_app(control_url)


def deploy_spillway(args, image, release):
    project = args.sim_project
    env_file = SIM_DIR / ".env"
    control_url = f"http://127.0.0.1:{args.control_port}"
    docker("build", "-t", image, "-f", str(args.source / "Dockerfile"), str(args.source))

    if not container_running(f"{project}-local-db-1"):
        # First deploy: start the whole runtime with this app.
        set_app_image(env_file, image)
        # shutil.which: on Windows a bare "bash" may resolve to WSL, not Git Bash.
        command([shutil.which("bash") or "bash", "scripts/sim.sh", "up", project], cwd=ROOT)
        wait_local_app(control_url)
    else:
        # Candidate: same image and same DB as local-app, outside the edge.
        candidate = f"{project}-candidate-{release}"
        db_password = os.environ.get("APP_DB_PASSWORD", "spillway-secret")
        docker("run", "-d", "--name", candidate, "--network", f"{project}_lan",
               "--label", f"spillway.deploy={args.name}",
               "-e", f"PORT={args.port}", "-e", "SITE=local", "-e", "INSTANCE=candidate",
               "-e", f"DB_URL=postgres://spillway:{db_password}@local-db:5432/spillway?sslmode=disable",
               "-p", f"127.0.0.1::{args.port}", image)
        try:
            port_output = docker("port", candidate, f"{args.port}/tcp").stdout.strip().splitlines()
            if not port_output:
                raise DeployError("후보 컨테이너의 임시 포트를 확인할 수 없습니다")
            wait_http(f"http://127.0.0.1:{int(port_output[0].rsplit(':', 1)[1])}{args.health}")
        finally:
            remove_container(candidate)

        old_image = docker("inspect", f"{project}-local-app-1", "--format", "{{.Config.Image}}").stdout.strip()
        try:
            swap_app(project, env_file, image, control_url)
        except DeployError as exc:
            if not old_image:
                raise
            try:
                swap_app(project, env_file, old_image, control_url)
            except DeployError as rollback_error:
                raise DeployError(f"새 배포 실패: {exc}; 이전 버전 복구도 실패: {rollback_error}") from exc
            raise DeployError(f"새 배포 실패: {exc}; 이전 버전({old_image})으로 복구했습니다") from exc

    print(f"SPILLWAY READY http://localhost:{args.edge_port}  dashboard={control_url}  image={image}")
    print("  이 이미지가 로컬 앱이자 버스트 인스턴스입니다. 이미 떠 있던 버스트 인스턴스는 축소될 때까지 이전 버전일 수 있습니다.")
    return image


def cloud_image(args, release):
    return f"{args.region}-docker.pkg.dev/{args.project}/{args.registry}/{args.name}:{release}"


def deploy_cloudrun(args, release):
    if not args.project:
        raise DeployError("Cloud Run 배포에는 --project GCP_PROJECT가 필요합니다")
    service = f"spillway-demo-{args.name}"
    image = cloud_image(args, release)
    gcloud = lambda *parts: command(["gcloud", *parts])
    gcloud("artifacts", "repositories", "describe", args.registry,
           "--location", args.region, "--project", args.project)
    gcloud("auth", "configure-docker", f"{args.region}-docker.pkg.dev", "--quiet")
    docker("buildx", "build", "--platform", "linux/amd64", "--push", "-t", image,
           "-f", str(args.source / "Dockerfile"), str(args.source))
    access = "--allow-unauthenticated" if args.public else "--no-allow-unauthenticated"
    gcloud("run", "deploy", service, "--image", image, "--port", str(args.port),
           "--region", args.region, "--project", args.project, access, "--quiet")
    detail = gcloud("run", "services", "describe", service, "--region", args.region,
                    "--project", args.project, "--format=json")
    status = json.loads(detail.stdout).get("status", {})
    ready = any(c.get("type") == "Ready" and c.get("status") == "True"
                for c in status.get("conditions", []))
    url = status.get("url", "")
    if not ready or not url:
        raise DeployError("Cloud Run Ready 조건 또는 공개 URL을 확인하지 못했습니다")
    if args.public:
        wait_http(url.rstrip("/") + args.health)
    print(f"CLOUD RUN READY {url}  image={image}")
    return url


def parse(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", type=Path, default=None,
                        help="app directory with a Dockerfile (default: examples/hello, "
                             "or examples/guestbook for --target spillway)")
    parser.add_argument("--target", choices=("local", "cloudrun", "both", "spillway"), default="local")
    parser.add_argument("--name", default=None, help="lowercase deployment name (default: source directory name)")
    parser.add_argument("--port", type=int, default=8080, help="container HTTP port")
    parser.add_argument("--host-port", type=int, default=18081, help="local stable port")
    parser.add_argument("--health", default="/healthz", help="HTTP 200 readiness path")
    parser.add_argument("--project", default="", help="GCP project ID for Cloud Run")
    parser.add_argument("--region", default="asia-northeast3")
    parser.add_argument("--registry", default="spillway", help="existing Artifact Registry repository")
    parser.add_argument("--public", action="store_true", help="allow unauthenticated Cloud Run access")
    parser.add_argument("--sim-project", default=os.environ.get("SPILLWAY_SIM_PROJECT", "spillway-sim"),
                        help="Spillway runtime (compose project) for --target spillway")
    parser.add_argument("--edge-port", type=int, default=int(os.environ.get("EDGE_PORT", "8080")))
    parser.add_argument("--control-port", type=int, default=int(os.environ.get("CONTROL_PORT", "8090")))
    parser.add_argument("--dry-run", action="store_true", help="show the deployment plan without changes")
    args = parser.parse_args(argv)
    if args.source is None:
        args.source = ROOT / "examples" / ("guestbook" if args.target == "spillway" else "hello")
    args.source = args.source.resolve()
    args.name = args.name or args.source.name.lower()
    if not NAME.fullmatch(args.name):
        parser.error("--name must be lowercase letters, digits or hyphens (max 30)")
    if not (1 <= args.port <= 65535 and 1 <= args.host_port <= 65535):
        parser.error("ports must be between 1 and 65535")
    if not args.health.startswith("/") or args.health.startswith("//"):
        parser.error("--health must be an absolute URL path")
    if not (args.source / "Dockerfile").is_file():
        parser.error(f"Dockerfile not found in {args.source}")
    if args.target in ("cloudrun", "both") and not args.project:
        parser.error("--project is required for Cloud Run")
    return args


def main(argv=None):
    args = parse(argv)
    release = dt.datetime.now(dt.timezone.utc).strftime("%Y%m%d%H%M%S") + "-" + uuid.uuid4().hex[:6]
    local_image = f"spillway-demo/{args.name}:{release}"
    if args.dry_run:
        print(f"source={args.source}\nrelease={release}\ntarget={args.target}")
        if args.target == "spillway":
            print(f"SPILLWAY: Docker build {local_image} -> candidate health check on {args.sim_project} local DB "
                  f"-> local app + burst image -> edge http://localhost:{args.edge_port} -> rollback on failure")
        if args.target in ("local", "both"):
            print(f"LOCAL: Docker build {local_image} -> candidate health check -> 127.0.0.1:{args.host_port} -> rollback on failure")
        if args.target in ("cloudrun", "both"):
            print(f"CLOUD RUN: build/push {cloud_image(args, release)} -> spillway-demo-{args.name} -> Ready check")
        return 0
    try:
        if args.target == "spillway":
            deploy_spillway(args, local_image, release)
            return 0
        # Cloud first keeps the existing local version serving if cloud setup
        # is unavailable. Multi-target deployment is not an atomic transaction.
        if args.target in ("cloudrun", "both"):
            deploy_cloudrun(args, release)
        if args.target in ("local", "both"):
            deploy_local(args, local_image, release)
    except (DeployError, ValueError, json.JSONDecodeError) as exc:
        print(f"DEPLOY FAILED: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
