# PR: `feat/adaptive-batch` → `main`

- 브랜치: `feat/adaptive-batch` (base `main` = d9d36397, 스택 없음)
- 커밋 (설계 §9 a→e 순서, 각 커밋이 단독으로 빌드·테스트 통과):
  - 53272152 `batcher: put batch parameters behind an atomic pointer and add SetBatchParams`
  - 80e8b356 `batcher: lock-free Size, SizeBytes and OldestAge getters on the request pool`
  - dcb58656 `batcher: add BatchTuner, a pure control law for batch size and timeout`
  - 855d7c39 `controller: drive BatchTuner from propose/decide/startView when AdaptiveBatch is on`
  - dd7af5d5 `test: adaptive batch integration tests`
- 작성일: 2026-11-02
- 설계: `docs/notes/design-adaptive-batch.md` (§11 리뷰 결정 반영), `docs/notes/decisions-2026-11-02.md`

## 무엇을

리더의 배치 파라미터 두 개 — 배치 최대 요청 수 C(`RequestBatchMaxCount`), 배치 타임아웃 T(`RequestBatchMaxInterval`) — 를 `AdaptiveBatch=true` 일 때 런타임에 조정하는 제어기를 넣는다. 바이트 상한 S(`RequestBatchMaxBytes`) 는 고정이다.

1. **`BatchParams` + `SetBatchParams`** (`internal/bft/batcher.go`, 커밋 a). `BatchBuilder` 의 세 필드를 불변 `BatchParams` 하나로 묶어 `atomic.Pointer` 뒤에 둔다. `NextBatch` 는 호출 진입 시 포인터를 **한 번** 로드하고 네 사용 지점이 모두 그 스냅샷을 쓴다 — 진행 중인 호출은 옛 파라미터(이미 무장된 타이머 포함)를 유지하고, 변경은 다음 호출부터 보인다. `SetBatchParams` 는 0 만 정규화(count/bytes 0→1, timeout≤0→이전값)하고 정책 clamp 는 하지 않는다. `Batcher` 인터페이스에 `SetBatchParams`/`BatchParams` 추가, `internal/bft/mocks/batcher.go` 는 mockery v3.8.1 로 재생성. 게이지 `batcher_max_count`, `batcher_timeout_seconds` 는 **배처가** 생성 시와 `SetBatchParams` 마다 기록한다 (OFF 에서도 설정값이 보인다).
2. **풀 신호 getter** (`internal/bft/requestpool.go`, 커밋 b). `count atomic.Int64`/`bytes atomic.Uint64` 미러를 `Submit`/`deleteRequest`/`RemoveRequest` 의 게이지 기록 지점과 같은 자리(락 안)에서 갱신하고, `Size()` 가 락 없이 미러를 돌려준다 (값 동일; `Close` 도 `deleteRequest` 를 거치므로 0 으로 떨어짐). `SizeBytes()` (신규, 미러), `OldestAge()` (신규, `RLock` + `fifo.Front().additionTimestamp`). `RequestPool` 인터페이스는 변경 없음 — tuner 는 `PoolStats{Size, OldestAge}` 만 받는다.
3. **`BatchTuner`** (`internal/bft/batchtuner.go`, 커밋 c). 고루틴·티커 없는 스텝 구동 컴포넌트. 규칙은 설계 §5.3 그대로이고 §11 의 수정이 들어갔다:
   - G1 drain: 리더 풀 최고령 요청 나이 ≥ `ThetaA·RequestForwardTimeout` → `(C_cap, T_min)`
   - G2 latency: `L_fast`(EWMA, α) > `γ · L_ref` 이고 C > C_min → C×d, 내려온 지점 `cCeil` 을 `CeilHold` 결정 동안 상한으로 기억. **`L_ref` = 최근 `RefWindow` 결정의 `L_fast` 슬라이딩 윈도우 최소** (링 버퍼, O(RefWindow) 스캔). **윈도우가 차기 전(Reset 뒤 결정 < RefWindow)에는 가드가 무장되지 않는다.**
   - R1: FULL 배치 k 연속 + 백로그(`Size()>0`) → C×u (cCeil 이하)
   - R2/R3: TIMEOUT 배치 k 연속, 충만율 φ=n/C ≥ `PhiHi` → T×u / φ < `PhiLo` → T×d; dead band 안이면 유지 (`PhiHi=1.0` 으로 R2 를 끌 수 있음)
   - R5: `IdleAfter` 동안 제안 없음 → `(C_init, T_0)` 쪽으로 한 단계, `IdleAfter` 당 최대 1회
   - `apply` 가 유일한 쓰기 경로이며 항상 clamp. 값이 바뀌지 않으면 publish 하지 않는다 (예외: `Reset` 은 항상 publish 해 배처가 `(C_init, T_0)` 임을 보장).
   - `TunerConfigFromConfiguration` 이 모든 파라미터를 설정에서 유도하고, `WithOverrides` 로 테스트·벤치가 임의 필드만 덮어쓴다 (0 = 유도값 유지, **C_cap·T_max 는 유도값 위로 못 올림**, 순서 불변식 정규화).
   - 카운터 `batcher_count_of_adjustments{param∈{COUNT,TIMEOUT},direction∈{UP,DOWN}}` (값이 실제로 바뀐 노브만), `batcher_count_of_guard_trips{reason∈{DRAIN,LATENCY}}` (가드 조건 참마다). `Initialize` 가 라벨 조합을 선점. disabled provider 지원.
   - 로그: 신규 `Debugf("Adaptive batch: max count %d, timeout %s (%s)")` 한 종류, 사유 `RESET|RELAX|DRAIN|GUARD_DOWN|FULL|TIMEOUT_HIGH_FILL|TIMEOUT_LOW_FILL`. `T_0 > Forward/4` 인 설정이면 생성 시 `Warnf` 1회.
