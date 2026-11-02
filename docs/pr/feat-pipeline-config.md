# feat/pipeline-config — 파이프라인 설정 플래그 + 테스트 하네스 배관

- 브랜치: `feat/pipeline-config` (base: `main` = ea1d499d, 코드는 baseline 32a628d6 과 동일)
- 커밋:
  - `a575763c` config: add PipelineDepthMax and AdaptiveBatch plumbing with validation
  - `08a25ba1` test: keep the full node configuration across Restart and reconfig

## 무엇을

1. `pkg/types/config.go`
   - `Configuration.PipelineDepthMax uint64` 추가. 리더가 동시에 in-flight 로 둘 수 있는 제안 수의 상한. 0 또는 1 = 원본 순차 동작, 0 은 "미설정" 으로 1 과 동일 취급. 1 초과는 파이프라이닝 활성화 의미이지만 **이 브랜치에서는 구현되지 않음(배관만)**. `DefaultConfig` 에서 `1`.
   - `Configuration.AdaptiveBatch bool` 추가. true 이면 배처가 런타임에 배치 크기·타임아웃을 조정(미구현, 배관만). 기본 `false`.
   - `func (c Configuration) EffectivePipelineDepth() uint64` 추가: `max(1, PipelineDepthMax)`. 이후 브랜치가 0→1 규칙을 반복하지 않도록 한 곳에 둠.
   - `Validate()` 에 아래 규칙 추가 (유효 깊이 ≤ 1 이면 아무것도 바뀌지 않음):
     - 유효 깊이 > 1 이고 `LeaderRotation == true` → `"PipelineDepthMax greater than one requires LeaderRotation to be off (and DecisionsPerLeader to be zero)"`
     - 유효 깊이 > 1 이고 `RequestBatchMaxCount > RequestPoolSize / depth` (⇔ `depth × RequestBatchMaxCount > RequestPoolSize`, 오버플로 없는 나눗셈 형태) → `"PipelineDepthMax times RequestBatchMaxCount is bigger than RequestPoolSize"`
     - `PipelineDepthMax == 0` 은 거부하지 않음 (기존 설정·필드 누락 reconfig 가 계속 통과해야 하므로).
2. 테스트 하네스 `test/`
   - `test_app.go`: `Restart()`/`RestartSync()` 가 재조립할 때 `Setup` 클로저가 캡처한 생성 시점 설정 대신 **실행 중이던 `a.Consensus.Config` 전체**를 사용. 기존의 `DecisionsPerLeader`/`LeaderRotation` 두 필드만 이어받던 코드를 포괄함 (기존 테스트들이 `n.Consensus.Config.X = …; n.Restart()` 패턴을 쓰므로 그대로 동작).
   - `test_app.go`: `newNodeWithConfig(id, network, testName, testDir, cfg types.Configuration)` 신설. `cfg` 를 그대로 쓰고 `SelfID` 만 덮어씀. `newNode(...)` 는 종전과 동일하게 `fastConfig` + `SyncOnStart=true` + 로테이션 인자를 만들어 이 함수에 위임 (`fastConfig` 불변). 노드별 `LeaderHeartbeatTimeout`/`ViewChangeTimeout`/`PipelineDepthMax`/`AdaptiveBatch` 를 생성 시점부터 넣을 수 있고 `Restart()` 뒤에도 유지됨.
   - `reconfig.go`: 테스트용 `Configuration` 미러와 `recconfigToUint`/`recconfigToInt` 양방향 변환기에 `PipelineDepthMax`(int64)·`AdaptiveBatch` 추가 → reconfig 가 두 필드를 0/false 로 조용히 되돌리지 않음.
   - 신규 `test/pipeline_config_test.go`
     - `TestPipelineConfigSurvivesRestart`: 4노드를 `newNodeWithConfig` 로 `PipelineDepthMax=1`, `ViewChangeTimeout=10s`, `LeaderHeartbeatTimeout=10s` 로 시작 → 요청 3건 전달·전 노드 동일 확인 → 팔로워 1개 `Restart()` → `Consensus.Config.PipelineDepthMax==1`, `EffectivePipelineDepth()==1`, `AdaptiveBatch==false`, 두 타이머 10s 유지 확인 → 요청 1건 추가 전달 확인. `t.Parallel()`, 약 1.3s.
     - `TestReconfigCarriesPipelineFields`: 변환기 왕복에서 `PipelineDepthMax=3`, `AdaptiveBatch=true` 보존 확인.
3. `pkg/consensus`, `internal/bft` 는 **수정하지 않음**. 새 필드는 어떤 컴포넌트에도 복사되지 않으며 `Validate` 외에는 읽는 곳이 없다.

## 왜

