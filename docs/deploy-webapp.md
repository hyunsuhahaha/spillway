# 한 번의 웹앱 배포

`scripts/deploy_webapp.py`는 로컬 소스의 Dockerfile을 빌드해 Docker 또는 Google Cloud Run에 배포한다. **상태 없는 HTTP 앱**용이다. Spillway의 Postgres 복제·대피 데모는 현재 내장 방명록 앱에서만 동작한다.

## 로컬에서 실제 시연

준비: Python 3.10+, Docker Desktop. 기본 예제는 `examples/hello`이고 컨테이너 포트 8080에서 `/healthz`가 200을 반환한다.

```bash
python scripts/deploy_webapp.py --target local
# LOCAL READY http://127.0.0.1:18081
```

다시 실행하면 새 이미지로 빌드한다. 후보 컨테이너를 임시 포트에서 먼저 확인하고 기존 컨테이너를 교체한다. 최종 컨테이너의 헬스체크가 실패하면 이전 이미지를 재실행한다. **고정 포트 전환 순간의 무중단은 보장하지 않는다.** 앱에 영속 데이터가 있다면 별도 볼륨·백업 설계가 필요하다.

다른 앱: `python scripts/deploy_webapp.py --source PATH --name NAME --port 8080 --health /healthz --host-port 18081`. Dockerfile과 200 응답을 주는 헬스 경로가 필요하다. 로컬 포트는 기본적으로 `127.0.0.1`에만 바인딩된다. 실행 계획만 보려면 `--dry-run`을 붙인다.

정리할 때는 자신이 배포한 컨테이너인지 `docker inspect spillway-deploy-hello --format '{{json .Config.Labels}}'`에서 `spillway.deploy=hello`를 확인한 뒤 `docker rm -f spillway-deploy-hello`를 실행한다. 해당 컨테이너 삭제는 복구할 수 없지만, 예제는 상태가 없어 명령을 다시 실행하면 재생성된다.

## Cloud Run (실계정 검증 대기)

준비: 결제 활성화된 GCP 프로젝트, `gcloud` 로그인, Docker buildx, 해당 리전의 Artifact Registry Docker 저장소 `spillway`, Cloud Run 배포 권한. 이 저장소는 `deploy/gcp` Terraform으로 만들 수 있다. 배포 명령은 고유 태그의 이미지를 빌드·푸시하고 `spillway-demo-<name>` 서비스를 만들거나 새 리비전으로 갱신한다. 기존 하이브리드 런타임 서비스 `spillway-app`은 건드리지 않는다.

```bash
python scripts/deploy_webapp.py --target cloudrun --project YOUR_PROJECT --public
# CLOUD RUN READY https://...
```

`--public`은 심사위원이 URL에 접근해야 할 때만 사용한다. 생략하면 인증이 필요한 서비스로 배포한다. 서비스 Ready 상태를 확인하고, 공개 배포라면 `/healthz`도 검사한다. `--target both`는 클라우드를 먼저 배포하고 로컬을 뒤이어 배포한다. **교차 환경 원자성이나 Cloud Run 자동 롤백은 제공하지 않는다.** 로컬 단계에서 실패하면 클라우드에는 새 버전이 남을 수 있다.

현재 GCP 프로젝트가 없어 실제 Cloud Run 배포와 외부 URL 접근은 검증하지 못했다. 아래 명령은 변경 없이 명령 계획만 출력한다.

```bash
python scripts/deploy_webapp.py --target cloudrun --project YOUR_PROJECT --public --dry-run
```

테스트: `python -m unittest discover -s tests -p 'test_*.py'`. Cloud Run 경로는 CLI 명령과 Ready 응답을 모의 테스트한다. 실제 성공을 주장하려면 별도 프로젝트에서 실행하고 URL·배포 리비전·청구액을 기록해야 한다.
