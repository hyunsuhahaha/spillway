#!/usr/bin/env python3
"""End-to-end verification of Spillway against the single-machine simulation.

Runs every scenario the demo shows, measures it, asserts the guarantees, and
writes a Markdown report:

  1. burst          load above local capacity -> cloud instances -> scale back
  2. evacuation     local site cut off the network -> automatic evacuation
  3. failback       local site returns -> fenced -> failback -> protection restored
  4. migration      planned move to the cloud with zero data loss (RPO 0)
  5. app failure    only the local web app dies -> cloud serves, DB stays local

Usage:  python scripts/e2e.py [--control http://localhost:8090] [--report docs/verification.md]
Requires the simulation to be running (scripts/sim.sh up). Standard library only.
"""
import argparse
import datetime as dt
import json
import os
import shutil
import subprocess
import sys
import time
import urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
CONTROL = "http://localhost:8090"
TOKEN = os.environ.get("SPILLWAY_TOKEN", "")
SIM_PROJECT = os.environ.get("SPILLWAY_SIM_PROJECT", "spillway-sim")
RESULTS = []

# Windows consoles default to a legacy code page; the report text is Korean.
for stream in (sys.stdout, sys.stderr):
    try:
        stream.reconfigure(encoding="utf-8", errors="replace")
    except AttributeError:
        pass


