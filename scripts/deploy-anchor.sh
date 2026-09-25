#!/usr/bin/env bash
# Copy deploy/cloud (compose.yml + .env) to the GCP anchor VM and start it.
#
#   ./scripts/deploy-anchor.sh <project> [zone]
#
# Requires: gcloud logged in, deploy/cloud/.env filled in, images pushed.
set -euo pipefail
cd "$(dirname "$0")/.."

project=${1:?usage: deploy-anchor.sh <project> [zone]}
zone=${2:-asia-northeast3-a}
vm=spillway-anchor

[ -f deploy/cloud/.env ] || { echo "deploy/cloud/.env is missing (copy .env.example)" >&2; exit 1; }

gcloud compute ssh "$vm" --project "$project" --zone "$zone" --command "sudo mkdir -p /opt/spillway && sudo chown \$USER /opt/spillway"
gcloud compute scp deploy/cloud/compose.yml deploy/cloud/.env "$vm:/opt/spillway/" --project "$project" --zone "$zone"
gcloud compute ssh "$vm" --project "$project" --zone "$zone" --command "
  set -e
  region=\$(grep ^GCP_REGION= /opt/spillway/.env | cut -d= -f2)
  sudo gcloud auth configure-docker \${region:-asia-northeast3}-docker.pkg.dev --quiet >/dev/null 2>&1 || true
  cd /opt/spillway && sudo docker compose pull && sudo docker compose up -d && sudo docker compose ps"
echo
echo "dashboard: http://$(gcloud compute addresses describe spillway-anchor --project "$project" --region "${zone%-*}" --format='value(address)'):8090"
