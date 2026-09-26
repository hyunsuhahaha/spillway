# 실제 배포: 노트북 + GCP (+ AWS)

> ⚠️ 이 경로는 **아직 실제 클라우드에서 실행해 보지 않았다.** Terraform은 `terraform validate`, Compose는 `docker compose config`까지 검증했다. 처음 배포할 때는 아래 순서대로 진행하면서 [문제 해결](#문제-해결)을 참고하자. 동작과 수치 검증은 [단일 PC 시뮬레이션](../README.md#빠른-시작-한-대-pc-시뮬레이션)으로 했다.

## 전체 그림

```
                 사용자 ──▶ http://<앵커 VM 공인 IP>   (edge)
                                   │
   ┌───────────── GCP (asia-northeast3) ─────────────┐        ┌──── 노트북 (로컬 사이트) ────┐
   │ 앵커 VM  e2-small                                │        │ tailscale                   │
   │  tailscale ◀══════════ tailnet ══════════════════╪════════▶  local-app  :8080           │
   │  edge :80 · control :8090 (tailnet 전용)          │        │  local-db   :5432 (주 DB)    │
   │  dbrouter :6432 (VPC 내부)                       │        │                             │
   │  cloud-db (대기 복제본) · probe                   │        │  siteagent  :7000           │
   │                                                  │        └─────────────────────────────┘
   │ Cloud Run  spillway-app  (0→N, Direct VPC egress ─▶ dbrouter :6432)
   └──────────────────────────────────────────────────┘
   (선택) AWS: ALB ─▶ Fargate spillway-app (tailscale 사이드카 → dbrouter)
```

## 준비물

- GCP 프로젝트 (결제 사용 설정), `gcloud` CLI 로그인
- Tailscale 계정, **재사용 가능한 인증 키** (Settings → Keys → Reusable). AWS까지 쓰면 **Ephemeral** 키 하나 더.
- Docker (buildx), Terraform ≥ 1.6
- (선택) AWS 계정, `aws` CLI 로그인

비밀값은 세 사이트가 같아야 한다: `SPILLWAY_TOKEN`, `REPL_PASSWORD`, `APP_DB_PASSWORD`.

```bash
openssl rand -hex 24   # 토큰/비밀번호 생성 예시
```

## 1. 로컬 사이트 (노트북)

```bash
cd deploy/local
cp .env.example .env         # TS_AUTHKEY와 비밀값 입력
docker compose up -d --build
docker compose exec tailscale tailscale ip -4   # → LOCAL_TS_IP
```

Tailscale 관리 콘솔에 `spillway-local`이 보이면 된다.

## 2. GCP 인프라

```bash
cd deploy/gcp
cp terraform.tfvars.example terraform.tfvars   # project, app_db_password
terraform init
terraform apply -target=google_project_service.services -target=google_artifact_registry_repository.spillway
../../scripts/push-images.sh gcp "$(terraform output -raw registry)"
terraform apply
terraform output cloud_env    # deploy/cloud/.env에 붙여넣을 값
```

만들어지는 것:
- 앵커 VM (Docker 설치 스크립트 포함), 고정 공인 IP, 방화벽은 앱 진입점 80만 공개. 관리 화면 8090은 호스트에 포트를 게시하지 않고 tailnet IP에서만 접속, 6432는 VPC 내부만 허용
- Cloud Run `spillway-app`: 최소 0, 최대 10, Direct VPC egress로 앵커 VM의 dbrouter에 접속
- 서비스 계정 두 개: Cloud Run 실행용, 앵커 VM용(`run.developer` — 컨트롤 플레인이 최소 인스턴스를 바꿈)
- Cloud Monitoring API와 앵커 VM의 `monitoring.viewer`: 대시보드의 Cloud Run 실제 인스턴스 관측값에 사용. 지표는 최대 수분 늦으므로 라우팅 준비 판단에는 쓰지 않는다. 지표가 없으면 `–`로 표시하고 0으로 간주하지 않는다.

## 3. 클라우드 사이트 (앵커 VM)

```bash
cd deploy/cloud
cp .env.example .env
# terraform output cloud_env 값 + TS_AUTHKEY + LOCAL_TS_IP + 비밀값 입력
# CLOUD_TS_IP는 VM이 tailnet에 붙은 뒤 채운다(아래)
cd ../..
./scripts/deploy-anchor.sh <project>
gcloud compute ssh spillway-anchor --zone asia-northeast3-a --command "sudo docker compose -f /opt/spillway/compose.yml exec tailscale tailscale ip -4"
# → CLOUD_TS_IP를 .env에 넣고 한 번 더 실행
./scripts/deploy-anchor.sh <project>
```

Tailscale에 연결된 운영자 기기에서 `http://<CLOUD_TS_IP>:8090` 대시보드를 열어 **데이터 보호: 정상**(클라우드 복제본 스트리밍)이면 준비 완료다. `SPILLWAY_TOKEN`이 비었거나 `.env.example`의 예시값이면 클라우드 제어기가 시작을 거부한다. 이전 버전의 공개 HTTP 대시보드를 썼다면 토큰을 새로 발급하고 해당 브라우저의 사이트 데이터를 지운다.

## 4. (선택) AWS Fargate를 두 번째 버스트 대상으로

```bash
cd deploy/aws
cp terraform.tfvars.example terraform.tfvars   # cloud_ts_ip = CLOUD_TS_IP, 에페메럴 키
terraform init
terraform apply -target=aws_ecr_repository.spillway
../../scripts/push-images.sh aws "$(terraform output -raw ecr_repository)"
terraform apply
terraform output cloud_env    # deploy/cloud/.env에 추가, CLOUD_PROVIDERS=cloudrun,ecs
../../scripts/deploy-anchor.sh <project>
```

Fargate 작업은 Tailscale 사이드카(userspace, SOCKS5 `localhost:1055`)로 tailnet에 붙고, 앱은 `DB_SOCKS5`로 앵커 VM의 dbrouter에 접속한다. DB 포트는 인터넷에 열리지 않는다.

## 5. 확인

```bash
curl http://<공인 IP>/api/whoami          # {"site":"local",...}
CLOUD_TS_IP=100.64.0.2  # 실제 앵커 VM의 Tailscale IP로 바꿀 것
curl "http://${CLOUD_TS_IP}:8090/api/state" | head   # tailnet에 연결된 기기에서만
# 공인 IP의 :8090은 접속이 실패해야 한다.
# 인증 없는 POST는 실제 동작이 없는 경로에서도 401이어야 한다:
curl -o /dev/null -w '%{http_code}\n' -X POST "http://${CLOUD_TS_IP}:8090/api/__auth-check"
```

대시보드의 부하 테스트로 버스트를 확인한다. 로컬 노트북의 네트워크를 끊으면 대피가 일어나야 한다.

## 행사장 체크리스트

- [ ] 행사장 와이파이에서 Tailscale이 붙는가? (UDP가 막혀도 DERP로 우회되지만 지연이 늘어난다. **핫스팟 백업**)
- [ ] 노트북 절전 모드 해제, Docker Desktop 자동 시작
- [ ] 클라우드 인프라는 전날 미리 생성 (Cloud Run 첫 배포·VM 부팅은 분 단위)
- [ ] 발표 직전 `페일백`으로 평상 상태 확인, `프로브 초기화`
- [ ] 비용: 앵커 VM(e2-small)은 상시 과금, Cloud Run/Fargate는 버스트 동안만. 끝나면 `terraform destroy`

## 문제 해결

| 증상 | 확인할 것 |
|---|---|
| 클라우드 복제본이 붙지 않음 | 앵커 VM에서 `nc -zv $LOCAL_TS_IP 5432`, 두 사이트 `REPL_PASSWORD` 일치 여부 |
| 대시보드에 `cloudrun: metadata token` 오류 | VM 서비스 계정 스코프(cloud-platform), `roles/run.developer` 부여 여부 |
| Cloud Run 스케일 실패 400 | 어댑터가 서비스 수준 → 리비전 템플릿으로 자동 대체한다. 계속 실패하면 `iam.serviceAccountUser` 확인 |
| Cloud Run 인스턴스가 비정상 | Cloud Run 로그에서 DB 접속 오류 확인 → 방화벽 `spillway-dbrouter-vpc`, VPC egress 설정 |
| 페일백 중 `SWITCHING_BACK` 상태에서 멈춤 | 승격 후 응답 유실·라우터 전환 실패 등으로 DB 역할이 불확실해 요청을 자동 재개하지 않은 상태다. 양쪽 siteagent의 `/status`에서 `role`·`fenced`를 확인하고, 유일한 쓰기 가능 DB와 dbrouter 대상을 확정하기 전에는 `/resume`이나 `/promote`를 수동 호출하지 않는다. |
| Fargate 작업이 DB에 못 붙음 | CloudWatch `/ecs/spillway` tailscale 스트림, 에페메럴 키 유효 기간 |
| 로컬 Docker Desktop에서 tailscale 실패 | `/dev/net/tun` 사용 불가 시 `TS_USERSPACE=true`로 바꾸면 인바운드는 동작하지만, 페일백(로컬 → 클라우드 DB 복제)에는 커널 모드가 필요하다 |