def api(method, path, body=None):
    data = json.dumps(body or {}).encode() if method == "POST" else None
    req = urllib.request.Request(CONTROL + path, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    if TOKEN:
        req.add_header("X-Spillway-Token", TOKEN)
    with urllib.request.urlopen(req, timeout=15) as r:
        return json.loads(r.read() or b"{}")


def state():
    return api("GET", "/api/state")


def sim(cmd):
    # shutil.which: on Windows a bare "bash" may resolve to WSL, which has no docker.
    subprocess.run([shutil.which("bash") or "bash", "scripts/sim.sh", cmd, SIM_PROJECT], cwd=ROOT, check=True, stdout=subprocess.DEVNULL)


def log(msg):
    print(f"[{dt.datetime.now():%H:%M:%S}] {msg}", flush=True)


def wait(desc, pred, timeout, interval=0.5):
    start = time.time()
    last = None
    while time.time() - start < timeout:
        try:
            v = state()
            if pred(v):
                return v, time.time() - start
            last = v
        except Exception as e:  # control plane briefly busy
            last = {"error": str(e)}
        time.sleep(interval)
    mode = last.get("mode") if isinstance(last, dict) else None
    raise AssertionError(f"timeout after {timeout}s waiting for: {desc} (mode={mode})")


def instances(v):
    return sum(p["ready"] for p in v["providers"])


def probe(v):
    return v.get("probe") or {}


def last_op(v):
    return v.get("operation") or {}


def check(name, cond, detail=""):
    RESULTS.append({"check": name, "ok": bool(cond), "detail": detail})
    log(("PASS " if cond else "FAIL ") + name + (f" — {detail}" if detail else ""))
    return cond


def reset_probe():
    api("POST", "/api/probe/reset")
    time.sleep(6)  # let it write and verify a baseline


def settle_probe():
    # Give the verifier time to re-read every acknowledged write.
    time.sleep(8)
    return probe(state())


# --------------------------------------------------------------------- scenarios

def scenario_burst(report):
    log("== 1. 버스트")
    warm = state()["settings"].get("warm_min", 0)
    reset_probe()
    api("POST", "/api/load", {"rps": 220, "seconds": 45})
    t0 = time.time()
    v, t_burst = wait("BURST", lambda v: v["mode"] == "BURST", 40)
    v, _ = wait("cloud instances ready", lambda v: instances(v) >= 2, 40)
    v, t_settle = wait("user p95 back under threshold during load",
                       lambda v: v["edge"]["stats"]["p95_ms"] < v["settings"]["p95_high_ms"] and v["mode"] == "BURST", 60)
    peak = max(instances(state()), 0)
    during = state()
    share_cloud = during["edge"]["groups"].get("cloud", {}).get("count", 0)
    share_local = during["edge"]["groups"].get("local", {}).get("count", 0)
    v, t_back = wait("NORMAL after load", lambda v: v["mode"] == "NORMAL", 120)
    v, _ = wait(f"cloud scaled back to {warm}", lambda v: instances(v) == warm, 60)
    load = probe(v).get("load", {})
    p = settle_probe()
    report["burst"] = {
        "time_to_burst_s": round(t_burst, 1),
        "time_to_recover_p95_s": round(t_burst + t_settle, 1),
        "peak_instances": peak,
        "local_capacity_rps": round(during.get("local_capacity_rps", 0), 1),
        "cloud_share_pct": round(100 * share_cloud / max(1, share_cloud + share_local)),
        "load_sent": load.get("sent"), "load_failed": load.get("failed"),
        "scale_back_after_load_s": round(t_back, 1),
        "probe_lost": p.get("lost"), "probe_failed": p.get("failed"),
    }
    check("버스트: 부하 중 클라우드 인스턴스가 생성됨", peak >= 2, f"최대 {peak}개")
    check("버스트: 부하 요청 실패 0건", load.get("failed") == 0, f"보냄 {load.get('sent')}, 실패 {load.get('failed')}")
    check("버스트: 부하 종료 후 평상 복귀 및 축소", v["mode"] == "NORMAL" and instances(v) == warm, f"{warm}개로 축소")
    check("버스트: 데이터 유실 0건", p.get("lost") == 0)


def scenario_evacuation(report):
    log("== 2. 긴급 대피 (로컬 네트워크 차단)")
    reset_probe()
    sim("cut-local")
    t0 = time.time()
    v, t_start = wait("EVACUATING", lambda v: v["mode"] in ("EVACUATING", "EVACUATED"), 60, 0.25)
    v, t_done = wait("EVACUATED", lambda v: v["mode"] == "EVACUATED" and not last_op(v).get("running"), 120)
    v, _ = wait("writes flowing again", lambda v: probe(v).get("current_gap_s", 99) < 1.0, 60)
    p = settle_probe()
    op = last_op(state())
    outage = (p.get("last_outage") or {})
    report["evacuation"] = {
        "detect_s": round(t_start, 1),
        "switch_s": round(op.get("total_ms", 0) / 1000, 2),
        "client_rto_s": outage.get("seconds"),
        "failed_writes": outage.get("failures"),
        "lost_writes": p.get("lost"),
        "steps": op.get("steps"),
    }
    check("대피: 자동 대피 완료", op.get("ok") is True, f"감지 {t_start:.1f}s + 전환 {op.get('total_ms', 0) / 1000:.1f}s")
    check("대피: 전환 중 사용자 요청 실패 0건 (엣지가 대기시킴)", outage.get("failures", 0) == 0, f"무응답 {outage.get('seconds')}s")
    log(f"   대피 중 확인 응답 받은 쓰기 유실: {p.get('lost')}건 (비동기 복제이므로 0은 보장하지 않음)")

    log("   대피 상태에서 컨트롤 플레인 재시작")
    subprocess.run(["docker", "restart", f"{SIM_PROJECT}-control-1"], check=True, stdout=subprocess.DEVNULL)
    v, t_rec = wait("control plane back in EVACUATED", lambda v: v["mode"] == "EVACUATED" and v["router"]["target"].startswith("cloud-db"), 60)
    report["evacuation"]["control_restart_recovered_s"] = round(t_rec, 1)
    check("재시작: 컨트롤 플레인이 대피 상태를 DB 역할에서 복원", v["mode"] == "EVACUATED" and v["router"]["target"].startswith("cloud-db"), f"{t_rec:.1f}s")
    restored = last_op(v)
    check("재시작: 작업 로그(대피 단계 기록)가 파일에서 복원됨", restored.get("kind") == "evacuate" and len(restored.get("steps") or []) > 0,
          f"이벤트 {len(v.get('events') or [])}건")


def scenario_failback(report):
    log("== 3. 로컬 복귀 + 페일백")
    sim("restore-local")
    v, _ = wait("local returned + fenced", lambda v: v.get("local_returned") and (v["local"].get("status") or {}).get("fenced"), 60)
    check("복귀: 돌아온 옛 주 DB가 자동으로 읽기 전용 격리됨 (스플릿 브레인 방지)", v.get("local_returned") and (v["local"].get("status") or {}).get("fenced"))
    reset_probe()
    api("POST", "/api/failback")
    v, _ = wait("failback finished", lambda v: v["mode"] == "NORMAL" and not last_op(v).get("running") and last_op(v).get("kind") == "failback", 300)
    op = last_op(v)
    v, t_prot = wait("protection restored", lambda v: v["protection"]["state"] == "ok", 180)
    p = settle_probe()
    outage = (p.get("last_outage") or {})
    report["failback"] = {
        "total_s": round(op.get("total_ms", 0) / 1000, 2),
        "client_rto_s": outage.get("seconds", 0),
        "failed_writes": outage.get("failures", 0),
        "lost_writes": p.get("lost"),
        "protection_restored_after_s": round(t_prot, 1),
        "steps": op.get("steps"),
    }
    check("페일백: 로컬이 주 사이트로 복귀", op.get("ok") is True, f"{op.get('total_ms', 0) / 1000:.1f}s")
    check("페일백: 데이터 유실 0건 (RPO 0 전환)", p.get("lost") == 0)
    check("페일백: 클라우드 복제본 재구성 → 보호 정상", v["protection"]["state"] == "ok", f"{t_prot:.1f}s")


def scenario_migration(report):
    log("== 4. 계획된 이사 (무손실)")
    reset_probe()
    api("POST", "/api/migrate")
    v, _ = wait("migrated", lambda v: v["mode"] == "EVACUATED" and not last_op(v).get("running") and last_op(v).get("kind") == "migrate", 120)
    op = last_op(v)
    p = settle_probe()
    outage = (p.get("last_outage") or {})
    report["migration"] = {
        "total_s": round(op.get("total_ms", 0) / 1000, 2),
        "client_rto_s": outage.get("seconds", 0),
        "failed_writes": outage.get("failures", 0),
        "lost_writes": p.get("lost"),
        "steps": op.get("steps"),
    }
    check("이사: 완료", op.get("ok") is True, f"{op.get('total_ms', 0) / 1000:.1f}s")
    check("이사: 데이터 유실 0건 (RPO 0 보장)", p.get("lost") == 0)
    check("이사: 사용자 요청 실패 0건", outage.get("failures", 0) == 0)
    log("   이사 후 원위치 (페일백)")
    api("POST", "/api/failback")
    wait("back to normal", lambda v: v["mode"] == "NORMAL" and not last_op(v).get("running"), 300)
    wait("protection restored", lambda v: v["protection"]["state"] == "ok", 180)


def scenario_app_failure(report):
    log("== 5. 로컬 앱만 장애 (DB는 정상)")
    reset_probe()
    sim("stop-local-app")
    v, t_burst = wait("cloud takes over app traffic", lambda v: v["mode"] == "BURST" and instances(v) >= 1, 60)
    v, _ = wait("writes flowing", lambda v: probe(v).get("current_gap_s", 99) < 1.0, 60)
    db_primary = (v["local"].get("status") or {}).get("role")
    sim("start-local-app")
    v, t_back = wait("NORMAL again", lambda v: v["mode"] == "NORMAL", 120)
    p = settle_probe()
    outage = (p.get("last_outage") or {})
    report["app_failure"] = {
        "takeover_s": round(t_burst, 1),
        "client_rto_s": outage.get("seconds", 0),
        "failed_writes": outage.get("failures", 0),
        "lost_writes": p.get("lost"),
        "db_role_during": db_primary,
        "back_to_normal_s": round(t_back, 1),
    }
    check("앱 장애: 클라우드 앱이 트래픽을 넘겨받음 (DB 승격 없이)", db_primary == "primary", f"{t_burst:.1f}s")
    check("앱 장애: 데이터 유실 0건", p.get("lost") == 0)


# --------------------------------------------------------------------- report

def gap(seconds):
    # The probe only records gaps longer than PROBE_OUTAGE_MIN (1s).
    return f"{seconds}초" if seconds else "1초 미만"


def write_report(path, report, started):
    ok = all(r["ok"] for r in RESULTS)
    L = []
    L.append("# 검증 결과 (시뮬레이션 E2E)\n")
    L.append(f"- 실행 시각: {started:%Y-%m-%d %H:%M:%S}")
    L.append(f"- 환경: 단일 PC Docker 시뮬레이션 (`deploy/sim`), 예열 인스턴스 {report.get('warm_min', 0)}개")
    L.append(f"- 결과: **{'전체 통과' if ok else '실패 있음'}** ({sum(r['ok'] for r in RESULTS)}/{len(RESULTS)})")
    L.append("- 재현: `./scripts/sim.sh up && python scripts/e2e.py`\n")
    L.append("## 요약\n")
    L.append("| 시나리오 | 핵심 수치 |")
    L.append("|---|---|")
    b = report.get("burst", {})
    if b:
        L.append(f"| 버스트 | 부하 시작 {b['time_to_burst_s']}초 뒤 버스트, 최대 인스턴스 {b['peak_instances']}개, 실측 로컬 한계 {b['local_capacity_rps']} rps, 부하 요청 실패 {b['load_failed']}건 |")
    e = report.get("evacuation", {})
    if e:
        L.append(f"| 긴급 대피 | 장애 감지 {e['detect_s']}초 + 전환 {e['switch_s']}초, 사용자 무응답 {e['client_rto_s']}초, 실패 요청 {e['failed_writes']}건, 유실 {e['lost_writes']}건 |")
    f = report.get("failback", {})
    if f:
        L.append(f"| 페일백 | 전체 {f['total_s']}초, 사용자 무응답 {gap(f['client_rto_s'])}, 유실 {f['lost_writes']}건, 보호 재개 {f['protection_restored_after_s']}초 |")
    m = report.get("migration", {})
    if m:
        L.append(f"| 계획된 이사 | 전환 {m['total_s']}초, 사용자 무응답 {m['client_rto_s']}초, 실패 요청 {m['failed_writes']}건, 유실 {m['lost_writes']}건 |")
    a = report.get("app_failure", {})
    if a:
        L.append(f"| 로컬 앱 장애 | {a['takeover_s']}초 만에 클라우드 앱이 인계, DB는 로컬 {a['db_role_during']} 유지, 유실 {a['lost_writes']}건 |")
    L.append("\n## 확인 항목\n")
    L.append("| 결과 | 항목 | 비고 |")
    L.append("|---|---|---|")
    for r in RESULTS:
        L.append(f"| {'✅' if r['ok'] else '❌'} | {r['check']} | {r['detail']} |")
    for key, title in (("evacuation", "긴급 대피"), ("failback", "페일백"), ("migration", "계획된 이사")):
        steps = report.get(key, {}).get("steps") or []
        if steps:
            L.append(f"\n## {title} 단계별 소요 시간\n")
            L.append("| 단계 | ms |")
            L.append("|---|---:|")
            for s in steps:
                L.append(f"| {s['name']} | {s['ms']} |")
    L.append("\n## 원본 데이터\n")
    L.append("```json")
    L.append(json.dumps(report, ensure_ascii=False, indent=2))
    L.append("```")
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w", encoding="utf-8", newline="\n") as fh:
        fh.write("\n".join(L) + "\n")
    log(f"report written to {path}")
    return ok


def main():
    global CONTROL
    ap = argparse.ArgumentParser()
    ap.add_argument("--control", default=CONTROL)
    ap.add_argument("--report", default=os.path.join(ROOT, "docs", "verification.md"))
    ap.add_argument("--only", default="", help="comma list: burst,evacuation,failback,migration,app")
    args = ap.parse_args()
    CONTROL = args.control.rstrip("/")
    started = dt.datetime.now()
    log("waiting for a healthy NORMAL state with streaming replication")
    wait("ready", lambda v: v["mode"] == "NORMAL" and v["protection"]["state"] == "ok" and v["local"]["app_healthy"], 180)
    report = {"warm_min": state()["settings"].get("warm_min", 0)}
    only = set(filter(None, args.only.split(",")))
    steps = [("burst", scenario_burst), ("evacuation", scenario_evacuation), ("failback", scenario_failback),
             ("migration", scenario_migration), ("app", scenario_app_failure)]
    try:
        for name, fn in steps:
            if not only or name in only:
                fn(report)
    except AssertionError as e:
        check("시나리오 진행", False, str(e))
    finally:
        try:
            sim("restore-local")
            sim("start-local-app")
        except Exception:
            pass
    ok = write_report(args.report, report, started)
    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    main()