- `docs/notes/00-summary.md` 3절 순서 2번: 이후 `feat/adaptive-batch`, `feat/pipeline-window`, `feat/state-wal-window`, … 가 공통으로 쓸 플래그와 하네스 배관을 작은 브랜치로 먼저 넣어, 각 기능 브랜치 diff 가 프로토콜 변경만 보여주도록 한다.
- `PipelineDepthMax > 1 ⇒ LeaderRotation == false` (`docs/notes/prev-commit-dependency.md` 선택지 (c)): 직전 커밋 서명 체인(`view.go` `Propose`/`GetMetadata`/`verifyPrevCommitSignatures`/`verifyBlacklist`)은 깊이 1 체크포인트(`Checkpoint.Get`)에 묶여 있어 W>1 이면 팔로워마다 체크포인트가 N-1/N 으로 갈려 검증이 비결정적으로 실패(`Complain+Sync+ABORT`) 할 수 있다. 반면 `DecisionsPerLeader == 0` 이면 생성·검증이 전부 생략되므로 설정 규칙 한 줄로 이 의존성을 통째로 우회한다. `config.go` 는 이미 `LeaderRotation=false ⇔ DecisionsPerLeader=0` 을 강제한다. Fabric 통합도 로테이션을 강제로 끄므로 비교 실험의 공정성에 영향 없음.
- `W × RequestBatchMaxCount ≤ RequestPoolSize` (`docs/notes/batcher.md`): 풀 용량은 세마포어이고 `NextRequests` 는 비파괴적이므로, in-flight 배치 W 개만으로 풀이 차면 `Submit()` 이 `RequestPoolSubmitTimeout` 까지 블록한다. 기본값 100×4 = 400 이 딱 맞다.
- 0→1 정규화: reconfig 경로와 Fabric 설정 블록에서 필드가 빠지면 0 이 들어온다(`docs/notes/test-harness.md` 열린 문제 3). 거부하면 기존 설정이 깨지므로 "기본값 = 원본 동작" 원칙에 맞게 1 로 취급한다.
- 하네스 `Restart()` 는 `DecisionsPerLeader/LeaderRotation` 만 이어받아 새 플래그가 재시작 후 0 으로 초기화되었고(`docs/notes/test-harness.md`), reconfig 변환기도 필드를 떨어뜨렸다. 이대로면 이후 브랜치의 재시작·reconfig 테스트에서 플래그가 조용히 꺼진다. 또 뷰 체인지가 들어가는 테스트는 `fastConfig` 의 1분 타이머 두 개에 묶여 테스트당 2분이 기본이 되므로, 새 테스트가 처음부터 짧은 타이머를 넣을 수 있는 생성자가 필요했다.

## 안전성에 미치는 영향

- 없음. 프로토콜 코드(`internal/bft`, `pkg/consensus`, `smartbftprotos`, `pkg/api`) 는 한 줄도 바뀌지 않았다. 새 필드는 `Validate()` 에서만 읽힌다.
- 기본값(`PipelineDepthMax=1`, `AdaptiveBatch=false`) 과 미설정(`0`) 모두에서 `Validate()` 의 새 분기는 실행되지 않으므로(`depth > 1` 가드), 기존 설정의 수용/거부 결과와 에러 문자열이 그대로다. `TestConfigurationValidate` 의 기존 케이스 전부 무수정 통과.
- 하네스 변경은 테스트 전용 패키지(`test/`) 에 한정. `Restart()` 의 설정 carry-over 를 두 필드에서 전체 구조체로 넓힌 것은, 기존 테스트가 재시작 전에 바꾸는 필드가 그 두 개뿐이고 reconfig 뒤 `Restart()` 를 호출하는 테스트가 없으므로 기존 테스트의 동작을 바꾸지 않는다(전체 `go test ./test` 통과로 확인).
- 로그 문자열 변경 없음 (테스트가 훅을 거는 15개 로그 라인 그대로).

## 플래그

- 플래그: `PipelineDepthMax` / `AdaptiveBatch`
- 기본값(OFF)에서 원본 동작과 동일함: **예** — 두 필드는 `Validate()` 외 어디서도 읽히지 않고, `Validate()` 의 새 분기는 유효 깊이 > 1 에서만 실행된다. `DefaultConfig.PipelineDepthMax = 1`, `AdaptiveBatch = false`; `fastConfig` 는 두 필드를 명시하지 않아 0/false (= 1/false 취급).

## 테스트

실행 환경: Apple Silicon (darwin/arm64), Go 1.27.3, worktree `/Users/liam/Desktop/smartbft/wt-feat-config`. 모두 커밋 `08a25ba1` 기준.

