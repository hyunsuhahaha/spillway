#!/usr/bin/env bash
# Spillway single-machine simulation helper.
#   ./scripts/sim.sh up              build and start everything
#   ./scripts/sim.sh down            stop and delete everything (incl. data)
#   ./scripts/sim.sh cut-local       disconnect the local site from the "internet" (= pull the cable)
#   ./scripts/sim.sh restore-local   reconnect the local site
#   ./scripts/sim.sh stop-local-app  crash only the local web app (DB stays up)
#   ./scripts/sim.sh start-local-app bring the local web app back
#   ./scripts/sim.sh status          one-line status from the control plane
#   ./scripts/sim.sh logs [svc]      follow logs
set -euo pipefail
cd "$(dirname "$0")/../deploy/sim"
export MSYS_NO_PATHCONV=1

PROJECT=spillway-sim
WAN=${PROJECT}_wan
LOCAL_CONTAINERS=("${PROJECT}-local-app-1" "${PROJECT}-local-db-1")

burst_rm() {
  local ids
  ids=$(docker ps -aq --filter "label=spillway.burst=spillway-burst" || true)
  if [ -n "$ids" ]; then docker rm -f $ids >/dev/null; fi
}

case "${1:-}" in
  up)
    docker compose build local-app local-db
    docker compose up -d
    echo
    echo "  사용자 URL : http://localhost:${EDGE_PORT:-8080}"
    echo "  관제 대시보드: http://localhost:${CONTROL_PORT:-8090}"
    ;;
  down)
    burst_rm
    docker compose down -v --remove-orphans
    ;;
  cut-local)
    for c in "${LOCAL_CONTAINERS[@]}"; do docker network disconnect "$WAN" "$c" 2>/dev/null || true; done
    echo "로컬 사이트를 인터넷에서 분리했습니다 (케이블 뽑기)."
    ;;
  restore-local)
    for c in "${LOCAL_CONTAINERS[@]}"; do
      alias=${c#${PROJECT}-}; alias=${alias%-1}
      docker network connect --alias "$alias" "$WAN" "$c" 2>/dev/null || true
    done
    echo "로컬 사이트를 다시 연결했습니다."
    ;;
  stop-local-app)  docker stop "${PROJECT}-local-app-1" >/dev/null && echo "로컬 앱을 중지했습니다." ;;
  start-local-app) docker start "${PROJECT}-local-app-1" >/dev/null && echo "로컬 앱을 시작했습니다." ;;
  status)
    curl -s "http://localhost:${CONTROL_PORT:-8090}/api/state" | python -c '
import json,sys
v=json.load(sys.stdin); e=v.get("edge") or {}; s=e.get("stats") or {}; p=v.get("probe") or {}
print("mode=%s p95=%sms rps=%s cloud=%s lost=%s last_rto=%s" % (v["mode"], s.get("p95_ms"), s.get("rps"),
  sum(x["ready"] for x in v["providers"]), p.get("lost"), (p.get("last_outage") or {}).get("seconds")))'
    ;;
  logs) shift; docker compose logs -f --tail=100 "$@" ;;
  *) sed -n '2,11p' "$0"; exit 1 ;;
esac