4. **와이어링** (`internal/bft/controller.go`, `pkg/consensus/consensus.go`, 커밋 d). 플래그를 읽는 곳은 `continueCreateComponents` 한 곳. ON 이면 **모든 노드**가 tuner 를 만들어 `Controller.BatchTuner` 에 붙이고 (수명 = 컨트롤러 → reconfig 시 새 설정으로 자동 재생성), 발화는 리더만 지나는 세 지점: `propose()` 의 `NextBatch` 반환 직후 `OnBatch(batch, Batcher.BatchParams())` / 빈 배치 분기 `OnIdle()`, `decide()` 의 `iAm` 블록에서 `acquireLeaderToken()` **직전** `OnDecision()`, `startView()` 리더 분기 `Reset()`. `Consensus.BatchTunerConfig *TunerConfig` (신규, `type TunerConfig = algorithm.TunerConfig` 별칭, nil = 유도 기본값) 로 `types.Configuration` 을 건드리지 않고 주입. 하네스 `test_app.go` 는 `Restart()` 에서 이 필드도 carry-over.
5. **통합 테스트** (`test/adaptive_batch_test.go`, 커밋 e): 아래 "테스트".

**실제 적용된 기본값** (`TunerConfigFromConfiguration`):

| 기호 | 값 | fastConfig | DefaultConfig |
|---|---|---|---|
| `C_cap` | `min(RequestBatchMaxCount, RequestPoolSize / EffectivePipelineDepth())` (최소 1) | 10 | 100 |
| `C_min` | 1 | 1 | 1 |
| `C_init` | `max(C_min, ⌈C_cap/4⌉)` | 3 | 25 |
| `S` | `RequestBatchMaxBytes` (고정) | 10 MiB | 10 MiB |
| `T_0` | `RequestBatchMaxInterval` | 10 ms | 50 ms |
| `T_min` | `max(T_0/4, 1 ms)`, 단 `T_0` 을 넘지 않음 | 2.5 ms | 12.5 ms |
| `T_max` | `max(T_0, min(4·T_0, RequestForwardTimeout/4))` (§10-7) | 40 ms | 200 ms |
| `u`, `d` | 1.25, 0.75 (`mulUp` 최소 +1, `mulDown` 최소 −1) | | |
| `k` | 3 | | |
| `φ_lo`, `φ_hi` | 0.25, 0.75 | | |
| `α` | 0.3 | | |
| `γ` | 2.0 (실험 변수; `WithOverrides` 로 스윕) | | |
| `RefWindow` | 100 결정 | | |
| `θ_A` | 0.5 × `RequestForwardTimeout` | 250 ms | 1 s |
| `IdleAfter` | `10·T_0` | 100 ms | 500 ms |
| `CeilHold` | `10·k` = 30 결정 | | |

