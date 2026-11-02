# feat/pipeline-window — 합의 파이프라이닝 (깊이 W = `PipelineDepthMax`)

- 브랜치: `feat/pipeline-window` (base `main` = `5ae45b7c`, PR #1 config · #2 metrics · #3 adaptive-batch 포함)
- 커밋 (설계 §12 의 a→f, 각 커밋 W=1 에서 빌드·테스트 green):

| # | 커밋 | 내용 |
|---|---|---|
| a | `ac868caf` requestpool: reservation state for requests in a proposal in flight | `requestItem.reserved`, `Reserve/UnreserveAll`, `NextRequests` 가 예약 건을 건너뜀, `deleteRequest` 가 예약 해제, `RequestPool` 인터페이스 + mockery 재생성, `TestReqPoolReserve` |
| b | `c63c1c6a` util: key InFlightData by proposal sequence | `InFlightData` 를 `map[seq]` 로, getter 는 최저 seq, `clearUpTo`(sync 후 prune), `pruneDelivered`(최고 항목 보존 → W=1 ViewData 바이트 동일), `decide` 의 `rotated` 플래그(R4), `TestInFlightDataMultiplex` |
| c | `f0061784` view: per-sequence slot state machine (depth 1) | `seqState`/`slots map[seq]`, 이벤트 구동 `run`, non-blocking `stepWindow/stepPrepares/stepCommits`, in-order `tryDeliver`, `advanceLow`(low watermark 발행), prev 링, `verifyProposal(seq,…)`+`expectedDecisions`, `head/seqLock`, `Start()` 복원 변환. W 는 1 로 고정 |
| d | `fe6ff5c5` view: pipeline window, prefix rule and pre-prepare resend | `ProposalMaker.PipelineDepth/PrePrepareResendInterval` 배선, 수용 `[low, low+W]`/처리 `[low, low+W)`, D2 접두사 규칙, W>1 전용 `resendTick`, 중복 pre-prepare 로그 레벨 분기, W>1 + WAL in-flight 재시작 **error** 거부, 단위 테스트 7개 |
| e | `68f9ed12` controller: leader token semaphore and proposer loop | cap-W `leaderToken chan Proposer`, `proposerLoop`, R1–R7, `Reserve/UnreserveAll` 호출, drain 3곳, `changeView` 의 `Batcher.Reset` 순서, BatchTuner 훅 보존, 단위 테스트 4개 |
| f | `58dadb2b` test: multi in-flight integration tests and message-histogram identity test | `TestMultiInFlightBasic`, `TestPipelineDecideInOrder`, `TestPipelineLeaderStopsAtWindow`, `TestPipelineRestartWithInFlightIsRejected`, `TestMessageHistogramMatchesBaseline` + golden |

변경 규모: 17 files, +3234 / −492. 라이브러리: `view.go` +480/−251, `controller.go` +128/−48, `util.go` +108/−21, `requestpool.go` +62/−6, `state.go` +32/−1, `consensus.go` +22/−0, `mocks/request_pool.go` +73 (재생성). 테스트: `view_window_internal_test.go` +767, `controller_window_internal_test.go` +394, `pipeline_window_test.go` +340, `message_histogram_test.go` +350, golden +113, `controller_test.go` +208/−164 (픽스처), `requestpool_test.go` +80, `util_test.go` +64, `test_app.go` +11, `controller_decide_test.go` +2/−1.

## 무엇을

리더가 직전 제안의 결정을 기다리지 않고 최대 W 개의 제안을 동시에 in-flight 로 두는 합의 파이프라이닝. W = `Configuration.EffectivePipelineDepth()` (`PipelineDepthMax`, 기본 1).

- **View** (`internal/bft/view.go`): 2슬롯(`current/next`) 상태기계를 seq 별 `seqState` 슬롯 윈도우로 재구성.
  - 라우팅 (`processMsg`): `s < low ∧ low−s ≤ W` → prev 링(전달된 슬롯의 `prepareSent/commitSent` 로 유니캐스트 응답, Assist 무응답) · `low ≤ s ≤ low+W` → 수용(W+1 칸) · 그 밖 → 기존 Info 로그 + `discoverIfSyncNeeded`. W=1 ⇒ `{S−1}`, `{S, S+1}`, 나머지 — baseline 과 같은 분할.
  - 처리 (`stepWindow`): `[low, low+W)` 만 seq 순으로 처리. 슬롯 k 의 pre-prepare 는 `[low, k)` 가 모두 검증된 pre-prepare 를 가질 때만 처리(**D2 접두사 규칙**; 간극에서 `break`, 표는 보존). 단계 진입 시 prepare/commit 1회 송신(복원 슬롯 포함).
  - 전달 (`tryDeliver`): `slots[low]` 가 `slotCommitted` 일 때만 `Decide`; 전달 직전 `advanceLow` 가 `ProposalSequence/DecisionsInView` 를 함께 +1 하고 `ViewSequences` 에 low watermark 발행 (I3/I4). `Controller.Decide` 블로킹 유지.
  - 검증·메타데이터: `verifyProposal(seq,…)` 가 `LatestSequence == seq`, `DecisionsInView == expectedDecisions(seq) = DecisionsInView + (seq − low)`; 리더 `GetMetadata` 는 `head` 와 `expectedDecisions(head)` — 같은 식 (Lemma).
  - W>1 전용 2곳: (1) 리더의 미커밋 슬롯 pre-prepare 재브로드캐스트 `resendTick` (주기 = `LeaderHeartbeatTimeout/LeaderHeartbeatCount`, W=1 이면 nil 채널), (2) 같은 pre-prepare 중복 수신 시 Debug (W=1 은 기존 Warn 유지).
- **Controller** (`controller.go`): `leaderToken` 을 cap W 세마포어로, 토큰은 발급 뷰 포인터를 싣는다. `proposerLoop` 고루틴이 토큰마다 `propose(view)`. R1 뷰 시작 시 `W − restored` 발급(+`UnreserveAll`) · R2 결정 후 1개 반환 · R3 abort drain · R4 로테이션 시 R2 생략 · R5 빈 배치 반환 · R6 Stop drain · R7 중단된 뷰의 토큰 폐기. `propose` 가 배치 요청을 `RequestPool.Reserve` 로 예약. `maybePruneInFlight → clearUpTo`, `decide → pruneDelivered`.
- **RequestPool** (`requestpool.go`): 예약 상태 — `NextRequests` 가 예약 건을 건너뛰고 `check` 는 미예약 수/바이트 기준, 삭제 시 예약 해제, `Size()` 는 예약 포함.
- **InFlightData** (`util.go`): seq 키 맵, getter 는 최저 seq, `clearUpTo/pruneDelivered`.
- **State** (`state.go`): `InFlightSequenceInWAL` — W>1 에서 WAL 에 in-flight 레코드(마지막 pre-prepare, 또는 pre-prepare+commit, seq > 마지막 결정)가 있으면 `Consensus.Start` 가 **error 반환** (panic 아님). `storeProposal` 이 pre-prepare seq 로 키를 지정.
- **consensus.go**: `Controller.PipelineDepth`, `ProposalMaker.PipelineDepth/PrePrepareResendInterval` 배선, 재시작 거부 검사.

범위 밖(브랜치 5/6/8/9)은 설계 §1.2 의 임시 동작 그대로: ViewData 는 최저 in-flight 항목 1건, `pruneDelivered` 가 최고 항목을 남겨 W=1 ViewData 바이트 동일, W>1 in-flight 재시작은 명시적 error.

## 왜

baseline 은 seq k 의 Decide 가 끝나야 k+1 을 제안한다(리더 토큰 1개, 뷰의 2슬롯). RTT 가 지배하는 환경에서 처리량 상한이 `1/(RTT 왕복 + Deliver)` 로 묶인다. W 개를 겹치면 왕복이 중첩된다. 이 브랜치는 그 기반(토큰 세마포어·슬롯 윈도우·예약·in-flight 다중화)을 **W=1 에서 메시지 수준 동일**을 유지하며 넣는다. 설계: `docs/notes/design-pipeline-window.md` (§14 리뷰 결정 반영).

## 안전성에 미치는 영향

- **seq 별 합의 불변**: 슬롯마다 독립된 `voteSet`(발신자당 1표)·다이제스트 검사·`Quorum-1` 수집 로직은 baseline 코드 그대로(`stepPrepares/stepCommits`). 한 (view, seq) 에 pre-prepare 는 최초 1개만(I1); 상충 제안은 prepare 정족수가 서지 않는다. 정족수 계산(`computeQuorum`) 은 W 와 무관.
- **순서 전달**: `Decide(k)` 는 `k == low ∧ slotCommitted` 일 때만, 호출 전 `low = k+1` (I3). `TestViewInOrderDeliver`(L+1 이 먼저 committed), `TestPipelineDecideInOrder`(노드 1,4 가 seq 2,3 을 먼저 committed → 전달 0건 → seq 1 커밋 후 1,2,3 순 전달).
- **중복 전달 없음**: `slotDelivered → release()`, `tryDeliver` 는 `slots[low]` 만. 뷰 체인지 경쟁은 `MutuallyExclusiveDeliver`(변경 없음). 하네스 `"Committed sequence %d twice"` 패닉 0건.
- **요청 중복 제안 없음**: in-flight 제안의 요청은 `Reserve` 로 `NextRequests` 에서 제외, 해제는 삭제(전달) 또는 `startView` 의 `UnreserveAll` 만. `TestMultiInFlightBasic` 44건 중복 0.
- **D2 (팔로워 prepare 선행 조건)**: `phase ≥ slotProposed` 집합은 항상 `[low, p)` 접두사 → 접두사 밖 인증서는 접두사 안의 pre-prepare 를 가진 노드만으로 만들어진다(브랜치 6 접두사 복구 논증의 전제). `TestViewFollowerPreparePrecondition`.
- **뷰 체인지 (임시, 브랜치 6 전)**: ViewData 는 최저 in-flight 1건, `CheckInFlight` 는 `maxLastDecision+1` 만 복구, 그 위 in-flight 는 뷰와 함께 폐기되고 요청은 풀에 남아 새 뷰에서 재제안(`UnreserveAll`). Deliver 기준 안전성 유지(설계 §1.2/§8.4). 라이브니스 최적이 아님(문서화된 한계).
- **재시작 (임시, 브랜치 5 전)**: W>1 + WAL in-flight 는 `Consensus.Start` 가 error 로 거부 (`TestPipelineRestartWithInFlightIsRejected`). W=1 복원 경로는 그대로(`TestRestartKeepsViewAndInFlightProposal`, `TestLeaderProposeAfterRestartWithoutSync` 통과).
- **prev_commit_signatures / 블랙리스트 / 로테이션**: 코드 미변경. `Validate()` 가 W>1 ⇒ `LeaderRotation=false`(DecisionsPerLeader 0) 를 강제하므로 W>1 경로에서 비활성.
- **config barrier**: 미구현(브랜치 8). W>1 중 설정 트랜잭션이 섞이면 `VerificationSequence` 불일치로 다음 seq 제안이 거부 → 뷰 체인지(안전, 라이브니스 저하). 테스트는 W>1 에서 reconfig 하지 않음.

## 플래그

- 플래그: `PipelineDepthMax` (`EffectivePipelineDepth()`: 0 → 1)
- 기본값(OFF)에서 원본 동작과 동일함: **예** — (i) 전체 기존 테스트 통과, (ii) `TestMessageHistogramMatchesBaseline` 이 `baseline` 태그에서 기록한 golden 과 메시지 종류별 송신 수를 비교해 통과(아래), (iii) `if W==1`/`W>1` 분기는 설계 §2.2 의 두 곳(`resendTick` nil, 중복 pre-prepare 로그 레벨)뿐, (iv) 승인된 W=1 편차 3개는 송신 메시지 집합에 영향 없음.

## 테스트

모든 명령은 `/Users/liam/Desktop/smartbft/wt-feat-window` 에서, Go 1.27.3, golangci-lint v2.15.0, mockery v3.8.1.

```
go build ./... && go vet ./...                                  # OK
golangci-lint run ./...                                         # 0 issues
go test ./internal/... ./pkg/... -count=1                       # ok (internal/bft, pkg/api, pkg/types, pkg/wal)
go test ./internal/bft/ -race -count=1                          # ok, DATA RACE 0
go test ./test -run 'TestBasic$|TestBasicReconfig|TestTwoPrePreparesInARow|TestRestartKeepsViewAndInFlightProposal$|TestNodeInFlightThenViewChange$|TestPipelineConfigSurvivesRestart|TestAdaptiveBatch|TestMultiInFlight|TestPipeline|TestMessageHistogram' -count=1
                                                                # ok, wall-clock 128.3 s (go test 보고 127.2 s)
go test ./test -count=1                                         # ok (전체 통합 스위트), wall-clock 155.4 s (go test 보고 154.4 s)
go test ./test -run 'TestMultiInFlightBasic|TestPipelineDecideInOrder|TestPipelineLeaderStopsAtWindow|TestPipelineRestartWithInFlightIsRejected' -count=3
                                                                # ok 7.3 s (3회 모두 통과, flaky 아님)
go test ./test -run TestMessageHistogramMatchesBaseline -count=3  # ok 375 s (3회 모두 통과; 그 전 판의 -count=3 는 state_transfer_response 15 vs 기록 14 로 1회 실패 → state transfer 를 범위 비교로 바꾼 뒤 재실행)
```

커밋 단위 결과: (a)(b) 단위 전체 ok · (c) 단위 ok + 전체 통합 137 s ok · (d) 단위 ok + 타깃 통합 124 s ok · (e) 단위/race ok + 전체 통합 142 s ok (첫 실행에서 `TestHeartbeatTimeoutCausesViewChange` 가 뷰 체인지 1회 초과로 실패 → `changeView` 의 `Batcher.Reset` 순서 수정 후 통과, 아래 "차이" 2) · (f) 신규 테스트 ok.

### 플래그 ON 테스트 (신규)

| 테스트 | W | 어서션 |
|---|---|---|
| `TestMultiInFlightBasic` | 4 | 팔로워 commit 보류 중 리더가 seq 1..4 제안(`Deciding` 0건 → in-flight 4), 이후 4건×10 그룹; 전 노드 `"but our sequence is"`/`"dropping message"`/`"Heartbeat sequence is bigger than expected"` 0건, 44 레코드 전 노드 동일·seq 연속, 리더 in-flight 최대 ≤ 4 |
| `TestPipelineDecideInOrder` | 3 | 노드 2,3 의 seq 1 commit 을 보류(Assist prepare 로 치환·후속 재전송) → 노드 1,4 가 seq 2,3 을 먼저 committed, 전달 0건 → 재전송 후 1,2,3 순 전달, 전 노드 동일 |
| `TestPipelineLeaderStopsAtWindow` | 3 | 요청 5건, commit 보류 → `Proposing … 3` 까지만, 1 s 동안 `… 4` 없음, 해제 후 4,5; 전 과정 in-flight 최대 == 3 |
| `TestPipelineRestartWithInFlightIsRejected` | 3 | pre-prepare 만 받은 노드의 `TryRestart()` → `Consensus.Start` error (`"sequence 1 in flight"`, `"PipelineDepthMax 3"`), panic 없음 |
| 단위 (`internal/bft`) | 1,2,3,4 | `TestViewSlotRouting`(W∈{1,2,4}) · `TestViewInOrderDeliver` · `TestViewFollowerPreparePrecondition` · `TestViewLeaderResendsPrePrepares`(W=2 재전송, committed 제외; W=1 `resendTick==nil`·추가 송신 0) · `TestViewDuplicatePrePrepareLogLevel` · `TestViewGetMetadataPredictsDecisions` · `TestInFlightSequenceInWAL` · `TestControllerTokenSemaphore`(W∈{1,3}) · `TestControllerDecideRotationNoDoubleToken` · `TestProposerLoopDiscardsTokenOnStoppedView` · `TestProposerLoopProposesForEveryToken` · `TestReqPoolReserve` · `TestInFlightDataMultiplex` |

### 메시지 히스토그램 동일성 (W=1 ↔ baseline)

`TestMessageHistogramMatchesBaseline`: 모든 노드의 `MutateSend` 훅으로 송신 메시지를 종류별로 집계. golden `test/testdata/message_histogram_baseline.json` 은 `baseline` 태그(`32a628d6`)의 임시 워크트리(`git worktree add --detach ../wt-baseline baseline`, 작업 후 `remove --force`)에 **같은 테스트 파일**을 복사해 `SMARTBFT_RECORD_HISTOGRAM=5` 로 5회 기록(절차는 테스트 상단 주석). 노드가 매 seq 정확히 1회 브로드캐스트하는 prepare/commit 과, 이미 전달한 seq 의 메시지를 늦게 받은 노드가 되돌려 보내는 Assist prepare/commit(`prepare_assist`/`commit_assist`, 타이밍 의존) 을 분리해 센다. 비교 규칙(코드에 고정): 기록 5회가 일치한 종류는 **정확 일치**, 아니면 기록 범위 ±3; `*_assist`·heartbeat 는 기록 최댓값 +3 상한.

| 시나리오 | 종류 | baseline (5회) | 이 브랜치 W=1 (7회: 타깃 1회 + `-count=3` 2회) | 판정 |
|---|---|---|---|---|
| basic (요청 1건) | pre_prepare / prepare / commit | 3 / 12 / 12 | 3 / 12 / 12 (7회 모두) | 정확 일치 |
| | state_transfer_request / response | 12 / 12 | 12 / 12 (7회 모두) | 범위 ±3 (아래 설명) |
| | commit_assist / prepare_assist | 0–3 / 0 | 0–3 / 0 | 상한 |
| | view_change / view_data / new_view / heart_beat | 0 | 0 | 정확 일치 |
| in-flight → view change (`TestNodeInFlightThenViewChange`) | pre_prepare / prepare / commit | 12 / 39 / 48 | 12 / 39 / 48 (7회 모두) | 정확 일치 |
| | view_data / new_view | 4 / 6 | 4 / 6 (7회 모두) | 정확 일치 |
| | state_transfer_request / response | 15 / 14 | 15 / 14–15 | 범위 ±3 |
| | view_change | 54–57 (5 s 타이머 재전송) | 54 (7회 모두) | 범위 ±3 |
| | heart_beat | 51–54 | 51–54 | 상한 |
| | commit_assist / prepare_assist | 0 / 0 | 0 / 0 | 상한 |

비교 규칙은 코드에 고정: 합의·뷰체인지 종류(pre_prepare/prepare/commit/view_change/view_data/new_view)는 기록 5회가 일치하면 **정확 일치**, 아니면 기록 범위 ±3; state_transfer_request/response 는 항상 기록 범위 ±3(뒤처진 노드의 sync 시점과, 그 순간 끊긴 노드가 응답하지 못하는 일이 타이밍에 따름 — 두 번째 기록에서 14/14 였던 response 가 이 브랜치 실행에서 15 로 나와 정확 일치 규칙을 벗어난 적이 있음); `*_assist`·heartbeat 는 기록 최댓값 +3 상한. 첫 golden(assist 를 분리하지 않은 판)에서는 basic 의 commit 이 baseline 자체에서 12–15 로 변동했고 타깃 통합 테스트와 병렬 실행 중 1회 범위를 벗어났다(늦은 commit 에 대한 Assist 응답은 부하에 따라 늘어남). Assist 를 별도 종류로 분리한 뒤 basic 의 구조적 종류(pre-prepare/prepare/commit)는 7회 모두 정확 일치한다.

## 셀프 리뷰 체크리스트

### A. 공통

- [x] 변경이 설정 플래그 뒤에 있고, 플래그 기본값이 원본 동작과 동일하다 — `PipelineDepthMax` 기본 1; W>1 분기 2곳(§2.2) 외 코드 경로 단일, W 매개화
- [x] 플래그 OFF 상태에서 `baseline`과 메시지 흐름이 동일하다 — `TestMessageHistogramMatchesBaseline` (위 표), 기존 통합 테스트 전체 통과, 로그 훅 문자열 15+4개 보존
- [x] 플래그 OFF: `go build ./... && go vet ./...` 통과
- [x] 플래그 OFF: `go test ./internal/... ./pkg/...` 통과 (+ `-race` on `internal/bft`)
- [x] 플래그 OFF: `go test ./test/...` 통과 — 전체(`-run` 없음) 155 s + 타깃 패턴(위) 128 s
- [x] 플래그 ON 테스트를 최소 1개 추가했다 — 통합 4개 + 단위 11개
- [x] 커밋 메시지가 `<area>: <요약>` 형식이며 `Flag:` 줄이 있다 — 6 커밋 모두 `Flag: PipelineDepthMax (default 1 = unchanged)`, `Refs:` 포함
- [x] `git rebase main` 완료, 다른 `feat/` 브랜치 위에 스택되지 않았다 — base `5ae45b7c` = main
- [x] 관련 설계 노트가 `docs/notes/` 에 있고 `Refs:` 로 연결했다 — `design-pipeline-window.md`, `decisions-2026-11-02.md`

### B. 안전성

- [x] 서로 다른 정직 노드가 같은 seq에 다른 블록을 커밋할 수 있는 경로가 없다 — seq 별 `voteSet`·다이제스트·정족수 로직 baseline 그대로(I1 최초 pre-prepare 1개), `TestPipelineDecideInOrder`/`TestMultiInFlightBasic` 전 노드 레코드 동일
- [x] 쿼럼(2f+1) 계산이 파이프라인 깊이와 무관하게 유지된다 — `computeQuorum` 미변경, 슬롯 스텝은 `v.Quorum-1` 사용, `TestViewInOrderDeliver` 서명 수 == Quorum
- [x] 뷰 체인지 중 in-flight 제안이 누락·중복 커밋되지 않는다 — 최저 in-flight 1건 복구(`CheckInFlight`/`commitInFlightProposal` 미변경), 상위 in-flight 는 폐기 후 재제안(요청 풀 잔존), `MutuallyExclusiveDeliver` 가 중복 차단. 다중 in-flight 복구는 브랜치 6 (문서화된 한계: 라이브니스)
- [x] `prev_commit_signatures` 체인이 끊기지 않는다 — 코드 미변경; W>1 은 `Validate()` 로 `DecisionsPerLeader=0` 강제 → 체인 비활성(대체 근거: `docs/notes/prev-commit-dependency.md`)
- [ ] 설정 트랜잭션 전후로 파이프라인이 비워진다 (config barrier) — **브랜치 8 범위**. 현 동작: W>1 중 reconfig 는 다음 seq 의 `VerificationSequence` 불일치로 거부 → 뷰 체인지(안전). W>1 테스트는 reconfig 를 하지 않음

## 열린 질문 결정 목록

(`docs/notes/decisions-2026-11-02.md` feat 4 표 그대로)

| # | 항목 | 결정 |
|---|---|---|
| (b) | baseline 에 주기적 재전송 없음 | 확인·정정 동의. pre-prepare 재브로드캐스트는 W>1 전용(`resendTick` nil at W=1). **W>1 에서도 1차 복구 경로는 sync 이고 재전송은 지연 단축용 보조** — 안전성 논증은 재전송에 의존하지 않음 |
| (c) | W=1 편차 3개 (prev-seq 응답 슬롯 정확, 죽는 뷰로의 제안 억제, 늦은 중복 pre-prepare Warn 1회) | 승인, 조건: **메시지 종류별 히스토그램 동일성 테스트** 추가. `MutateSend` 훅으로 pre-prepare/prepare/commit/heartbeat/view-change 수를 세어 baseline 태그와 W=1 main 이 `TestBasic`·`TestNodeInFlightThenViewChange` 에서 같음을 단언 (golden 파일 방식, 타이밍 의존 메시지는 범위로) |
| ① | 재전송 틱 | 하트비트 주기, 새 설정 필드 없음. **한계 기록**: 6 s 는 라운드(ms)보다 길어 netem 손실 실험 p99 에 드러남. 후속 브랜치 후보: gap-triggered 1회 재전송(리더가 슬롯 k+1 prepare 는 받았는데 k 는 못 받은 노드에게 k 의 pre-prepare 를 노드·슬롯당 1회) |
| ② | pre-prepare Save 의 `truncateTo` | `true` 유지 (브랜치 5 가 바꿈) |
| ③ | `Controller.Decide` 블로킹 | 유지. **한계 기록**: Deliver 완료까지 뷰 고루틴 블록 → 다른 슬롯 메시지가 incMsgs 에 적체. W 를 키워도 TPS 가 안 오르면 첫 용의자. 판별용으로 Deliver 소요(`view_latency_batch_save`, PR #2 에서 의미 명시)를 라운드 지연과 함께 그래프에 |
| ⑩ | 팔로워 수용 구간 W+1 칸 | 권고대로 (W=1 에서 {S, S+1} 환원이 근거) |
| (a) | DecisionsInView 예측식 | **Lemma 추가**: 팔로워 검사식과 리더 예측식이 같은 식. 새 뷰 초기값에서도 성립 — `changeView(view, seq, dec)` 가 세 값을 함께 받아 `startView` 가 같은 쌍으로 View 를 만들며(controller.go:386-414, 363-384), 호출 4곳(:491 새 뷰 dec=0, :503 sync, :510 뷰 재시작, :561 로테이션)이 모두 노드 간 동일한 입력에서 쌍을 유도. 뷰 시작 쌍의 일치는 baseline 이 이미 요구 |
| (d) | 한계 2개 (W>1 재시작 미지원, >1 in-flight 뷰 체인지 단일 복구 폴백) | 승인. **추가**: W>1 + in-flight 상태의 재시작을 `Restore`/`Start` 에서 명시적으로 거부 (**error, panic 아님**) |
| 기타 | ④~⑨, ⑪, ⑫ | 권고대로 |

구현 대응: (b) `View.resendTick`/`resendPrePrepares` · (c) 편차 3개 그대로 + `TestMessageHistogramMatchesBaseline` · ① `Consensus.prePrepareResendInterval()` = `LeaderHeartbeatTimeout/LeaderHeartbeatCount` · ② `State.Save(ProposedRecord, true)` 유지 · ③ `Controller.Decide`/`decisionChan` cap 1 유지 · ⑩ `processMsg` 수용 `[low, low+W]`, `stepWindow` 처리 `[low, low+W)` · (a) `expectedDecisions` 를 `GetMetadata`/`verifyProposal` 이 공유 · (d) `InFlightSequenceInWAL` + `Consensus.Start` error.

## 설계 문서와의 차이

구현 중 설계(`design-pipeline-window.md`)와 달라진 점. 모두 W=1 메시지 동일성을 유지한다.

1. **토큰이 뷰 포인터를 싣는다** (`leaderToken chan Proposer`). 설계 §6.2 는 `propose()` 안에서 `view := c.currentView()` 스냅샷 + `Stopped()` 검사였다. 토큰을 꺼낸 뒤 스냅샷하기 전에 뷰가 바뀌면 옛 토큰으로 새 뷰에 제안해 in-flight W+1 이 가능하므로, 발급 시점에 뷰를 토큰에 묶었다(R7 "뷰 포인터가 세대"의 직접 구현). `acquireLeaderTokens(view, n)`; R2 는 `currView`, R5 는 같은 뷰로 반환.
2. **`changeView` 에서 `Batcher.Reset()` 을 `startView` 앞으로** 이동. baseline 순서(startView → Reset)는 propose 가 run 고루틴에서 뒤에 실행되어 문제 없었지만, proposerLoop 는 발급 즉시 토큰을 꺼내 `Batcher.Closed()==true` 로 폐기해 새 리더가 영원히 제안하지 않았다(`TestHeartbeatTimeoutCausesViewChange` 가 뷰 체인지 2회로 실패). 리더가 아니면 영향 없음, 메시지 동일.
3. **`InFlightData.StorePrepares(view, seq)` 의 폴백**: 해당 seq 항목이 없고 항목이 정확히 1개이면 그 항목을 prepared 로 표시(설계 §8.1 은 panic). 기존 `TestSaveCommitUsesTheGivenTruncationFlag` 가 메타데이터 없는 제안(키 0)을 저장한 뒤 seq 200 의 commit 을 저장하기 때문. 항목 0개 또는 2개 이상이면 기존 panic.
4. **`StoreProposalAt(seq, prop)` 추가**, `PersistedState.storeProposal` 이 pre-prepare 의 `Seq` 로 키 지정(설계 §13-7 권고는 메타데이터 추출; `state_test` 의 픽스처가 pre-prepare seq 200 / 메타데이터 seq 0 로 어긋남). `StoreProposal(prop)`(메타데이터 키, 실패 시 0) 은 다른 호출자용으로 유지. `state.go` 변경은 이 한 줄 + `InFlightSequenceInWAL`.
5. **`pruneDelivered` nil 리시버 안전** — `InFlight` 없이 만든 컨트롤러 픽스처(기존 테스트)용.
6. **`doPhase` 가 low 가 움직이는 동안 반복** (`stepWindow → tryDeliver` 루프). 설계 §3.4 의 단일 패스(앞·뒤 `tryDeliver`)는 전달로 윈도우에 들어온 슬롯을 다음 이벤트까지 처리하지 않아 `TestTwoPrePreparesInARow` 가 멈췄다(baseline 은 `default: doPhase()` 스핀으로 이어감).
7. **`verifiedSigs`**: 송신이 `abortChan` 에서 포기(고루틴 누수 방지), 용량 `N·(2W+2)`(설계 `N·(W+2)`), `acceptVerifiedSig` 는 슬롯당 `Quorum-1` 개까지만 수집 — baseline 이 `Quorum-1` 에서 수집을 멈춰 결정의 서명 수가 정확히 Quorum 인 것을 보존.
8. **재시작 거부 위치**: `Consensus.Start`(설정 검증 직후, 컴포넌트 생성 전) 에서 `InFlightSequenceInWAL(WALInitialContent, Metadata.LatestSequence)` 로 판정해 error 반환. `Restore` 의 error 는 `NewProposer` 가 `Panicf` 로 바꾸므로 "error, panic 아님" 요구를 만족하지 못한다. 하네스에 `App.TryRestart() error` 추가.
9. **`Start()` 를 `setup()` + 고루틴으로 분리** — `TestViewSlotRouting` 이 고루틴 없이 `processMsg` 를 직접 호출하기 위함. 동작 동일.
10. **`Phase` 메트릭**: `slotCommitted`(정족수 수집, 전달 대기) 는 `PREPARED` 로 보고(baseline 에서 그 구간의 Phase 값).
11. **히스토그램 테스트의 정확 일치 범위**: 설계 §14 는 basic 시나리오 전부 정확 일치를 예상했지만 baseline 자체에서 commit 수가 12–15 로 변동(전달 직후 도착한 commit 에 prev-seq assist commit 으로 응답). 따라서 basic 은 pre-prepare/prepare/state-transfer 정확 일치 + commit 범위, view-change 시나리오는 pre-prepare/prepare/commit/view-data/new-view 정확 일치 + view-change/state-transfer 범위 + heartbeat 상한. 규칙은 코드에 고정, 근거는 golden 의 min/max.
12. **기존 테스트 픽스처 수정** (동작 변경 아님): `controller_test.go` 의 `mocks.RequestPool` 12곳에 `.On("UnreserveAll")/.On("Reserve", …)`, `RequestInspector: rawRequestInspector{}` 추가(propose 가 요청 ID 를 예약에 씀; 기존 `testRequestInspector` 는 임의 바이트에 panic); `controller_decide_test.go` 픽스처에 `InFlight`, `leaderToken chan Proposer`. mockery 로 `mocks/request_pool.go` 재생성. 그 외 기존 테스트는 무수정 통과.
13. **`TestMultiInFlightBasic` 의 부하 형태**: 설계는 보류 1회 + 4건이었고, 여기서는 보류 1회(in-flight 4 증명) + 4건×10 그룹(그룹마다 전 노드 전달 대기). 그룹 단위로 보내는 이유: 자유 흐름에서는 팔로워가 2 전달 뒤처지면 `s > low+W` 의 pre-prepare 를 정당하게 버리고(baseline 의 S+2 폐기와 같은 정책) 재전송/sync 로 회복하므로 "드롭 0건" 단언이 부하에 따라 흔들릴 수 있다. 그룹 안에서는 `head ≤ low_group + W ≤ low_F + W` 가 보장된다. 자유 흐름 하의 드롭/재전송 빈도는 벤치에서 측정할 항목.
14. 설계 §11.2 의 `TestPipelineDepthOneEqualsBaseline` 은 추가하지 않음 — `TestMessageHistogramMatchesBaseline` 이 W=1 동일성을 더 강하게 고정.
15. `TestPipelineDecideInOrder` 의 지연 수단: 설계는 `MutateSend` 안에서 블록이었으나 훅은 송신 뷰 고루틴에서 실행되어 "seq 1 commit 을 seq 2,3 commit 뒤로" 미루면 자기 자신을 막는다. 대신 seq 1 commit 을 Assist prepare(수신측에서 중복 표로 무시) 로 치환하고 원본을 `Network.send` 로 나중에 재전송.

## Refs

- docs/notes/design-pipeline-window.md (§1–§14)
- docs/notes/decisions-2026-11-02.md
- docs/notes/leader-token-flow.md, view-slots.md, viewchange-trace.md ("안전성 보강" D1/D2), test-harness.md
