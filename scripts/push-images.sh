#!/usr/bin/env bash
# Build the Spillway images for linux/amd64 and push them to a cloud registry.
#
#   ./scripts/push-images.sh gcp asia-northeast3-docker.pkg.dev/<project>/spillway
#       pushes spillway:latest (app, edge, control, ...) and spillway-pg:latest
#   ./scripts/push-images.sh aws <account>.dkr.ecr.ap-northeast-2.amazonaws.com/spillway
#       pushes the app image as <repo>:latest (Fargate only runs the app)
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

case "$target" in
  gcp)
    host=${repo%%/*}
    gcloud auth configure-docker "$host" --quiet
    build app "$repo/spillway:latest"
    build pg "$repo/spillway-pg:latest"
    ;;
  aws)
    host=${repo%%/*}
    region=$(echo "$host" | cut -d. -f4)
    aws ecr get-login-password --region "$region" | docker login --username AWS --password-stdin "$host"
    build app "$repo:latest"
    ;;
  *)
    echo "unknown target: $target (gcp|aws)" >&2
    exit 1
    ;;
esac
echo "pushed $version to $repo"