**강제되는 경계**: 모든 시점에 `C ∈ [1, min(RequestBatchMaxCount, RequestPoolSize/W)]`, `T ∈ [max(T_0/4, 1ms), max(T_0, min(4·T_0, RequestForwardTimeout/4))]`. `apply` 의 clamp 가 유일한 publish 경로이고, `WithOverrides` 는 `C_cap`/`T_max` 를 유도값 위로 올리지 못하므로 테스트·벤치 주입으로도 경계가 넓어지지 않는다 (`TestTunerBounded` 10,000 스텝, `TestTunerWithOverrides`).

## 왜

- 고정 `RequestBatchMaxCount`/`MaxInterval` 은 운영자가 정한 안전 상한이지, 현재 부하에서 지연을 늘리지 않는 가장 큰 배치가 아니다. 제어기는 상한 **아래에서** 탐색한다: 경부하에서는 TIMEOUT 으로 닫히는 배치의 충만율로 T 를 맞추고(상각 vs 지연), 중부하에서 FULL 이 연속되면 C 를 상한까지 올리고, 라운드 지연이 기준의 γ배를 넘으면 C 를 되돌린다. 논문의 "배치 크기 동적 조절" 축이며, 7번 브랜치 `feat/adaptive-depth` 가 C 의 AIMD 골격을 재사용한다.
- 노브마다 신호 하나(C ← FULL+백로그/지연 가드, T ← TIMEOUT 충만율)라 2차원 극한 사이클이 없고, dead band 폭(×3) > 보폭(×1.25) 이라 T 는 band 를 건너뛰지 못하며, 모든 갱신이 clamp 라 유계이다 (설계 §5.4). 파라미터가 전부 설정에서 유도되어 운영자가 새로 정할 숫자가 없다.
- 리뷰 수정(§11): 전 구간 최소 기준의 지연 가드는 RTT 가 영구 증가하면 C 를 천장에 가둔다. 슬라이딩 윈도우 최소는 `RefWindow` 결정 안에 새 수준을 따라가고 C 가 복귀한다 — `TestTunerRecoversAfterPermanentLatencyStep` 이 10→50 ms 스텝 뒤 가드가 `RefWindow+5` 결정 안에 멎고 C 가 `C_cap` 으로 돌아오는 것을 확인.
- 이벤트 구동(티커 없음)인 이유: run 고루틴이 이미 `Batcher` 의 유일한 사용자라 새 동시성이 없고, 결정이 자연스러운 샘플 단위이며, 유휴 완화는 빈 배치 폴(주기 T)로 충분하다. 4번 브랜치의 proposer 분리를 대비해 tuner 내부에 `sync.Mutex` 를 둔다.

## 안전성에 미치는 영향

**합의 안전성에는 영향이 없다.** 바뀌는 것은 리더가 다음 제안에 몇 건을 얼마나 기다려 넣을지(C, T) 뿐이며, 메시지 종류·검증·WAL 레코드·결정 순서는 건드리지 않는다.