- [x] `go build ./... && go vet ./...` — 통과. `gofmt -l pkg test` 출력 없음. (golangci-lint 는 로컬 미설치로 미실행)
- [x] `go test ./internal/... ./pkg/... -count=1` — 전부 `ok`, wall-clock **8.7s** (`internal/bft` 5.6s, `pkg/wal` 7.9s, `pkg/types` 0.5s, `pkg/api` 0.4s)
- [x] `go test ./test -count=1 -run 'TestBasic$|TestBasicReconfig|TestRestartKeepsViewAndInFlightProposal$|TestPipelineConfigSurvivesRestart|TestReconfigCarriesPipelineFields' -v` — 5개 전부 PASS, 패키지 2.3s (TestBasic 0.99s, TestRestartKeepsViewAndInFlightProposal 1.20s, TestPipelineConfigSurvivesRestart 1.33s, TestBasicReconfig 2.07s, TestReconfigCarriesPipelineFields 0.00s)
- [x] `go test ./test -count=1` (전체 통합 스위트) — `ok github.com/hyperledger/SmartBFT/test 128.522s`, wall-clock **2m09s** (real 129.1s, user 6.5s, sys 3.7s — 하트비트·뷰체인지 1분 타이머 2개가 하한, test-harness.md 의 139s 측정과 일치), 신규 테스트 2개 포함 전부 PASS
- [x] 플래그 ON 테스트 추가:
  - 단위 (`pkg/types/config_test.go`): `TestConfigurationValidatePipelineDepth` — 기본 설정 통과 / 깊이 0·1 + 로테이션 ON 통과 / 깊이 2 + 로테이션 ON 실패(에러 문자열 일치) / 깊이 2 + 로테이션 OFF + 풀 400 통과 / 깊이 4×100 = 400 통과 / 깊이 4×100 > 399 실패 / 깊이 `^uint64(0)` 오버플로 없이 실패 / `AdaptiveBatch=true` 가 검증에 영향 없음. `TestConfigurationEffectivePipelineDepth` — 0→1, 1→1, 2→2, 7→7, DefaultConfig→1.
  - 하네스 (`test/pipeline_config_test.go`): `TestPipelineConfigSurvivesRestart` (명시적 `PipelineDepthMax=1` + 10s 타이머, 재시작 후 유지), `TestReconfigCarriesPipelineFields` (변환기 왕복). 이 브랜치의 플래그는 배관만이므로 "ON" 의 의미는 "필드를 명시적으로 설정하고 재시작·reconfig 를 거쳐 값이 살아남는가" 이다.

## 셀프 리뷰 체크리스트

### A. 공통 (모든 feat/ PR)

- [x] 변경이 설정 플래그 뒤에 있고, 플래그 기본값이 원본 동작과 동일하다 — `DefaultConfig.PipelineDepthMax=1`, `AdaptiveBatch=false`; 0 은 1 로 정규화. 두 필드를 읽는 곳은 `Validate()` 의 `depth > 1` 분기만.
- [x] 플래그 OFF 상태에서 `baseline`과 메시지 흐름이 동일하다 — `internal/bft`, `pkg/consensus`, `smartbftprotos` 무수정(`git diff ea1d499d --stat`: `pkg/types/config.go`, `pkg/types/config_test.go`, `test/reconfig.go`, `test/test_app.go`, `test/pipeline_config_test.go` 만). 동일 바이너리·동일 설정이므로 메시지 흐름은 정의상 동일. 전체 `go test ./test` 통과로 하네스 변경의 무해성 확인.
- [x] 플래그 OFF: `go build ./... && go vet ./...` 통과
- [x] 플래그 OFF: `go test ./internal/... ./pkg/...` 통과 (8.7s)
- [x] 플래그 OFF: `go test ./test/...` 통과 — 전체 스위트 1회 (`-count=1`, wall-clock 2m09s, 전부 PASS) + `-run 'TestBasic$|TestBasicReconfig|TestRestartKeepsViewAndInFlightProposal$|TestPipelineConfigSurvivesRestart|TestReconfigCarriesPipelineFields'`
- [x] 플래그 ON 테스트를 최소 1개 추가했다 — `TestConfigurationValidatePipelineDepth`, `TestConfigurationEffectivePipelineDepth`, `TestPipelineConfigSurvivesRestart`, `TestReconfigCarriesPipelineFields`
- [x] 커밋 메시지가 `<area>: <요약>` 형식이며 `Flag:` 줄이 있다 — `config:` a575763c, `test:` 08a25ba1, 둘 다 `Flag:`/`Refs:` 줄 포함, attribution trailer 없음
- [x] `git rebase main` 완료, 다른 `feat/` 브랜치 위에 스택되지 않았다 — base 가 `main`(ea1d499d) 그 자체. `feat/metrics-round-latency` 위에 스택되지 않음 (정책 3절의 "선행 1" 은 병합 순서 요건이며 코드 의존성은 없음)
- [x] 관련 설계 노트가 `docs/notes/` 에 있고 `Refs:` 로 연결했다 — `prev-commit-dependency.md`, `batcher.md`, `test-harness.md`

### B~E

- B(안전성)·C(활성성): 프로토콜 변경 없음 → 해당 없음. D(Fabric): `pkg/api` 무변경 → 해당 없음. E(exp 태그): 해당 없음.

## Refs

- docs/notes/00-summary.md (2·3절, feat 2 행)
- docs/notes/prev-commit-dependency.md (선택지 (c): `PipelineDepthMax>1 ⇒ !LeaderRotation`)
- docs/notes/batcher.md (`W × RequestBatchMaxCount ≤ RequestPoolSize`)
- docs/notes/test-harness.md (`Restart()` carry-over, reconfig 변환기, 타이머 1분 하한)
- docs/branch-policy.md (1-1, 2, 4, 8절)
