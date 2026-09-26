# AI 활용 기록 — 검증 가능한 범위

이 프로젝트는 AI를 **운영 판단의 블랙박스**로 넣지 않았다. 배포·DB 역할 전환은 재현 가능한 코드와 상태 검사로 결정한다. 대신 개발 과정에서 AI를 다음처럼 사용했다.

| 작업 | AI가 도운 부분 | 사람이 확인할 근거 |
|---|---|---|
| 요구사항 해석 | 킥오프 자료의 “실제 웹앱 배포 시연”, 로컬·클라우드 양쪽 평가, 발표 시간·제출물 조건을 추출해 기존 버스팅/대피 중심 설계와의 차이를 찾음 | 킥오프 PDF 25·27·33·39·41쪽, `docs/submission.md` |
| 실패 경로 검토 | 페일백에서 기존 주 DB 차단보다 새 DB 승격이 앞서는 순서와, 실패 시 요청을 무조건 재개하는 경로를 식별 | `internal/control/control.go`, `TestFailbackCutoverFailures` |
| 테스트 설계 | API 응답이 실패해도 DB 작업은 이미 성공했을 수 있다는 조건으로 fence·promote·router·edge 응답 유실 사례를 생성 | `internal/control/control_test.go`, `go test -race ./...` |
| 배포 UX | 한 명령으로 소스 빌드→후보 헬스체크→실행 URL 확인, 실패 시 이전 이미지 복구 흐름을 설계·구현 | `scripts/deploy_webapp.py`, `tests/test_deploy_webapp.py`, 실제 로컬 HTTP 200 |

AI 산출물은 자동으로 정답이 되지 않는다. 로컬 코드는 단위·E2E 테스트로 확인했고, **Cloud Run 경로는 실계정 부재로 모의 테스트만 했다**. 팀원의 실제 토론·승인·기여 내용은 팀이 별도로 기록해야 하며, 이 문서가 회의록을 대신하지 않는다.