- **팔로워 측 배치 크기 검사는 라이브러리·하네스·Fabric verifier 어디에도 없다** (설계 §2.2, 결정 목록 "발견"). 즉 비잔틴 리더는 baseline 에서도 임의 크기 배치를 보낼 수 있으므로 적응형 제어는 공격 표면을 추가하지 않는다. 논문에 필수 기재. 상한 초과 감지 → 뷰 체인지 류 실험은 파이프라인 깊이에만 적용.
- **설정 불변식 유지**: `C ≤ RequestBatchMaxCount` 이므로 `Validate()` 가 설정값에 대해 증명한 `RequestBatchMaxCount ≤ RequestBatchMaxBytes`, `W×RequestBatchMaxCount ≤ RequestPoolSize` 는 자동 성립. 추가로 `C ≤ RequestPoolSize/W` 를 제어기가 직접 막아 W=1 에서도 in-flight 배치만으로 세마포어가 차지 않는다. S 를 고정해 `RequestMaxBytes ≤ S` 가 깨질 수 없다.
- **요청 타임아웃 체인(complain 예산)**: `T_max ≤ RequestForwardTimeout/4` → 배치 대기 `≤ 2·T_max ≤ Forward/2`. G1 가드가 `A ≥ Forward/2` 에서 drain 으로 전환해 리더 풀에서 Forward 자체가 발화하는 것을 막는다. `T_0 > Forward/4` 인 비정상 설정은 `T_max = T_0` 으로 올려 설정값은 항상 허용(OFF 와 ON 의 시작 상태 동일)하고 Warn 한다.
- **뷰 체인지**: `startView` 리더 분기에서 `Reset()` → `(C_init, T_0)`, 샘플·EWMA·streak 초기화. 늦게 도착한 옛 뷰의 결정은 `decide` 가 `d.view != currView` 로 걸러 `OnDecision` 에 이르지 않고, WAL 복원 in-flight 의 결정은 `pending == nil` 이라 샘플 없이 반환한다 (`TestTunerIgnoresDecisionWithoutSample`).
- **reconfig**: 컨트롤러·배처·tuner 가 함께 재생성되어 새 설정의 `(C_init, T_0)` 로 리셋. `AdaptiveBatch` 가 true→false 로 바뀌면 tuner 가 만들어지지 않는다.
- **동시성**: `BatchParams` 는 불변 구조체 포인터 교체라 읽기가 세 값을 한 세트로 본다 (`TestSetBatchParamsConcurrent`, 8 writer × 8 reader, 필드 혼합 0, `-race` 깨끗). tuner 의 세 훅은 모두 run 고루틴에서 순차 실행되고 뮤텍스로도 보호된다. 풀 미러는 락 안에서 갱신되고 읽기는 wait-free.
- **활성성**: `T_min ≥ 1 ms` 가 유휴 리더의 busy-loop 을 막는 바닥. 모든 훅은 O(1) 산술 + atomic 로드 + `RLock` 1회 (`OldestAge`) 이며 `NextBatch` 밖에서 실행되어 run 루프 블로킹을 늘리지 않는다 (`T_max` 는 `NextBatch` 의 대기 상한을 4배까지 늘리지만 뷰 체인지는 `Batcher.Close()` 로 즉시 깨운다).

## 플래그

- 플래그: `AdaptiveBatch`
- 기본값(OFF)에서 원본 동작과 동일함: **예**
  - 플래그를 읽는 곳은 `pkg/consensus/consensus.go continueCreateComponents` 한 곳. OFF 면 `Controller.BatchTuner == nil` 이고 훅 4곳은 전부 `if c.BatchTuner != nil` 가드 뒤에 있다. **`SetBatchParams` 를 부르는 코드는 tuner 하나뿐**이므로 OFF 에서는 모든 `NextBatch` 호출이 생성자 값을 로드한다 — `NextRequests` 인자·`time.After` 시작 시점·`countBatch` 위치는 원본과 같고, 바뀐 것은 "필드 3개 읽기" → "포인터 1회 atomic 로드" 뿐.
  - 새 고루틴 없음(ON 에서도), 기존 로그 문자열 변경 없음(test-harness.md 의 훅 문자열 15개 그대로; tuner 로그는 신규 `Debugf` 한 종류), `RequestPool` 인터페이스·proto·`pkg/api` 인터페이스 변경 없음(`MetricsBatcher` 필드 추가만).
  - 증거: `TestLeaderPropose` 에 `batcher.AssertNotCalled("SetBatchParams"/"BatchParams")` 추가; `TestAdaptiveBatchOffIsBaseline` 이 같은 부하에서 "Adaptive batch:" 로그 0건·배치 ≤ 10·전 노드 동일을 고정; 기존 단위 테스트(`TestBatcherBasic/WhileSubmitting/Close/Reset/CountsBatchesByReason`, `TestReqPool*`, 컨트롤러·뷰·뷰체인저 전부)와 통합 스위트 전체가 무수정 통과.

## 테스트

실행 환경: macOS (darwin/arm64), go1.27.3, worktree `/Users/liam/Desktop/smartbft/wt-feat-adaptive`, HEAD dd7af5d5. 전부 `-count=1`, wall-clock 은 `/usr/bin/time -p` 의 `real`.

