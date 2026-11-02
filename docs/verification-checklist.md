# 셀프 리뷰 체크리스트

`feat/` PR 본문에 해당 절을 복사해 붙이고 항목마다 체크합니다. 근거(테스트 이름, 로그, 노트 링크)를 함께 적습니다.

## A. 공통 (모든 feat/ PR)

- [ ] 변경이 설정 플래그 뒤에 있고, 플래그 기본값이 원본 동작과 동일하다
- [ ] 플래그 OFF 상태에서 `baseline`과 메시지 흐름이 동일하다 (어떻게 확인했는지 기록)
- [ ] 플래그 OFF: `go build ./... && go vet ./...` 통과
- [ ] 플래그 OFF: `go test ./internal/... ./pkg/...` 통과
- [ ] 플래그 OFF: `go test ./test/...` 통과 (범위 지정 시 `-run` 패턴 기록)
- [ ] 플래그 ON 테스트를 최소 1개 추가했다
- [ ] 커밋 메시지가 `<area>: <요약>` 형식이며 `Flag:` 줄이 있다
- [ ] `git rebase main` 완료, 다른 `feat/` 브랜치 위에 스택되지 않았다
- [ ] 관련 설계 노트가 `docs/notes/` 에 있고 `Refs:` 로 연결했다

## B. 안전성 (Safety)

- [ ] 서로 다른 정직 노드가 같은 seq에 다른 블록을 커밋할 수 있는 경로가 없다
- [ ] 쿼럼(2f+1) 계산이 파이프라인 깊이와 무관하게 유지된다
- [ ] 뷰 체인지 중 in-flight 제안이 누락·중복 커밋되지 않는다
- [ ] `prev_commit_signatures` 체인이 끊기지 않는다 (또는 끊기는 경우 대체 근거를 명시)
- [ ] 설정 트랜잭션 전후로 파이프라인이 비워진다 (config barrier)

## C. 활성성 (Liveness)

- [ ] 리더 장애 시 뷰 체인지가 정상 완료되고 파이프라인이 재시작된다
- [ ] 깊이 W 세마포어가 데드락 없이 해제된다 (타임아웃·뷰 체인지 경로 포함)
- [ ] 적응형 제어기가 배치 크기·깊이를 하한/상한 안에서만 움직인다

## D. Fabric 연동 (pkg/api 또는 fabric 포크 변경 시)

- [ ] `pkg/api` 인터페이스 변경이 Fabric 포크 `orderer/consensus/smartbft/` 에 반영되었다
- [ ] PreviousHash·VerificationSequence 검증 완화가 플래그 OFF 시 원본과 동일하다
- [ ] Fabric 통합 테스트(또는 로컬 네트워크 smoke test) 결과를 기록했다

## E. 실험 재현성 (exp/ 태그를 찍는 경우)

- [ ] `exp-<yyyymmdd>-<topic>` 태그를 push 했다
- [ ] `results/<yyyymmdd>-<topic>/README.md` 에 태그·플래그 값·netem 설정·노드 수를 적었다
