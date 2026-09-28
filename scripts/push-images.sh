#!/usr/bin/env bash
# Build the Spillway images for linux/amd64 and push them to a cloud registry.
#
#   ./scripts/push-images.sh gcp asia-northeast3-docker.pkg.dev/<project>/spillway
#       pushes spillway:latest (edge, control, ...), spillway-pg:latest and the
#       web app as guestbook:latest (Cloud Run bursts with it)
#   ./scripts/push-images.sh aws <account>.dkr.ecr.ap-northeast-2.amazonaws.com/spillway
#       pushes the web app as <repo>:latest (Fargate only runs the app)
#
# APP_SOURCE=<dir with Dockerfile> pushes your own app instead of examples/guestbook.
#
# Requires: docker (buildx), and gcloud or aws CLI logged in.
set -euo pipefail
cd "$(dirname "$0")/.."

target=${1:-}
repo=${2:-}
version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)
if [ -z "$target" ] || [ -z "$repo" ]; then
  sed -n '2,9p' "$0"
  exit 1
fi

build() { # build <target> <tag>
  docker buildx build --platform linux/amd64 --build-arg VERSION="$version" --target "$1" -t "$2" --push .
}
app_source=${APP_SOURCE:-examples/guestbook}
build_app() { # build_app <tag>
  docker buildx build --platform linux/amd64 -t "$1" --push "$app_source"
}

case "$target" in
  gcp)
    host=${repo%%/*}
    gcloud auth configure-docker "$host" --quiet
    build app "$repo/spillway:latest"
    build pg "$repo/spillway-pg:latest"
    build_app "$repo/guestbook:latest"
    ;;
  aws)
    host=${repo%%/*}
    region=$(echo "$host" | cut -d. -f4)
    aws ecr get-login-password --region "$region" | docker login --username AWS --password-stdin "$host"
    build_app "$repo:latest"
    ;;
  *)
    echo "unknown target: $target (gcp|aws)" >&2
    exit 1
    ;;
esac
echo "pushed $version to $repo"