- [x] `go build ./... && go vet ./...` — 통과
- [x] `go test ./internal/... ./pkg/... -count=1` — 통과, **8.41 s** (`internal/bft` 6.12 s · `pkg/api` 0.58 s · `pkg/types` 0.80 s · `pkg/wal` 7.97 s)
- [x] `go test ./internal/bft/ -race -count=1` — 통과, **8.84 s**, data race 없음
- [x] `go test ./test -run 'TestBasic$|TestBasicReconfig|TestRestartKeepsViewAndInFlightProposal$|TestPipelineConfigSurvivesRestart|TestAdaptiveBatch' -count=1` — 통과, **4.80 s** (`TestAdaptiveBatchGrowsUnderLoad` 3.0 s, `TestAdaptiveBatchRelaxesWhenIdle` 3.2 s, `TestAdaptiveBatchOffIsBaseline` 3.9 s; 셋 다 `t.Parallel()`)
- [x] `go test ./test -count=1` (전체 통합 스위트 1회) — 통과, **129.04 s wall** (패키지 128.07 s, user 7.5 s; 2m09s, `--- FAIL` 0건; test-harness.md 측정 139 s·PR #1 의 2m09s 와 같은 수준)
- [x] `go test ./test -run 'TestAdaptiveBatch' -count=3` — 통과 (안정성 확인, 11.7 s)
- [x] `golangci-lint run ./...` (tools/go.mod 고정 v2.15.0; gci·gofumpt·gofmt·goimports 포함) — **0 issues**. `gofmt -l internal pkg test` 출력 없음.
- [x] mockery v3.8.1 (`rm -f internal/bft/mocks/*.go && mockery --config .mockery.yml`) — `batcher.go` 만 변경, 나머지 목은 바이트 동일.
- [x] 플래그 ON 테스트 추가:
  - 통합 (`test/adaptive_batch_test.go`, 4노드 `newNodeWithConfig`, fastConfig + `SyncOnStart` + 뷰체인지/하트비트 10 s):
    - `TestAdaptiveBatchGrowsUnderLoad` — `AdaptiveBatch=true`, `BatchTunerConfig{K:1}` 주입, 클라이언트 3개가 리더에 300건 제출. 첫 배치 ≤ `C_init`(3), 최대 배치 > 3 이고 ≤ 10, 리더 로그에 `"max count 10,"` 관측, 4노드 레코드 `require.Equal` 로 동일·순서 동일·중복 0. 리더 궤적(로그): `3(RESET) → 4 → 5 → 7 → 9 → 10 (FULL)`, 꼬리에서 `timeout 7.5ms (TIMEOUT_LOW_FILL)`.
    - `TestAdaptiveBatchRelaxesWhenIdle` — `IdleAfter=50ms` 주입. 부하 뒤 `"(RELAX)" … "max count 3,"` 관측 (궤적 `10 → 7 → 5 → 3`), 이어서 10건 일괄 제출 시 첫 배치 ≤ 3 이고 여러 배치로 나뉨, 전 노드 동일.
    - `TestAdaptiveBatchOffIsBaseline` — 같은 부하, 플래그 OFF: "Adaptive batch:" 로그 0건(1 s 부정 확인), 배치 ≤ 10, 전 노드 동일.
  - 단위 (`internal/bft/batchtuner_internal_test.go`, 가짜 Batcher/PoolStats + 주입 시계, 테이블 구동): `TestTunerConfigFromConfiguration` (fast/Default/깊이 8/`T_0>Forward/4`), `TestTunerWithOverrides` (0=기본, 경계 미확대, 정규화), `TestTunerGrowsOnFullStreakWithBacklog` (3,3,4,4,4,5,…,10 — 매 3번째 결정에만 변경, 이후 불변), `TestTunerHoldsWhenFullButNoBacklog`, `TestTunerTimeoutHighFillLengthens` (단조 ↑, 3번째 결정에만, `T_max` 고정, C 불변), `TestTunerTimeoutLowFillShortens` (단조 ↓ → `T_min`), `TestTunerDeadBandHolds`, `TestTunerPhiHiOneDisablesLengthening`, `TestTunerLatencyGuardShrinksAndRemembersCeiling` (22→16, 30 결정 동안 ≤ 22, 그 뒤 28), `TestTunerLatencyGuardNotArmedBeforeWindow`, `TestTunerRecoversAfterPermanentLatencyStep` (RTT 10→50 ms 영구 스텝: 가드가 `RefWindow+5` 안에 멎고 C = `C_cap` 복귀), `TestTunerDrainOnOldRequest` (300 ms ≥ 250 ms → `(10, 2.5ms)`, DRAIN 카운터), `TestTunerIdleRelaxesToInitial` (C 10→7→5→3, T 40→…→10 ms, 이후 publish 0), `TestTunerResetOnStartView`, `TestTunerIgnoresDecisionWithoutSample`, `TestTunerBounded` (난수 10,000 스텝: 경계·no-op publish 없음), `TestTunerNoOscillationUnderSteadyLoad` (λ ∈ {100, 400, 5000}/s 합성 플랜트: 마지막 200 결정 변경 0; 경계 λ 에서 T 단조 비감소), `TestTunerWarnsOnLongInterval`.
  - 단위 (`batcher_test.go`): `TestBatchParamsDefaultsFromConstructor`(게이지 포함), `TestSetBatchParamsNormalisesZero`, `TestSetBatchParamsAppliesToNextCall`, `TestBatcherTimeoutUsesSnapshot`, `TestSetBatchParamsConcurrent`. (`requestpool_test.go`): `TestReqPoolSizeGetters`, `TestReqPoolSizeMirrorUnderConcurrency`. (`controller_test.go`): `TestLeaderProposeDrivesBatchTuner` — 실제 라운드 1회에서 `startView` 의 Reset publish, 제안 2회의 `BatchParams` 읽기, 결정 뒤 `SetBatchParams{MaxMsgCount: 2}` (K=1, 백로그 5) 확인.

## 셀프 리뷰 체크리스트

### A. 공통 (모든 feat/ PR)

- [x] 변경이 설정 플래그 뒤에 있고, 플래그 기본값이 원본 동작과 동일하다 — 플래그를 읽는 곳은 `continueCreateComponents` 한 곳; OFF 면 `Controller.BatchTuner == nil` 이고 `SetBatchParams` 호출자가 없다 (`TestLeaderPropose` 의 `AssertNotCalled`).
- [x] 플래그 OFF 상태에서 `baseline`과 메시지 흐름이 동일하다 — 메시지 송수신·검증·WAL·결정 순서 코드는 무수정 (`git diff d9d36397 -- internal/bft/view.go internal/bft/viewchanger.go internal/bft/state.go smartbftprotos` 비어 있음). 배처는 같은 인자로 `NextRequests` 를 같은 시점에 부른다(§2.1). `TestAdaptiveBatchOffIsBaseline` + 전체 통합 스위트 통과.
- [x] 플래그 OFF: `go build ./... && go vet ./...` 통과
- [x] 플래그 OFF: `go test ./internal/... ./pkg/...` 통과 (8.41 s; `-race` 8.84 s)
- [x] 플래그 OFF: `go test ./test/...` 통과 — 전체 스위트 1회 (`-count=1`, wall-clock 2m09s, 전부 PASS) + `-run 'TestBasic$|TestBasicReconfig|TestRestartKeepsViewAndInFlightProposal$|TestPipelineConfigSurvivesRestart|TestAdaptiveBatch'` (4.80 s)
- [x] 플래그 ON 테스트를 최소 1개 추가했다 — 통합 3개 (`TestAdaptiveBatchGrowsUnderLoad`, `TestAdaptiveBatchRelaxesWhenIdle`, `TestAdaptiveBatchOffIsBaseline`), 컨트롤러 ON 단위 1개 (`TestLeaderProposeDrivesBatchTuner`), tuner 단위 18개.
- [x] 커밋 메시지가 `<area>: <요약>` 형식이며 `Flag:` 줄이 있다 — `batcher:` ×3, `controller:`, `test:`; 다섯 커밋 모두 `Flag: AdaptiveBatch (default false = unchanged)` 와 `Refs:` 줄, attribution trailer 없음.
- [x] `git rebase main` 완료, 다른 `feat/` 브랜치 위에 스택되지 않았다 — base 가 `main`(d9d36397) 그 자체.
- [x] 관련 설계 노트가 `docs/notes/` 에 있고 `Refs:` 로 연결했다 — `design-adaptive-batch.md`, `decisions-2026-11-02.md` (+ 배경 `batcher.md`, `leader-token-flow.md`, `test-harness.md`).

### B~E

- B(안전성): 프로토콜·메시지·WAL 변경 없음 → 해당 없음 (위 "안전성에 미치는 영향" 참조).
- C(활성성): [x] "적응형 제어기가 배치 크기·깊이를 하한/상한 안에서만 움직인다" — `apply` clamp 단일 쓰기 경로, `TestTunerBounded`, `WithOverrides` 경계 미확대. 리더 장애·세마포어 항목은 본 브랜치 비범위.
- D(Fabric): `pkg/api` 인터페이스 무변경 (`MetricsBatcher` 구조체 필드 추가만; Fabric 은 `api.NewMetrics` 를 통해 생성하므로 영향 없음) → 해당 없음.
- E(exp 태그): 해당 없음.

## 열린 질문 결정 목록

`docs/notes/decisions-2026-11-02.md` 의 feat 3 표 그대로:

| # | 항목 | 결정 |
|---|---|---|
| 수정 | 지연 가드 기준선 "전 구간 최소" | **수정 요청**: RTT 가 영구 증가(netem 50→100 ms)하면 가드가 계속 발동해 C 가 천장에 갇힘. **최근 100 결정 슬라이딩 윈도우 최소** 또는 EWMA 쪽으로 천천히 감쇠하는 최소로 변경 (권고: 슬라이딩 윈도우 최소, 윈도우 크기는 TunerConfig) |
| 주의 | LAN 에서 가드가 정상 배치 성장을 혼잡으로 오인 가능 | 설계 의도와 일치하나 LAN TPS 가 baseline 보다 낮으면 이 가드 때문. **γ 를 실험 변수로**(기본 2.0), `guard_trips{reason}` 를 그래프에 함께 표시 |
| 발견 | 팔로워 측 배치 크기 상한 없음 | **논문에 필수 기재**: 팔로워도 Fabric verifier 도 배치 크기를 검사하지 않으므로 비잔틴 리더는 baseline 에서도 임의 크기 배치 가능 → 적응형 제어는 공격 표면을 추가하지 않음. "W_max 초과 감지 → 뷰 체인지" 실험은 배치 크기가 아니라 **파이프라인 깊이(수용 구간 밖 seq 폐기)에만** 적용. 평가 설계에 반영 |
| ③ | `C_init = C_cap/4` | 권고대로. 램프 1.25^6 ≈ 3.8배, k=3 이면 약 18 결정에 C_cap 도달. **벤치 warm-up 최소 30 결정** |
| ④ | 튠 파라미터는 `TunerConfig` 에만 | 권고대로, 조건: **벤치·하네스에서 주입 가능**해야 함 (k, γ, 배율 스윕). `types.Configuration` 비노출과 양립 |
| ⑩ | T 연장 규칙 유지, φ_hi=1.0 으로 끄기 | 권고대로 |
| 기타 | atomic.Pointer 스냅샷, MaxSizeBytes 고정, 풀 getter 3개, dead band ×3 > step ×1.25, 10·T_0 유휴 완화, 나이 가드(0.5×Forward → drain) | 권고대로 |

구현에서의 반영: 슬라이딩 윈도우 최소 → `TunerConfig.RefWindow`(기본 100) + 링 버퍼, 윈도우 미충족 시 가드 비무장 (`TestTunerRecoversAfterPermanentLatencyStep`, `TestTunerLatencyGuardNotArmedBeforeWindow`); γ → `TunerConfig.Gamma`(기본 2.0) + `batcher_count_of_guard_trips{reason}`; 주입 → `Consensus.BatchTunerConfig *TunerConfig` + `WithOverrides`; `C_init = ⌈C_cap/4⌉` (fast 3, Default 25); `PhiHi` 1.0 허용 (`TestTunerPhiHiOneDisablesLengthening`).

## 설계 문서와의 차이 (구현 시 결정)

1. `TunerConfig.Epsilon`(L_ref 상향 드리프트) 은 **두지 않았다** — §11 의 슬라이딩 윈도우 최소가 `L_ref ← min(L_fast, L_ref·(1+ε))` 식을 대체하므로 죽은 파라미터가 된다. 윈도우에 넣는 값은 `L_fast`(EWMA) 이다 (원 식도 `L_fast` 의 최소를 추적했음).
2. 훅 시그니처: `OnBatch(batch, p BatchParams)` (의사코드의 `waited` 미사용 → 제거), `OnDecision()` (의사코드의 `nDelivered` 미사용 → 제거; `unparam` 린터도 걸림).
3. §4.1 의 진단 전용 신호 S6(`PersistedState.OnAppend`), S7(`Submitted()`), S8(`submitRejected`), S9(`leaderFwdTimeouts`) 는 **구현하지 않았다** — 1차 법칙 미사용이고 메트릭 `wal_latency_append`, `pool_count_of_elements_all`, `pool_count_of_fail_add_request_to_pool{reason}` 으로 이미 관측 가능. 필요 시 별도 `metrics:` 커밋.
4. 규칙 R1~R3 의 변경 로그 사유를 빈 문자열 대신 `FULL` / `TIMEOUT_HIGH_FILL` / `TIMEOUT_LOW_FILL` 로 기록 (벤치에서 규칙 발화 로그로 결과를 설명하기 위함; 포맷은 설계 그대로).
5. `T_min = max(T_0/4, 1ms)` 에 "`T_0` 을 넘지 않음" 을 추가 (`T_0 < 1 ms` 설정에서 `T_min > T_max` 가 되는 것을 막음).
6. 타임아웃 보폭은 ns 정밀도 (`⌈1.25·10ms⌉ = 12.5 ms`; 설계 표의 13 ms 는 ms 반올림 표기). 유휴 완화 T 궤적도 40→30→22.5→16.875→12.66→10 ms.
7. `apply` 는 값이 바뀌지 않으면 publish 하지 않되 `Reset` 만 항상 publish 한다 (의사코드는 `why != ""` 면 항상 publish 라 `TestTunerBounded` 의 "no-op publish 없음" 과 충돌). DRAIN/GUARD 카운터는 조건 참마다 증가.
8. `poolStats` → exported `PoolStats` (생성자 인자 타입이 보이도록); `TunerConfig` 는 `pkg/consensus` 에 별칭으로 노출 (별도 모듈의 벤치가 `internal/` 을 import 하지 않고 필드를 채울 수 있게).
9. `WithOverrides` 의 병합 의미(0 = 유도값, `C_cap`/`T_max` 는 `min`) 와 `normalised()` 는 설계에 없던 세부 — 주입 가능성 조건(④)을 만족시키면서 경계 요구를 지키기 위함.
10. 통합 테스트 `TestAdaptiveBatchSurvivesViewChange` (§8.3 네 번째) 는 **미구현** (요구 범위 2개 + OFF 회귀 1개). 뷰 체인지 뒤 `Reset` 은 단위 테스트 `TestTunerResetOnStartView` 와 컨트롤러 `startView` 경로(`TestLeaderProposeDrivesBatchTuner` 의 Reset publish)로 덮는다.
11. `TestTunerNoOscillationUnderSteadyLoad` 의 경계 λ 케이스는 이산 플랜트에서 C=9·T=12.5 ms 에서 정적 상태에 도달한다(수요가 C 와 정확히 맞음, R4). "T 가 `T_max` 로 수렴" 대신 **T 단조 비감소(사이클 없음)** 와 마지막 200 결정 변경 0 을 단언.
12. 하네스: `test_app.go` `Setup` 이 `Restart()` 에서 `BatchTunerConfig` 도 carry-over (3줄).

## Refs

- docs/notes/design-adaptive-batch.md (§2 플래그 분기, §3 API, §4 신호, §5 법칙, §6 상호작용, §7 메트릭, §8 테스트, §9 커밋, §11 리뷰 반영)
- docs/notes/decisions-2026-11-02.md (feat 3 표)
- docs/notes/batcher.md, docs/notes/leader-token-flow.md, docs/notes/test-harness.md
- docs/branch-policy.md (§2, §4, §8), docs/verification-checklist.md (A, C)
