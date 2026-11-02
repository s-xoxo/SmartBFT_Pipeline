# design-adaptive-batch — `feat/adaptive-batch` 상세 설계 (구현 전 리뷰용)

- 날짜: 2026-11-02
- 대상 커밋 (SmartBFT 포크): `d9d36397` (= baseline `32a628d6` + PR #2 metrics `fa0ae1ad` + PR #1 config `a575763c`/`08a25ba1`). 아래 `file.go:NNN` 은 전부 이 체크아웃 기준.
- 선행 노트: [batcher.md](batcher.md) (F), [leader-token-flow.md](leader-token-flow.md) (A), [test-harness.md](test-harness.md) (H), [00-summary.md](00-summary.md) 2절 11행·3절 3행
- 브랜치 순서: 3번 (`feat/pipeline-config` 뒤, `feat/pipeline-window` 앞). 플래그 `Configuration.AdaptiveBatch` (`pkg/types/config.go:93-95`, 기본 `false`).
- 문서 성격: 리뷰어가 **구현 전에** 확정할 설계. 의사코드 수준까지 내려가며, 새로 추가하는 API 는 `(신규)` 로 표시한다. 코드에서 확인하지 못한 것은 `[?]` 로 남긴다.

---

## 0. 한 줄 요약

리더의 `BatchBuilder` 가 읽는 세 파라미터(최대 요청 수 C, 최대 바이트 S, 타임아웃 T)를 `atomic.Pointer[BatchParams]` 뒤로 옮기고 `SetBatchParams` 를 추가한다(플래그와 무관, 동작 동일). `AdaptiveBatch=true` 이면 컨트롤러에 **`BatchTuner`(신규, 고루틴 없음)** 가 붙어, 결정(decide) 1건마다 (a) 직전 배치가 FULL 로 닫혔는지·TIMEOUT 으로 닫혔는지와 그때의 충만율 φ=n/C, (b) 풀 백로그 B, (c) 리더 측 제안→결정 지연 L 을 읽고, **C 는 "FULL 이고 백로그가 남았으면 ×1.25 (상한 = 설정값)", T 는 "TIMEOUT 으로 닫힌 배치의 φ 가 0.75 이상이면 ×1.25, 0.25 미만이면 ×0.75, 그 사이면 유지"** 의 히스테리시스 규칙(k=3 연속 증거)으로 조정하며, L 이 최근 최저치의 2배를 넘으면 C 를 ×0.75 로 되돌린다. C 와 T 를 서로 다른 신호가 각각 구동하므로 2차원 진동이 생기지 않고, 모든 갱신이 clamp 라 유계이며, 정상 부하에서는 dead band 안에 들어온 뒤 더 이상 규칙이 발화하지 않는다.

---

## 1. 목표와 비범위

### 1.1 조정 대상

| 파라미터 | 설정 필드 | 배처 내 사용 지점 | 조정 여부 |
|---|---|---|---|
| C — 배치 최대 요청 수 | `RequestBatchMaxCount` (`config.go:18-20`) | `batcher.go:37` 복사 → `:50,63,68` `NextRequests(b.maxMsgCount, …)` | **조정** (범위 `[C_min, C_cap]`, 5절) |
| T — 배치 타임아웃 | `RequestBatchMaxInterval` (`config.go:24-28`) | `batcher.go:39` 복사 → `:56` `time.After(b.batchTimeout)` | **조정** (범위 `[T_min, T_max]`, 5절) |
| S — 배치 최대 바이트 | `RequestBatchMaxBytes` (`config.go:21-23`) | `batcher.go:38` 복사 → `:50,63,68` | **고정** (설정값 그대로 전달; `BatchParams` 필드로는 존재) |

S 를 고정하는 이유 (10절 열린 질문 2 에서 재론):
1. `Validate()` 의 교차 제약 두 개가 S 를 기준점으로 삼는다 — `RequestBatchMaxCount > RequestBatchMaxBytes → error` (`config.go:182-184`), `RequestMaxBytes > RequestBatchMaxBytes → error` (`:185-187`). S 를 런타임에 낮추면 `RequestMaxBytes ≤ S` 가 깨져 큰 요청이 영원히 배치에 못 들어가는 상태가 될 수 있다 (`requestpool.go:313-316` 은 첫 요청부터 S 를 넘으면 빈 배치를 `full=true` 로 반환).
2. Fabric 통합은 S 를 채널 설정 `BatchSize.AbsoluteMaxBytes` 로부터 채운다 (`fabric/orderer/consensus/smartbft/util.go:454-456`). 이는 운영자가 정한 블록 크기 상한이므로 라이브러리가 임의로 움직일 값이 아니다.
3. 요청 크기가 균일한 벤치 부하에서 바이트 상한은 거의 바인딩되지 않는다 (기본 S=10 MiB, `RequestMaxBytes`=10 KiB → 1,000 건이 들어가야 바인딩; C 기본 100 이 먼저 닿는다). 제어 차원을 하나 늘릴 실익이 없다.

### 1.2 비범위

| 항목 | 이유 |
|---|---|
| 파이프라인 깊이 W | 4번 브랜치 `feat/pipeline-window` 가 들어온 뒤에만 의미가 있음 (00-summary 4절 "적응형 제어기의 W 동적 조절" 포기 항목). 본 브랜치는 `PipelineDepthMax=1` 전제. 6.2절에 W>1 때 바뀌는 지점을 미리 적어 둔다. |
| 요청 타임아웃 `RequestForwardTimeout/ComplainTimeout/AutoRemoveTimeout` | `Pool.ChangeOptions()` 는 `rp.stopped` 일 때만 허용 (`requestpool.go:149-152` `if !rp.stopped { Errorf("Trying to change timeouts but the pool is not stopped"); return }`) — reconfig 전용 경로다. 또 이미 무장된 타이머(`requestpool.go:248-251`, `511-514`, `544-547`) 는 옵션 교체로 바뀌지 않으므로 "옵션은 바뀌었는데 타이머는 옛 값" 상태가 생긴다. 팔로워의 complain 은 `Forward+Complain` 경과 후 `Complain(view, true)` (`controller.go:278`) 로 **뷰 체인지를 유발**하므로 오발 비용이 크다. 1차 범위에서는 고정하고, 대신 T 의 상한을 `RequestForwardTimeout` 의 분수로 묶어(6.1절) 복잡도를 제어기 쪽에 둔다. |
| 풀 용량 `RequestPoolSize` | 세마포어 (`requestpool.go:118`) 로 생성 시 고정. reconfig 조차 `// TODO handle reconfiguration of queue size` (`consensus.go:229`). |
| 팔로워 측 동작 | 배치 파라미터는 `NextBatch()` 를 부르는 리더만 사용한다 (2.2절). 팔로워는 아무것도 바꾸지 않는다. |
| `config.go:24-27` 주석과 코드의 불일치 ("첫 요청 도착 기준" vs 실제 "호출 시점 기준") | 원본 동작 유지. upstream 이슈 후보 (batcher.md 열린 문제 3). |

---

## 2. 플래그 분기

### 2.1 `AdaptiveBatch=false` — 동작 동일성 논증

현재 (`batcher.go:17-44`):
```go
type BatchBuilder struct {
    pool          RequestPool
    submittedChan chan struct{}
    maxMsgCount   int            // :20
    maxSizeBytes  uint64         // :21
    batchTimeout  time.Duration  // :22
    closeChan     chan struct{}
    closeLock     sync.Mutex
    metrics       *api.MetricsBatcher
}
// NewBatchBuilder (:30-44): maxMsgCount: int(maxMsgCount), maxSizeBytes, batchTimeout 를 그대로 복사 (:37-39)
```
`NextBatch()` (`:49-75`) 는 세 필드를 `:50`, `:56`, `:63`, `:68` 에서 락 없이 읽는다. 생성 지점은 `consensus.go:459` 한 곳이고, 생성 후 쓰는 코드는 없다.

리팩터 후:
```go
type BatchBuilder struct {
    pool          RequestPool
    submittedChan chan struct{}
    params        atomic.Pointer[BatchParams]   // (신규) 세 필드를 대체
    closeChan     chan struct{}
    closeLock     sync.Mutex
    metrics       *api.MetricsBatcher
}

func NewBatchBuilder(pool RequestPool, submittedChan chan struct{}, maxMsgCount uint64, maxSizeBytes uint64,
    batchTimeout time.Duration, metrics *api.MetricsBatcher) *BatchBuilder {
    …
    b.params.Store(&BatchParams{MaxMsgCount: maxMsgCount, MaxSizeBytes: maxSizeBytes, Timeout: batchTimeout})
    return b
}

func (b *BatchBuilder) NextBatch() [][]byte {
    p := b.params.Load()                       // 호출당 1회 스냅샷
    maxCount := int(p.MaxMsgCount)             // 기존 :37 의 int 변환을 여기로
    currBatch, full := b.pool.NextRequests(maxCount, p.MaxSizeBytes, true)
    if full { b.countBatch(currBatch, api.ReasonBatchFull); return currBatch }
    timeout := time.After(p.Timeout)
    for {
        select {
        case <-b.closeChan: return nil
        case <-timeout:
            currBatch, _ = b.pool.NextRequests(maxCount, p.MaxSizeBytes, false)
            b.countBatch(currBatch, api.ReasonBatchTimeout); return currBatch
        case <-b.submittedChan:
            currBatch, full = b.pool.NextRequests(maxCount, p.MaxSizeBytes, true)
            if full { b.countBatch(currBatch, api.ReasonBatchFull); return currBatch }
        }
    }
}
```
동일성 근거:
- 생성자 인자가 그대로 `params` 초기값이 되고, 플래그 OFF 에서는 **`SetBatchParams` 를 부르는 코드가 없다** (호출자는 `BatchTuner` 하나이며 OFF 면 `nil`, 2.3절). 따라서 모든 `NextBatch()` 호출이 같은 포인터를 로드하고, 세 값은 생성 시점 값과 바이트 단위로 같다.
- 호출 순서·`NextRequests` 인자·`time.After` 시작 시점·`countBatch` 호출 위치가 모두 원본과 같다. 바뀐 것은 "구조체 필드 3개 읽기" → "포인터 1회 atomic load + 필드 3개 읽기" 뿐이다 (비용 ~1 ns, `NextBatch` 호출당 1회).
- `Close/Closed/Reset` (`:89-115`) 은 건드리지 않는다.
- 기존 단위 테스트 `TestBatcherBasic/WhileSubmitting/Close/Reset/CountsBatchesByReason` (`batcher_test.go:29,115,173,198,251`) 이 무수정 통과해야 한다 (8절).
- **새 고루틴은 어느 모드에서도 시작하지 않는다**: `BatchTuner` 는 이벤트 구동(2.3절·4.0절)이라 ON 에서도 고루틴을 만들지 않고, OFF 에서는 객체 자체가 없다. "`AdaptiveBatch=false` 면 제어기 고루틴이 시작되지 않는다" 는 요구는 자명하게 만족한다.

### 2.2 `AdaptiveBatch=true` — 어디서 무엇이 시작되나

**배치 파라미터를 소비하는 쪽은 리더만이다.** `NextBatch()` 의 유일한 호출자는 `controller.go:467` (`propose()`), 그 호출자는 `run()` 의 `case <-c.leaderToken: c.propose()` (`:496-497`) 이고, 토큰은 리더만 받는다 (`startView :373-381`, `decide :566-568`). 팔로워 노드에도 `BatchBuilder` 객체는 생기지만 (`consensus.go:459` 는 모든 노드가 실행) `NextBatch()` 가 호출되지 않는다.

**팔로워가 배치 크기를 검사하는가 — 라이브러리·Fabric 모두 검사하지 않는다.**
- 라이브러리: 팔로워가 pre-prepare 를 받는 경로는 `view.go:380-457 processProposal` → `:415 verifyProposal` → `:584-635`. 그 안의 검사는 `v.Verifier.VerifyProposal(proposal)` (`:586`, 애플리케이션 위임), 메타데이터의 뷰 번호 (`:598-601`), 시퀀스 (`:603-606`), 뷰 내 결정 수 (`:608-611`), 검증 시퀀스 (`:613-617`), 직전 커밋 서명·블랙리스트 (`:619-632`) 뿐이다. **요청 수나 바이트 크기와 `RequestBatchMaxCount/MaxBytes` 를 비교하는 코드는 없다.** `grep -rn "RequestBatchMaxCount\|RequestBatchMaxBytes\|maxMsgCount" internal pkg` 의 비테스트 결과는 `batcher.go:20,30,37,50,63,68`, `config.go`, `consensus.go:459` 가 전부다 — 두 설정값을 읽는 라이브러리 코드는 `consensus.go:459 → batcher.go:37-39` 한 경로뿐이다.
- 테스트 하네스: `test/test_app.go:213-222 VerifyProposal` 은 payload 를 디코드해 `RequestInfo` 목록만 돌려준다. 크기 검사 없음.
- Fabric: `orderer/consensus/smartbft/verifier.go:98-121 VerifyProposal` → `verifyHashChainAndDataHash`, `verifyBlockDataAndMetadata (:263-329)` → 블록 데이터 비어 있음·메타데이터·last config·`validateTransactions`(트랜잭션별 검증). 블록 내 트랜잭션 **개수** 상한 검사는 없다. `BatchSize.MaxMessageCount` 는 `util.go:455` 에서 `RequestBatchMaxCount` 로만 흘러가 리더의 배처에 쓰인다. `AbsoluteMaxBytes` 는 `msgprocessor/sizefilter.go:39` 에서 **개별 envelope** 크기 필터로 쓰인다 (배치 합계가 아님). (`blockcutter.go:121` 의 `MaxMessageCount` 검사는 SmartBFT 체인이 쓰지 않는 블록커터 경로다.)

따라서 "팔로워가 자기 설정보다 큰 배치를 거부한다" 는 제약은 **코드에 존재하지 않는다**. 적응 범위의 상한은 팔로워 거부가 아니라 다음 세 가지 **설정 불변식** 에서 나온다:
1. `C ≤ RequestBatchMaxCount` (설정값) — `Validate()` 가 설정값에 대해서만 `W × RequestBatchMaxCount ≤ RequestPoolSize` (`config.go:209-218`, depth>1 일 때) 와 `RequestBatchMaxCount ≤ RequestBatchMaxBytes` (`:182-184`) 를 증명해 두었다. 설정값을 넘지 않으면 두 불변식은 자동으로 유지된다.
2. Fabric 에서 `MaxMessageCount` 는 채널 설정으로 모든 orderer 가 합의한 값이다. 검사 코드가 없더라도 그 계약을 라이브러리가 넘어서면 운영자 관점의 블록 크기 상한이 무의미해진다.
3. 풀 용량: `C ≤ RequestPoolSize / W` 가 아니면 in-flight 배치만으로 세마포어가 차 `Submit()` 이 `SubmitTimeout` 까지 블록한다 (`requestpool.go:221-229`). W=1 에서 `Validate()` 는 이 검사를 하지 않으므로(`:210` 가드) 제어기가 `C_cap = min(RequestBatchMaxCount, RequestPoolSize / W)` 로 직접 막는다.

**T 는 어디서도 검사되지 않는다** (`batcher.go:56` 외 사용처 없음) → `[T_min, T_max]` 는 전적으로 제어기 정책 (6.1절: complain 예산).

**결론 — 제어기는 리더에서만 "발화"하되, 객체는 모든 노드가 가진다.** 리더 전용 생성은 리더십 변화를 추적해야 하므로 오히려 복잡하다. 대신 (i) `continueCreateComponents()` (`consensus.go:458-469`) 가 플래그 ON 이면 모든 노드에 `BatchTuner` 를 만들어 컨트롤러에 붙이고, (ii) 발화 지점을 **리더만 통과하는 코드 경로**에 둔다 — `propose()` (토큰 보유자 = 리더) 와 `decide()` 의 `if iAm, _ := c.iAmTheLeader(); iAm { … }` 블록 (`controller.go:566-568`). 팔로워의 tuner 는 영원히 호출되지 않으며 메모리 수백 바이트만 차지한다.

### 2.3 와이어링 (`pkg/consensus/consensus.go`)

```go
// consensus.go:458 continueCreateComponents (수정)
func (c *Consensus) continueCreateComponents() {
    batchBuilder := algorithm.NewBatchBuilder(c.Pool, c.submittedChan, c.Config.RequestBatchMaxCount,
        c.Config.RequestBatchMaxBytes, c.Config.RequestBatchMaxInterval, c.Metrics.MetricsBatcher)   // :459 그대로
    leaderMonitor := …                                                                                // :460 그대로
    c.controller.RequestPool = c.Pool
    c.controller.Batcher = batchBuilder
    c.controller.LeaderMonitor = leaderMonitor
    if c.Config.AdaptiveBatch {                                                                       // (신규)
        c.controller.BatchTuner = algorithm.NewBatchTuner(
            algorithm.TunerConfigFromConfiguration(c.Config),   // 5.2절 파라미터 유도
            batchBuilder, c.Pool, c.Metrics.MetricsBatcher, c.Logger)
    }
    … // :465-468 그대로
}
```
- `Controller` 에 필드 `BatchTuner *BatchTuner` (신규, `nil` = OFF) 추가 (`controller.go:77-132` 구조체).
- `createComponents()` (`consensus.go:393-456`) 가 reconfig 마다 `c.controller = &algorithm.Controller{…}` (`:425`) 로 컨트롤러를 **새로 만들고**, 이어서 `continueCreateComponents()` (`:230`) 가 새 `BatchBuilder` 와 새 `BatchTuner` 를 만든다. 즉 reconfig 뒤 제어기 상태는 새 설정의 초기값으로 자동 리셋된다 (3.5절·6.3절).

---

## 3. `SetBatchParams` API

### 3.1 타입과 메서드 `(신규)`

```go
// internal/bft/batcher.go

// BatchParams are the parameters NextBatch reads once per call.
type BatchParams struct {
    MaxMsgCount  uint64        // 배치 최대 요청 수 (설정 RequestBatchMaxCount 와 같은 타입)
    MaxSizeBytes uint64        // 배치 최대 바이트
    Timeout      time.Duration // 배치 타임아웃
}

// SetBatchParams replaces the parameters the next NextBatch call reads. A call of NextBatch that is
// already in progress keeps the parameters it loaded when it started. Zero values are normalised:
// MaxMsgCount 0 -> 1, MaxSizeBytes 0 -> 1, Timeout <= 0 -> the previous Timeout.
func (b *BatchBuilder) SetBatchParams(p BatchParams)

// BatchParams returns the parameters the next NextBatch call will read.
func (b *BatchBuilder) BatchParams() BatchParams
```

`Batcher` 인터페이스 (`controller.go:30-35`) 에 두 메서드를 추가한다:
```go
type Batcher interface {
    NextBatch() [][]byte
    Close()
    Closed() bool
    Reset()
    SetBatchParams(BatchParams)   // (신규)
    BatchParams() BatchParams     // (신규)
}
```
→ `internal/bft/mocks/batcher.go` 재생성 필요 (`AGENTS.md`: `rm -f internal/bft/mocks/*.go && mockery --config .mockery.yml`; `.mockery.yml` 의 `Batcher` 항목은 이미 있음). 기존 컨트롤러 테스트는 `batcher.On("NextBatch")…` 만 설정하므로 (`controller_test.go:88,138,291`) 새 메서드가 호출되지 않는 OFF 경로에서는 영향이 없다.

> 대안: `BatchTuner` 가 `*BatchBuilder` 구체 타입을 직접 들면 인터페이스·목 변경이 없다. 그러나 컨트롤러 단위 테스트에서 "결정 뒤 `SetBatchParams` 가 호출되었다" 를 목으로 검증하려면 인터페이스에 있어야 하고, 00-summary 2절 11행도 인터페이스 추가로 결정했으므로 인터페이스 추가를 채택한다.

### 3.2 구현 — atomic swap

```go
func (b *BatchBuilder) SetBatchParams(p BatchParams) {
    prev := b.params.Load()
    if p.MaxMsgCount == 0  { p.MaxMsgCount = 1 }
    if p.MaxSizeBytes == 0 { p.MaxSizeBytes = 1 }
    if p.Timeout <= 0      { p.Timeout = prev.Timeout }
    b.params.Store(&p)                       // 새 포인터 publish; 진행 중인 NextBatch 는 옛 포인터를 계속 사용
    b.metrics.MaxCount.Set(float64(p.MaxMsgCount))              // 7절 (신규 게이지)
    b.metrics.TimeoutSeconds.Set(p.Timeout.Seconds())           // 7절 (신규 게이지)
}

func (b *BatchBuilder) BatchParams() BatchParams { return *b.params.Load() }
```
- 포인터 교체라 쓰기·읽기 모두 wait-free. 구조체 자체는 불변(immutable) 으로 다루고 매번 새 포인터를 저장하므로 읽는 쪽이 필드 세 개를 **일관된 한 세트**로 본다 (필드별 atomic 세 개로 하면 "새 C + 옛 T" 조합이 보일 수 있다).
- 정규화는 "절대 깨지면 안 되는" 바닥(0 방지)만 한다. 정책적 clamp(`[C_min, C_cap]`, `[T_min, T_max]`) 는 `BatchTuner` 책임 (5.2절). 배처를 멍청하게 유지해 테스트에서 임의 값을 넣을 수 있게 한다.

### 3.3 `NextBatch` 의 읽기 지점 — 호출당 1회 스냅샷

2.1절 코드처럼 `p := b.params.Load()` 를 **함수 진입 시 한 번** 수행하고, `:50/:56/:63/:68` 에 해당하는 네 지점이 모두 `p` 를 쓴다. `select` 루프 안에서 다시 로드하지 않는다.
- 이유: `time.After(p.Timeout)` (`:56`) 은 한 번 무장되므로 중간에 T 를 바꿔도 타이머는 재무장되지 않는다. 루프에서 C 만 다시 읽으면 "타이머는 옛 T, 카운트는 새 C" 라는 반쪽 상태가 된다. 한 번 읽는 것이 설명하기 쉽고, 지연은 최대 **한 번의 `NextBatch` 호출** (≤ 옛 T ≤ `T_max`) 이다.
- 본 브랜치의 와이어링에서는 그 지연조차 0 이다: 제어기는 `decide()` 안에서, 즉 `acquireLeaderToken()` (`controller.go:567`) **직전**에 `SetBatchParams` 를 부르고, 다음 `NextBatch()` 는 그 토큰을 소비한 `propose()` 에서 시작된다 (`:496-497 → :467`). 같은 고루틴(run) 의 순차 실행이므로 새 파라미터가 반드시 다음 호출에 보인다.
- batcher.md 가 제안한 `paramsChanged chan struct{}` 로 타이머를 재무장하는 변형은 **채택하지 않는다** (필요 없고 select 분기가 늘어난다).

### 3.4 호출 권한

| 호출자 | 허용 | 비고 |
|---|---|---|
| `BatchTuner` (리더 경로, run 고루틴) | 예 | 유일한 운영 호출자 |
| 테스트 | 예 | 단위 테스트에서 임의 값 주입 |
| `pkg/consensus` 외부 (애플리케이션) | 아니오 | `internal/bft` 패키지이므로 export 되지 않음. 운영자가 바꾸는 길은 reconfig 뿐 (원본과 동일) |

### 3.5 reconfig 상호작용

`reconfig()` (`consensus.go:186-261`): `controller.StopWithPoolPause()` (`:193`, 내부에서 `Batcher.Close()` `controller.go:863`) → `c.Config = reconfig.CurrentConfig` (`:207`) → `createComponents()` (`:221`, 컨트롤러 재생성) → `continueCreateComponents()` (`:230`, **새 `BatchBuilder` + 새 `BatchTuner`**) → `startComponents` (`:254`). 옛 builder/tuner 는 참조가 끊겨 GC 된다. 따라서:
- reconfig 뒤 배치 파라미터는 새 설정값으로 시작한다 (6.3절 "reconfig 는 리셋" 이 코드 구조상 자동 성립).
- `BatchTuner` 는 **컨트롤러의 필드**이므로 "재부착" 코드를 따로 쓸 필요가 없다 — 2.3절의 `continueCreateComponents` 한 곳이 생성·부착을 모두 한다. 리뷰 포인트: `Consensus` 구조체에 tuner 를 들고 있지 **말 것** (수명이 컨트롤러와 같아야 함).

### 3.6 뷰 체인지·`Reset/Close` 상호작용

- `Batcher.Reset()` (`batcher.go:111-115`) 은 `closeChan` 만 새로 만든다 → `params` 는 유지된다. 호출 지점은 `changeView()` 의 새 리더 분기 (`controller.go:411-413`) 한 곳.
- `Batcher.Close()` 는 `Sync :439`, `AbortView :448`, `ViewChanged :458`, `Stop :846`, `StopWithPoolPause :863` 에서 호출되며 `params` 를 건드리지 않는다.
- 제어기 상태 리셋은 `startView()` 의 리더 분기 (`controller.go:373-381`) 에서 `c.BatchTuner.Reset()` 를 호출해 수행한다 (6.4절). 이때 tuner 가 `SetBatchParams(초기값)` 를 명시적으로 불러 builder 를 설정값으로 되돌린다 — `Batcher.Reset()` 이 params 를 되돌리지 않기 때문이다.

---

## 4. 신호 — 측정 지점·비용·갱신 주기

### 4.0 제어기의 실행 모델 (신호 표를 읽기 위한 전제)

`BatchTuner` 는 **고루틴·티커 없이** 컨트롤러 run 고루틴의 세 지점에서 호출된다 (모두 리더만 통과):

| 훅 (신규) | 호출 위치 | 시점 | 하는 일 |
|---|---|---|---|
| `OnBatch(batch, p BatchParams, waited time.Duration)` | `propose()` — `controller.go:467` `NextBatch()` 반환 직후, `:472 GetMetadata` 전 | 배치 1개 제안 직전 | 샘플 `{n=len(batch), bytes, p, proposedAt=now}` 를 `pending` 에 보관 (W=1: 최대 1개). 유휴 타이머 리셋 |
| `OnIdle()` | `propose()` — `controller.go:468-470` `len(nextBatch)==0` 분기, `acquireLeaderToken()` 전 | 빈 배치(유휴 폴) 마다, 주기 ≈ T | 유휴 경과 시간을 보고 완화 규칙(5.3절 R5) 발화 |
| `OnDecision(n int)` | `decide()` — `controller.go:565 MaybePruneRevokedRequests()` 뒤, `:566-568` 블록 안 `acquireLeaderToken()` **직전** (리더 && 같은 뷰 && 미중단 조건을 이미 통과한 지점) | 결정 1건마다 | `pending` 소비 → L, reason, φ, B, A 계산 → 규칙 적용 → 필요 시 `SetBatchParams` |
| `Reset()` | `startView()` 리더 분기 `controller.go:373-381` | 뷰 시작 (리더일 때) | 상태를 초기값으로, `SetBatchParams(초기값)` |

왜 티커가 아닌가: (1) run 고루틴이 이미 `Batcher` 의 유일한 사용자라 **새 동시성이 생기지 않는다** (단, 4번 브랜치의 proposer 분리를 대비해 tuner 내부에 `sync.Mutex` 를 둔다 — 비용은 비경합 락 ~20 ns); (2) 결정은 "라운드 1회" 라는 자연스러운 샘플 단위다; (3) 부하가 없으면 결정도 없어 조정할 것도 없고, 유휴 완화는 빈 배치 폴(주기 T) 로 충분히 자주 호출된다; (4) leader-token-flow.md 가 지적한 run 루프 블로킹 (`NextBatch` 가 run 고루틴을 점유) 을 **악화시키지 않는다** — 세 훅은 모두 O(1) 산술 + atomic 로드 + `Pool` 락 1회(아래) 이며 `NextBatch` 밖에서 실행된다. T 의 상한 `T_max` 는 블로킹 시간의 상한을 늘리지만, W=1 에서는 토큰 보유 중 결정이 도착할 수 없고 (A 노트 "지금 안전한 이유"), 뷰 체인지는 `Batcher.Close()` 로 즉시 깨우므로 응답성에 영향이 없다. 리더 하트비트는 별도 고루틴(`HeartbeatMonitor`) 이라 T 와 무관하다.

### 4.1 신호 표

| # | 신호 | 측정 지점 (현재 코드) | 현재 API | 필요한 변경 `(신규)` | 비용 | 갱신 주기 | 제어 법칙에서의 역할 |
|---|---|---|---|---|---|---|---|
| S1 | 풀 길이 (백로그 B) | `len(rp.existMap)` / `rp.fifo.Len()`; 게이지 기록 `requestpool.go:263` (Submit), `:396` (deleteRequest) | `Pool.Size()` `:286-291` — **배타 `Lock`**; 비테스트 호출자 0 | `rp.count atomic.Int64` 미러를 `:262 PushBack` 직후와 `:395 fifo.Remove` 직후에 갱신하고 `Size()` 가 `int(rp.count.Load())` 를 반환하도록 변경 (게이지 쓰기 지점과 같은 자리). 의미·값은 동일 | 미러 갱신 ~1 ns (이미 Lock 안); 읽기 wait-free. (변경하지 않더라도 `OnDecision` 당 1회 Lock 이라 경합은 미미하나, Submit 폭주 시 run 고루틴이 풀 락에 줄서는 것을 피하기 위해 미러를 채택) | 결정마다 1회 읽음 | R1 의 조건 `B > 0` (결정 직후 = `removeDeliveredFromPool :523` 뒤 남은 요청 = 다음 배치를 기다리는 수요) |
| S2 | 풀 바이트 | `rp.sizeBytes` `:65`; 갱신 `:279` (+), `:386` (−); 게이지 `:280`, `:387` | 없음 | `Pool.SizeBytes() uint64` (신규) — `rp.bytes atomic.Uint64` 미러, 같은 두 지점 | 동일 | 결정마다 | 1차 법칙에서는 **미사용** (S 고정). 메트릭·대안 법칙용 |
| S3 | 최고령 요청 나이 A | `fifo.Front().Value.(*requestItem).additionTimestamp` (`:74`, 설정 `:259`) | 없음 | `Pool.OldestAge() time.Duration` (신규): `RLock` → `Front()` nil 이면 0, 아니면 `time.Since(additionTimestamp)` | `RLock` 1회 + O(1). Submit(`:233 Lock`)·NextRequests(`:297 Lock`) 와 짧게 경합 | 결정마다 1회 | 가드 G1 (6.1절 complain 예산): `A ≥ θ_A · RequestForwardTimeout` 이면 drain 모드 |
| S4 | 배치 종료 사유 (FULL/TIMEOUT) 와 충만율 φ | `batcher.go:52,70` (FULL), `:64` (TIMEOUT) 에서 `countBatch` 로 카운터만 증가. 판정식은 `requestpool.go:323-325` `fullS := totalSize >= maxSizeBytes; fullC := len(batch) == maxCount` | 없음 (카운터는 쓰기 전용) | **배처 변경 없음** — tuner 가 `OnBatch` 에서 받은 `batch` 와 스냅샷 `p` 로 재계산: `n := len(batch)`, `bytes := Σ len(req)`; `reason = FULL ⇔ n ≥ p.MaxMsgCount ∨ bytes ≥ p.MaxSizeBytes`, `φ = n / p.MaxMsgCount`. (경계: `requestpool.go:313-316` 의 "다음 요청이 S 를 넘어 full=true" 는 재계산에서 TIMEOUT 으로 보이지만, S 는 고정이고 요청 크기 균일 가정에서 극히 드묾. 정확성이 필요하면 `BatchBuilder.LastBatchReason() string` (신규) 로 대체 가능 — 열린 질문 5) | O(n) 바이트 합 (n ≤ C_cap, 이미 배치를 만든 비용에 비해 무시 가능) | 제안마다 | R1 (FULL), R2/R3 (TIMEOUT 의 φ) |
| S5 | 제안→결정 지연 L (리더 관점 라운드 지연) | 시작: `controller.go:474 c.currView.Propose(proposal)` 직전; 끝: `controller.go:516 decide(d)` 진입 (Deliver `:518` 전) | 없음. 팔로워 포함 노드별 PP→commit 지연은 `view.go:371-378 observeBatchProcessingLatency` 가 히스토그램(쓰기 전용)에 기록 | **View·메트릭 인터페이스 변경 없음** — `OnBatch` 가 `proposedAt` 을 `pending` 샘플에 저장, `OnDecision` 이 `time.Since`. W=1 이므로 샘플은 항상 1개. (W>1 대비: `decide :554-557` 이 이미 `md.GetLatestSequence()` 를 풀므로 `map[seq]sample` 로 확장 가능.) 대안: `View` 에 `OnRoundLatency func(seq uint64, d time.Duration)` 필드를 두고 `:376` 뒤에서 호출, `ProposalMaker.NewProposer` (`util.go:286-`) 가 주입 — 팔로워 관점·Deliver 제외 값이 필요할 때만 | `time.Now` 2회 | 결정마다 | 가드 G2 (지연 팽창 시 C 축소), `L_fast`/`L_ref` EWMA |
| S6 | WAL append 지연 | `pkg/wal/writeaheadlog.go:424-426` (`begin := time.Now(); err := w.append(record); LatencyAppend.Observe(…)`). 라이브러리 측 호출은 `internal/bft/state.go:55 ps.WAL.Append(b, truncateTo)` 한 곳 (PP 저장 `view.go:442`, commit 저장 `:539`) | 히스토그램만. WAL 객체는 **애플리케이션이 생성** (`test_app.go:482-486`, Fabric) → 라이브러리가 `wal.Options` 에 옵저버를 꽂을 수 없음 | `PersistedState` 에 선택 필드 `OnAppend func(time.Duration)` (신규) 를 두고 `state.go:55` 전후로 측정해 호출. tuner 는 `atomic` EWMA 로 받음 | `time.Now` 2회 / Append | PP·commit 저장마다 (라운드당 2회) | 1차 법칙 **미사용** (L 에 이미 포함). 메트릭·진단용. 커밋 (b) 에 포함하되 제어 입력으로 쓰는 것은 열린 질문 6 |
| S7 | 요청 도착률 λ | `requestpool.go:264 CountOfRequestPoolAll.Add(1)` (Submit 성공) | 카운터(쓰기 전용) | `rp.submitted atomic.Uint64` (신규) 를 같은 지점에서 증가, `Pool.Submitted() uint64` (신규) getter; tuner 가 결정 간 차분/경과시간으로 λ̂ | ~1 ns | 결정마다 차분 | 1차 법칙 미사용. 대안 법칙(5.5절 Little) 의 핵심 입력. 메트릭 |
| S8 | 백프레셔 (세마포어 실패) | `requestpool.go:224-228` | 카운터(쓰기 전용) | `rp.submitRejected atomic.Uint64` (신규) | ~1 ns | 발생 시 | 진단용. (C 를 키우는 것이 올바른 반응이며 R1 이 이미 그렇게 함) |
| S9 | 리더에서 Forward 타임아웃 발화 | `controller.go:256-261` (`iAm` 분기, "this node is the leader, nothing to do") | 로그만 | `Controller.leaderFwdTimeouts atomic.Uint64` (신규) 증가 | ~1 ns | 발생 시 | G1 의 보조 신호 (S3 가 `θ_A·Forward` 에서 먼저 발화하므로 정상 동작에서는 0 이어야 함 → 메트릭으로 노출하면 G1 미작동 감지) |

**모두 리더 in-process 이며 `pkg/metrics` 인터페이스(쓰기 전용, `pkg/metrics/provider.go`) 를 읽지 않는다.** `RequestPool` 인터페이스 (`controller.go:38-47`) 는 바꾸지 않는다 — tuner 는 작은 로컬 인터페이스 `poolStats interface { Size() int; OldestAge() time.Duration }` 를 받고 `*Pool` 이 이를 만족한다. 그래서 `mocks/request_pool.go` 재생성이 필요 없고, 테스트는 가짜 `poolStats` 를 넣는다.

---

## 5. 제어 법칙

### 5.1 설계 원칙

1. **각 노브는 하나의 신호 계열만이 구동한다.** C 는 "FULL + 백로그" 와 지연 가드가, T 는 "TIMEOUT 배치의 충만율" 이 움직인다. C 규칙이 T 를 건드리거나 T 규칙이 C 를 건드리지 않는다. (과제 지시의 예시 "FULL 이면 C 증가 **그리고 T 단축**" 은 채택하지 않는다 — 5.4절 (S2) 에서 포화 경계에서 극한 사이클을 만든다는 것을 보인다. FULL 상태에서 T 는 바인딩되지 않으므로 단축해도 얻는 것이 없다.)
2. **히스테리시스**: 변경은 k 개 연속 같은 방향 증거가 있을 때만, 변경 직후 연속 카운터를 0 으로 → 노브당 변경 빈도 ≤ 1/k 결정.
3. **곱셈 보폭 + 절대 clamp**: 상한/하한을 넘는 값은 절대 publish 되지 않는다.
4. **가드는 규칙보다 우선**하고, 가드가 발화하면 그 결정에서는 다른 규칙을 적용하지 않는다.
5. 모든 파라미터는 설정값으로부터 유도되는 기본값을 가진다 → 운영자가 새로 정할 숫자가 없다.

### 5.2 상태·파라미터

```go
// internal/bft/batchtuner.go (신규)

type TunerConfig struct {
    CMin, CInit, CCap uint64          // 배치 최대 요청 수의 하한·초기값·상한
    SFixed            uint64          // 고정 바이트 상한 (= RequestBatchMaxBytes)
    T0, TMin, TMax    time.Duration   // 타임아웃 초기(복귀)값·하한·상한
    Up, Down          float64         // 곱셈 보폭 u, d
    K                 int             // 히스테리시스 연속 증거 수
    PhiLo, PhiHi      float64         // TIMEOUT 배치 충만율 dead band
    Alpha             float64         // L_fast EWMA 계수
    Epsilon           float64         // L_ref 상향 드리프트
    Gamma             float64         // 지연 가드 배수
    ThetaA            float64         // 최고령 가드 (× ForwardTimeout)
    ForwardTimeout    time.Duration   // = RequestForwardTimeout
    IdleAfter         time.Duration   // 유휴 완화 간격
    CeilHold          int             // 가드로 내려간 뒤 상한 기억 유지 결정 수
}

func TunerConfigFromConfiguration(c types.Configuration) TunerConfig
```

| 기호 | 의미 | 기본값 (설정 유도) | `fastConfig` 예 (`test_app.go:28-46`) | `DefaultConfig` 예 (`config.go:102-125`) |
|---|---|---|---|---|
| `C_cap` | C 상한 | `min(RequestBatchMaxCount, RequestPoolSize / EffectivePipelineDepth())` | min(10, 40/1) = 10 | min(100, 400) = 100 |
| `C_min` | C 하한 | 1 | 1 | 1 |
| `C_init` | 시작값 (뷰 시작·reconfig·유휴 복귀 목표) | `max(C_min, ⌈C_cap / 4⌉)` | 3 | 25 |
| `u` | 증가 계수 | 1.25 (최소 +1) | | |
| `d` | 감소 계수 | 0.75 (최소 −1) | | |
| `k` | 연속 증거 수 | 3 | | |
| `T_0` | 시작·복귀 타임아웃 | `RequestBatchMaxInterval` | 10 ms | 50 ms |
| `T_min` | T 하한 | `max(T_0 / 4, 1 ms)` | 2.5 ms | 12.5 ms |
| `T_max` | T 상한 | `min(4·T_0, RequestForwardTimeout / 4)` | min(40, 125) = 40 ms | min(200, 500) = 200 ms |
| `φ_lo`, `φ_hi` | dead band | 0.25, 0.75 | | |
| `α` | `L_fast` EWMA | 0.3 | | |
| `ε` | `L_ref` 상향 드리프트/결정 | 0.02 | | |
| `γ` | 지연 가드 배수 | 2.0 | | |
| `θ_A` | 최고령 가드 | 0.5 (× `RequestForwardTimeout`) | 250 ms | 1 s |
| `IdleAfter` | 유휴 완화 간격 | `10 · T_0` | 100 ms | 500 ms |
| `CeilHold` | 상한 기억 유지 | `10·k` = 30 결정 | | |

`C_init < C_cap` 인 이유: 적응의 가치는 "설정 상한은 운영자의 안전 상한이고, 제어기가 그 아래에서 **지연을 팽창시키지 않는 가장 큰 C** 를 찾는다" 는 데 있다. 경부하에서는 배치가 TIMEOUT 으로 닫혀 C 가 바인딩되지 않으므로 작은 C_init 은 비용이 없고, 중부하에서 FULL 이 연속되면 몇 결정 안에 상한까지 올라간다 (상한 `⌈log_{1.25}(C_cap/C_init)⌉ · k`: fastConfig 에서 ⌈log₁.₂₅(10/3)⌉·3 = 18 결정 — `mulUp` 의 "최소 +1" 때문에 실제 궤적은 3→4→5→7→9→10 으로 5회 × 3 = 15 결정; 기본 설정에서 ⌈log₁.₂₅ 4⌉·3 = 21 결정, 실제 25→32→40→50→63→79→99→100 으로 7회 × 3 = 21 결정). `C_init = C_cap` 로 두면 C 는 감소만 가능해 "부하에서 커진다" 는 성질을 보일 수 없다 (열린 질문 3).

런타임 상태:
```go
type BatchTuner struct {
    mu      sync.Mutex
    cfg     TunerConfig
    batcher Batcher
    pool    poolStats
    metrics *api.MetricsBatcher
    logger  api.Logger

    c, t          uint64, time.Duration   // 현재 C, T (= batcher.BatchParams() 와 항상 일치)
    lFast, lRef   float64                  // 초 단위; 0 = 미관측
    fullStreak, hiStreak, loStreak int
    cCeil         uint64; cCeilLeft int    // 가드 뒤 상한 기억 (TCP ssthresh 유사)
    lastBatchAt   time.Time                // 비어 있지 않은 배치가 마지막으로 제안된 시각 (유휴 판정)
    lastRelaxAt   time.Time
    pending       *batchSample             // W=1: 0 또는 1개
}
type batchSample struct { n int; bytes uint64; params BatchParams; proposedAt time.Time }
```

### 5.3 규칙 (의사코드)

```go
func (t *BatchTuner) OnBatch(batch [][]byte, p BatchParams, waited time.Duration) {
    t.mu.Lock(); defer t.mu.Unlock()
    var bytes uint64
    for _, r := range batch { bytes += uint64(len(r)) }
    t.pending = &batchSample{n: len(batch), bytes: bytes, params: p, proposedAt: time.Now()}
    t.lastBatchAt = t.pending.proposedAt
}

func (t *BatchTuner) OnIdle() {                                    // 빈 배치마다 (주기 ≈ T)
    t.mu.Lock(); defer t.mu.Unlock()
    now := time.Now()
    if now.Sub(t.lastBatchAt) < t.cfg.IdleAfter || now.Sub(t.lastRelaxAt) < t.cfg.IdleAfter { return }
    t.lastRelaxAt = now
    // R5 유휴 완화: (C_init, T_0) 로 한 단계씩 복귀
    newC := t.c
    if t.c > t.cfg.CInit { newC = max(t.cfg.CInit, mulDown(t.c)) }
    newT := t.t
    switch {
    case t.t > t.cfg.T0: newT = max(t.cfg.T0, mulDownD(t.t))
    case t.t < t.cfg.T0: newT = min(t.cfg.T0, mulUpD(t.t))
    }
    t.resetStreaks()
    t.apply(newC, newT, "RELAX")
}

func (t *BatchTuner) OnDecision(nDelivered int) {
    t.mu.Lock(); defer t.mu.Unlock()
    s := t.pending; t.pending = nil
    if s == nil { return }                        // 이 tuner 가 제안하지 않은 결정 (WAL 복원 in-flight 등) → 샘플 없음
    L := time.Since(s.proposedAt).Seconds()

    // 지연 추정기
    if t.lFast == 0 { t.lFast, t.lRef = L, L } else {
        t.lFast = t.cfg.Alpha*L + (1-t.cfg.Alpha)*t.lFast
        t.lRef  = min(t.lFast, t.lRef*(1+t.cfg.Epsilon))        // 최근 최저치 추적, 천천히 잊음
    }
    if t.cCeilLeft > 0 { t.cCeilLeft-- }

    full := s.n >= int(s.params.MaxMsgCount) || s.bytes >= s.params.MaxSizeBytes
    phi  := float64(s.n) / float64(s.params.MaxMsgCount)
    B    := t.pool.Size()                          // removeDeliveredFromPool 뒤 → 다음 배치를 기다리는 요청 수
    A    := t.pool.OldestAge()

    // G1 drain 가드: 리더 풀에 오래 머문 요청이 있다 → complain 예산 위험 → 최대 처리량 모드
    if A >= time.Duration(t.cfg.ThetaA*float64(t.cfg.ForwardTimeout)) {
        t.resetStreaks()
        t.apply(t.cfg.CCap, t.cfg.TMin, "DRAIN")
        return
    }
    // G2 지연 가드: 라운드 지연이 기준의 γ 배를 넘었다 → 배치가 라운드를 무겁게 만들고 있다 → C 축소
    if t.lRef > 0 && t.lFast > t.cfg.Gamma*t.lRef && t.c > t.cfg.CMin {
        t.cCeil, t.cCeilLeft = t.c, t.cfg.CeilHold
        t.resetStreaks()
        t.apply(max(t.cfg.CMin, mulDown(t.c)), t.t, "GUARD_DOWN")
        return
    }

    newC, newT := t.c, t.t
    if full {
        // R1: 가득 찼고 백로그가 남아 있다 → 더 큰 배치가 라운드당 더 많이 처리한다
        t.hiStreak, t.loStreak = 0, 0
        t.fullStreak++
        if t.fullStreak >= t.cfg.K && B > 0 {
            cap := t.cfg.CCap
            if t.cCeilLeft > 0 && t.cCeil < cap { cap = t.cCeil }     // 가드로 내려온 뒤에는 그 지점 위로 당분간 안 올라감
            newC = min(cap, mulUp(t.c))
            t.fullStreak = 0
        }
        // full && B == 0: 수요가 정확히 C 에 맞는다 → 아무것도 하지 않음 (R4)
    } else {
        t.fullStreak = 0
        switch {
        case phi >= t.cfg.PhiHi:        // R2: 거의 찼는데 타임아웃이 먼저 끊었다 → 조금 더 기다리면 꽉 찬다 (상각)
            t.loStreak = 0; t.hiStreak++
            if t.hiStreak >= t.cfg.K { newT = min(t.cfg.TMax, mulUpD(t.t)); t.hiStreak = 0 }
        case phi < t.cfg.PhiLo:         // R3: 기다려도 안 찬다 → 기다림은 지연 낭비 → T 축소
            t.hiStreak = 0; t.loStreak++
            if t.loStreak >= t.cfg.K { newT = max(t.cfg.TMin, mulDownD(t.t)); t.loStreak = 0 }
        default:                        // R4: dead band → 유지
            t.hiStreak, t.loStreak = 0, 0
        }
    }
    t.apply(newC, newT, "")
}

func (t *BatchTuner) Reset() {                    // startView (리더), 생성 직후
    t.mu.Lock(); defer t.mu.Unlock()
    t.pending = nil; t.lFast, t.lRef = 0, 0; t.resetStreaks(); t.cCeilLeft = 0
    t.lastBatchAt, t.lastRelaxAt = time.Now(), time.Now()
    t.apply(t.cfg.CInit, t.cfg.T0, "RESET")
}

func (t *BatchTuner) apply(c uint64, d time.Duration, why string) {
    c = clamp(c, t.cfg.CMin, t.cfg.CCap); d = clampD(d, t.cfg.TMin, t.cfg.TMax)
    if c == t.c && d == t.t && why == "" { return }
    // 7절 메트릭: 방향별 카운터
    t.c, t.t = c, d
    t.batcher.SetBatchParams(BatchParams{MaxMsgCount: c, MaxSizeBytes: t.cfg.SFixed, Timeout: d})
    t.logger.Debugf("Adaptive batch: max count %d, timeout %s (%s)", c, d, why)   // 신규 로그; 기존 15개 훅 문자열 불변
}

mulUp(c)   = max(c+1, ⌈u·c⌉)      mulDown(c)   = min(c-1, ⌊d·c⌋)   (c>1 일 때; c==1 이면 1)
mulUpD(t)  = ⌈u·t⌉                mulDownD(t)  = ⌊d·t⌋
```

### 5.4 안정성 논증

기호: 도착률 λ (일정), 라운드 지연 L(C) (C 에 대해 비감소), 배치 수 n.

**(S1) 유계성.** `apply` 가 유일한 쓰기 경로이고 항상 `clamp` 를 거친다 → 모든 시점에 `C ∈ [C_min, C_cap]`, `T ∈ [T_min, T_max]`. 초기값 `C_init ∈ [C_min, C_cap]`, `T_0 ∈ [T_min, T_max]` (정의상 `T_min ≤ T_0 ≤ T_max`: `T_0/4 ≤ T_0 ≤ 4T_0`, 단 `T_0 > ForwardTimeout/4` 인 비정상 설정이면 `T_max < T_0` 가 되어 clamp 가 T 를 `T_max` 로 내린다 — `Validate` 가 `ForwardTimeout ≥ 4·MaxInterval` 을 요구하지 않으므로 이 경우는 로그로 경고하고 clamp 에 맡긴다, 열린 질문 7).

**(S2) 노브 분리 → 2차원 극한 사이클 없음.** C 를 바꾸는 규칙은 {R1(FULL∧B>0, ↑), G2(지연, ↓), R5(유휴, ↓), G1(drain, ↑)}, T 를 바꾸는 규칙은 {R2(↑), R3(↓), R5, G1} 이다. R1~R4 중 어느 것도 두 노브를 동시에 움직이지 않는다. 반례로 과제 예시의 "FULL 이면 T 도 단축" 을 넣으면: `λ(T+L) ≈ C_cap` 인 포화 경계에서 FULL(B>0) → T↓ → 다음 라운드에 모이는 요청 수가 줄어 TIMEOUT(φ≈0.9) → R2 로 T↑ → 다시 FULL → … 로 T 가 영원히 톱니를 그린다. 분리하면 그 경계에서 FULL(B>0) 은 C 만 (이미 cap 이면 아무것도) 움직이고, TIMEOUT-high 는 T 만 올리므로 T 는 `T_max` 로 단조 수렴한 뒤 멈춘다. T 가 `T_max` 에 있어도 FULL 배치에는 바인딩되지 않으므로 지연을 해치지 않는다.

**(S3) T 의 1차원 단조 피드백 + dead band → 고정점 수렴.** λ 와 C 가 고정일 때 TIMEOUT 으로 닫히는 배치의 n 은 T 에 대해 비감소이므로 φ(T) 도 비감소다. R2/R3 는 φ 가 band 위면 T↑, 아래면 T↓, 안이면 유지인 **단조 피드백**이다. 한 번의 보폭이 band 를 뛰어넘지 않으면 (즉 `φ(u·T) ≤ u·φ(T)` 이고 `φ_hi/φ_lo = 3 > u = 1.25`), band 밖에서 출발한 T 는 단조로 band 로 들어오고 들어온 뒤 멈추거나 경계(`T_min`/`T_max`) 에서 멈춘다. 극한 사이클이 생기려면 band 를 건너뛰어야 하는데 band 폭(×3) 이 보폭(×1.25) 보다 넓으므로 불가능하다.

**(S4) C 의 수렴.** 포화 (`λ·L(C_cap) ≥ C_cap`, 즉 백로그가 항상 남음) 에서는 매 결정이 FULL∧B>0 → C 는 k 결정마다 ×1.25 로 단조 증가해 `⌈log_u(C_cap/C_init)⌉·k` 결정 안에 `C_cap` 에 도달하고 더 이상 어떤 규칙도 발화하지 않는다. 비포화면 FULL 이 k 번 연속 나오지 않거나 B=0 이라 C 는 바뀌지 않는다. G2 가 어떤 C* 에서 발화하면 C → d·C* 로 떨어지고 `cCeil = C*` 를 `CeilHold` 결정 동안 기억해 그 위로는 올라가지 않는다 → 이후 R1 은 C 를 `[d·C*, C*)` 안에서만 올리고, `CeilHold` 가 끝나야 C* 를 다시 시도한다. 즉 톱니(AIMD sawtooth) 가 생기더라도 진폭은 `C*·(1−d) = 25 %` 이하, 주기는 `≥ 2k` 결정, 그리고 상한 재시도 빈도는 `1/CeilHold` 로 감쇠한다. 이것이 TCP 의 ssthresh 와 같은 역할이다.

**(S5) 잡음 내성.** 단일 샘플(한 번의 FULL, 한 번의 느린 라운드) 은 아무 것도 바꾸지 못한다: R1~R3 는 k 연속 증거가 필요하고, G2 는 EWMA `L_fast`(α=0.3 → 1 샘플이 30 %) 가 `L_ref` 의 2배를 넘어야 한다. 예: `L_ref = 10 ms` 일 때 한 샘플이 100 ms 여도 `L_fast = 0.3·100 + 0.7·10 = 37 ms > 20 ms` → 발화한다. 이는 의도된 보수성이다 (한 번의 큰 스파이크는 실제 문제일 확률이 높다); 더 둔하게 하려면 α 를 낮춘다 (열린 질문 4).

**(S6) 정상 부하에서의 정적 상태 (case 분석).** 수렴 뒤 규칙이 발화하지 않는다:
- 포화: C=C_cap, 모든 배치 FULL∧B>0 → R1 조건은 참이지만 `min(cap, mulUp(C)) = C` 이므로 `apply` 가 no-op. T 규칙은 FULL 이라 미발화.
- 정확히 맞는 부하 (FULL∧B=0): R4 → 아무것도 없음.
- 비포화 steady: TIMEOUT 이고 φ 가 band 안 → R4. band 밖이면 (S3) 로 안으로 들어옴.
- 유휴: 결정이 없어 `OnDecision` 미호출; `OnIdle` 이 `(C_init, T_0)` 까지 유한 단계 뒤 더 이상 바꾸지 않음 (`newC == C && newT == T` 면 no-op).

**(S7) 변경 빈도 상한.** 노브당 `≤ 1` 변경 / k 결정 (R1~R3), 가드는 발화 시 streak 리셋이라 역시 k 결정 뒤에야 다음 규칙 변경이 가능. `SetBatchParams` 호출 빈도 ≤ 결정률.

### 5.5 대안 법칙 (비채택, 비교용)

**(A) Little 의 법칙 기반 크기 결정.** `C = ⌈λ̂ · L_target⌉`, `T = max(T_min, L_target − L̂)`. λ̂ 는 S7 의 차분, L̂ 는 S5 의 EWMA.
- 장점: 목표 지연 하나로 두 노브가 닫힌 형태로 나온다.
- 비채택 이유: (i) `L_target` 은 **설정에서 유도되지 않는 새 운영 파라미터**다 (자연스러운 기본값이 없다 — `MaxInterval` 은 "최대 대기" 이지 목표 지연이 아니다). (ii) λ̂ 는 경부하에서 분산이 매우 커 (결정 간격 수십 ms 에 요청 몇 개) 결정마다 C 가 크게 튀고, 안정화하려면 또 다른 EWMA 와 clamp 가 필요해 "단순" 하지 않다. (iii) 비포화에서 n 은 λ 가 결정하므로 C 를 λ̂·L_target 으로 잡는 것은 아무 효과가 없고, 포화에서는 λ̂ 가 **처리량** (= C/L) 에 묶여 자기참조가 된다 (λ̂ ≈ C/L → C = C·L_target/L → L_target 과 L 의 비로만 움직이는 가드와 동치). 즉 정보량이 5.3절의 G2 와 같다.

**(B) 목표 지연 PID.** `e = L_target − L̂`, `C += K_p e + K_i Σe + K_d Δe`.
- 비채택 이유: 플랜트 모델 `L(C, λ)` 없이 세 이득의 안정 범위를 논증할 수 없고, 배치 지연은 C 에 대해 계단형(WAL fsync, 메시지 크기) 이어서 미분항이 잡음을 증폭한다. 적분항은 포화 (`C_cap` 에 clamp 된 상태) 에서 windup 을 일으켜 부하가 빠질 때 과도한 축소를 만든다. 결과적으로 설명 가능한 파라미터가 없고 논문에서 "왜 이 값" 을 방어할 수 없다.

**채택 근거 요약.** 1차 법칙은 (1) 규칙 5개 + 가드 2개가 각각 한 문장으로 설명되고, (2) 파라미터 전부가 설정값에서 유도되며, (3) 유계성·단조 수렴·사이클 부재가 플랜트 모델 없이 규칙 구조만으로 증명되고 (5.4절), (4) 테이블 기반 단위 테스트로 궤적을 그대로 검증할 수 있다 (8절). 논문의 평가 장에서 "적응형 vs 고정" 비교를 할 때 결과를 규칙 발화 로그로 설명할 수 있다는 점이 결정적이다.

---

## 6. 상호작용

### 6.1 요청 타임아웃 체인 (complain 예산)

요청의 리더 풀 체류 시간 = 배치 대기 (≤ T + 유휴 폴 잔여 ≤ 2T) + 라운드 지연 L + Deliver + `removeDeliveredFromPool` (`controller.go:523`). 체인은 Submit 시각 기준 `Forward (2 s / fastConfig 500 ms) → Complain (20 s / 2 s) → AutoRemove (3 min)` (`requestpool.go:248-251 → 511-514 → 544-547`; 기본값 `config.go:108-110`, 하네스 `test_app.go:34-36`).
- **팔로워**의 Forward 만료 → 리더로 재전송 (`controller.go:263-264`), Forward+Complain 만료 → `Complain(view, true)` (`:278`) → 뷰 체인지 오발. **리더**의 Forward 만료 → 무시 (`:259`), Forward+Complain → `StopLeaderSendMsg` (`:273`, 하트비트 중단 → 팔로워 하트비트 타임아웃 유도).
- 설계 제약: `T_max ≤ RequestForwardTimeout / 4`. 따라서 `2·T_max ≤ Forward/2` 이고, 나머지 절반은 L + Deliver 예산이다. 추가로 G1 가드가 `A ≥ Forward/2` 에서 drain 모드 (`C_cap, T_min`) 로 전환해 Forward 자체가 리더에서 발화하는 일을 막는다 (S9 카운터가 0 인지로 확인).
- 팔로워 풀의 요청(클라이언트가 팔로워에 제출한 것) 은 Forward 뒤 리더로 전달되어 리더 풀에 들어오고, 그때부터 **리더 풀에서는 새 타임스탬프**로 추가되지만 (`requestpool.go:259`) 팔로워 쪽 타이머는 계속 간다 → 팔로워 complain 까지 남은 예산은 `Complain` (20 s / 2 s). `T_max` (200 ms / 40 ms) 는 이에 비해 두 자릿수 작다.

### 6.2 `PipelineDepthMax > 1` (4번 브랜치 이후)

| 변하는 것 | 본 브랜치에서 미리 해 두는 것 |
|---|---|
| 풀 제약 `W × C ≤ RequestPoolSize` 가 런타임에 성립해야 함 | `C_cap = min(RequestBatchMaxCount, RequestPoolSize / EffectivePipelineDepth())` 로 이미 W 를 반영 (`config.go:129-134`). W 는 설정값이라 런타임에 변하지 않음 (W 동적 조절은 비범위) |
| 백로그 B = `Size()` 에 in-flight(예약된) 요청이 섞임 (`NextRequests` 비파괴적, `requestpool.go:309`) | tuner 의 `poolStats` 를 `Size()` 대신 "예약 제외 대기 수" 를 돌려주는 메서드로 바꿀 수 있게 인터페이스로 격리 |
| 샘플이 동시에 W 개 | `pending *batchSample` → `map[seq]batchSample`; `OnBatch` 는 seq 를 받고, `OnDecision` 은 `decide :554-557` 가 푼 `md.GetLatestSequence()` 로 찾음 |
| `OnBatch` 가 proposer 고루틴, `OnDecision` 이 run 고루틴에서 호출 | tuner 내부 `sync.Mutex` 를 처음부터 둠 (4.0절) |
| 유휴 판정: W 개 proposer 가 각자 폴 | 폴 횟수가 아닌 **경과 시간** (`IdleAfter`) 기반으로 처음부터 설계 (5.3절 `OnIdle`) |
| T 의 의미가 "토큰을 받은 뒤 최대 대기" 로 바뀜 (batcher.md 열린 문제 3 리뷰 반영) | 법칙은 배치가 **왜 닫혔는지** (FULL/TIMEOUT) 와 φ 만 보므로 타이머 시작점이 바뀌어도 그대로 유효. 문서에 "T = 제안 가능해진 뒤 최대 대기" 로 정의를 고정 |
| 결정률이 W 배 → k 결정이 더 짧은 벽시계 | k 는 결정 단위 그대로 둠 (라운드 수 기준 히스테리시스가 의미상 맞음) |

### 6.3 reconfig

3.5절: 컴포넌트 전체 재생성이라 `(C_init, T_0)` 로 자동 리셋. 특별 처리 없음. 단 reconfig 로 `AdaptiveBatch` 가 true→false 로 바뀌면 `continueCreateComponents` 가 tuner 를 만들지 않아 OFF 가 되고, false→true 면 생성된다 — 하네스 변환기(`test/reconfig.go`) 는 PR #1 에서 이미 두 필드를 운반한다.

### 6.4 뷰 체인지

- `changeView()` (`controller.go:386-414`) → `abortView` (`:402`, 토큰 drain `:427`, `currView.Abort()` `:431`) → `startView` (`:409`) → 새 리더면 `Batcher.Reset()` (`:411-413`).
- tuner 는 `startView()` 의 리더 분기 (`:373-381`) 에서 `Reset()` → `(C_init, T_0)`, EWMA·streak·pending 초기화. 근거: 리더가 바뀌면 그 노드의 tuner 는 팔로워 시절 아무 샘플도 받지 않았으므로 상태가 낡았고, 같은 노드가 뷰 재시작(sync 뒤 `changeView(view, seq, dec)` `:503`, `:510`) 으로 리더를 유지해도 뷰 체인지 동안의 지연 샘플은 오염되어 있다. "새 뷰는 설정 상태에서 시작" 이 가장 설명하기 쉽다.
- `abortView` 뒤 늦게 도착한 옛 뷰의 결정은 `decide :533-540` 에서 `d.view != currView` 로 걸러져 `:566` 에 이르지 않으므로 `OnDecision` 이 호출되지 않는다 → `Reset()` 이 `pending=nil` 로 비운 것과 일관.
- WAL 에서 복원된 in-flight 제안 (`state.go`) 이 결정되면 `pending == nil` 이라 `OnDecision` 이 샘플 없이 반환한다 (5.3절 첫 줄).

### 6.5 proposer 고루틴 분리 (4번 브랜치)

- `propose()` 가 run 고루틴 밖으로 나가면 `OnBatch`/`OnIdle` 은 proposer 고루틴, `OnDecision`/`Reset` 은 run 고루틴에서 호출 → 뮤텍스 보호 (이미 설계에 포함).
- A 노트 R5 "빈 배치 시 토큰 반환 + 백오프: `NextBatch` 의 타임아웃이 백오프 역할, 어댑티브 배처가 이 대기를 조절" — 본 설계의 `T_min` 하한 (≥ 1 ms) 이 busy-loop 을 막는 백오프 바닥이다. `T_min` 을 0 으로 두면 유휴 리더가 `NextRequests` 를 바쁘게 돌게 되므로 하한을 두는 이유를 여기 기록한다.
- 타임아웃 의미 변화는 6.2절 표 참조.

### 6.6 로그 문자열

test-harness.md 가 열거한 15개 훅 문자열 (`"Processed proposal with seq %d"` `view.go:455`, `"%d processed commits for proposal with seq %d"` `:342`, `"Sequence: %d-->%d"` `:909`, `"Starting view with number %d, …"` `controller.go:383` 등) 은 **하나도 바꾸지 않는다**. tuner 의 로그는 전부 신규 `Debugf` 한 종류 (`"Adaptive batch: max count %d, timeout %s (%s)"`) 이며, 통합 테스트는 이 문자열에 훅을 걸 수 있다 (8.3절).

---

## 7. 메트릭 `(신규)`

`api.MetricsBatcher` (`pkg/api/metrics.go:278-314`) 패턴을 따른다 (네임스페이스 `consensus_smartbft_`):

| 이름 | 타입 | 라벨 | 기록 지점 | 의미 |
|---|---|---|---|---|
| `batcher_max_count` | Gauge | — | `BatchBuilder.SetBatchParams` 와 생성자 | 현재 C. OFF 에서는 설정값으로 한 번 기록되고 불변 |
| `batcher_timeout_seconds` | Gauge | — | 같은 지점 | 현재 T (초) |
| `batcher_count_of_adjustments` | Counter | `param ∈ {COUNT, TIMEOUT}`, `direction ∈ {UP, DOWN}` | `BatchTuner.apply` (값이 실제로 바뀐 노브만) | 조정 횟수. `RESET/RELAX/DRAIN/GUARD_DOWN` 도 방향으로 환원해 기록 |
| `batcher_count_of_guard_trips` | Counter | `reason ∈ {DRAIN, LATENCY}` | `OnDecision` 의 G1/G2 | 가드 발화 수 (논문: 가드가 얼마나 자주 개입했는지) |

```go
type MetricsBatcher struct {
    CountOfBatches     metrics.Counter   // 기존 (:280)
    MaxCount           metrics.Gauge     // 신규
    TimeoutSeconds     metrics.Gauge     // 신규
    CountOfAdjustments metrics.Counter   // 신규, labels param, direction
    CountOfGuardTrips  metrics.Counter   // 신규, label reason
    labels []string
}
```
`NewMetricsBatcher/With/Initialize` (`:286-307`) 에 네 항목을 추가하고 `Initialize` 에서 라벨 조합마다 `Add(0)`/`Set(0)` 으로 시계열을 선점한다 (기존 `:300-307` 과 같은 방식). 게이지 두 개는 **배처가** 기록한다 (OFF 에서도 현재 값이 보여 비교군 대시보드에 같은 시계열이 존재) 하고, 카운터 두 개는 tuner 가 기록한다. 히스토그램 버킷 `{0.005, …, 10}` (`:512`) 은 제어 해상도로 거칠다는 F 노트의 지적은 그대로 두되 (제어기는 메트릭을 읽지 않으므로 무관), `T` 게이지는 초 단위 float 이라 해상도 문제가 없다.

S6 (WAL append) 와 S7 (도착률) 은 이미 `wal_latency_append` (`pkg/wal/metrics.go:20-28`), `pool_count_of_elements_all` (`requestpool.go:264`) 로 노출되어 있어 새 메트릭이 필요 없다.

---

## 8. 테스트 계획

### 8.1 단위 — `internal/bft/batcher_test.go` (커밋 a)

| 테스트 | 내용 | 어서션 |
|---|---|---|
| 기존 5개 (`:29,115,173,198,251`) | 무수정 | 통과 (동작 동일성) |
| `TestBatchParamsDefaultsFromConstructor` | `NewBatchBuilder(…, 7, 2048, 10ms, nil)` | `BatchParams() == {7, 2048, 10ms}` |
| `TestSetBatchParamsAppliesToNextCall` | 풀에 요청 1개, `C=10, T=1 min` 으로 `NextBatch` 를 고루틴에서 호출(블록) → `SetBatchParams{1, 2048, 10ms}` → `Close()` 로 첫 호출 종료 → `Reset()` → 두 번째 `NextBatch` | 첫 호출 `nil` (Close), 두 번째 호출이 **10 ms 안에** 1건 반환 (FULL by count=1). `-race` 로 실행 |
| `TestSetBatchParamsNormalisesZero` | `{0, 0, 0}` 설정 | `BatchParams() == {1, 1, 이전 T}` |
| `TestSetBatchParamsConcurrent` | 고루틴 8개가 각 1,000회 `SetBatchParams` / 8개가 `BatchParams()` 읽기 | `-race` 깨끗; 읽힌 모든 스냅샷이 "쓴 값 집합 중 하나" (필드 혼합 없음 — 각 쓰기에 고유 조합 사용) |
| `TestBatcherTimeoutUsesSnapshot` | `T=50ms` 로 `NextBatch` 시작 → 10 ms 뒤 `SetBatchParams{…, T=1ms}` | 첫 호출은 ≥ 50 ms 후 반환 (진행 중 호출은 옛 T) |

### 8.2 단위 — `internal/bft/batchtuner_test.go` (커밋 c): 순수 함수 테이블 테스트

tuner 를 가짜 `Batcher`(기록만) 와 가짜 `poolStats`(테이블이 지정하는 `Size/OldestAge`) 로 구동하고, 시간은 `now func() time.Time` 주입으로 제어 (tuner 에 `clock` 필드).

| 테스트 | 입력 시퀀스 | 기대 궤적 |
|---|---|---|
| `TestTunerGrowsOnFullStreakWithBacklog` | fastConfig 유도 (C_init 3, cap 10). `OnBatch(n=C)`+`OnDecision` 반복, `Size()=5` | C: 3,3,3,**4**,4,4,**5**,…,**10**, 그 뒤 불변. 매 변경이 정확히 k=3 번째 결정에 발생 |
| `TestTunerHoldsWhenFullButNoBacklog` | 같은 입력, `Size()=0` | C 불변 (R4) |
| `TestTunerTimeoutHighFillLengthens` | `OnBatch(n=⌈0.8C⌉)` 반복 | T: 10,10,10,**13**,…, `T_max`(40 ms) 에서 고정; C 불변 |
| `TestTunerTimeoutLowFillShortens` | `n=1`, C=10 | T 단조 감소 → `T_min` 2.5 ms 고정 |
| `TestTunerDeadBandHolds` | `n=5`, C=10 (φ=0.5) | 아무 변경 없음 |
| `TestTunerLatencyGuardShrinksAndRemembersCeiling` | 결정 20회 L=10 ms (lRef 안정) → 이후 L=50 ms | 다음 결정에서 C ×0.75 (`GUARD_DOWN`), `cCeil` 기록; 이어지는 FULL 스트릭에서도 `CeilHold` 동안 C ≤ cCeil |
| `TestTunerDrainOnOldRequest` | `OldestAge() = 300 ms` (≥ 0.5×500 ms) | 즉시 `(C_cap, T_min)`; 가드 카운터 DRAIN +1 |
| `TestTunerIdleRelaxesToInitial` | C=10, T=40 ms 상태에서 시계를 `IdleAfter` 씩 전진하며 `OnIdle` | C: 10→7→5→3 (C_init 에서 멈춤), T: 40→30→22→16→12→10 ms (T_0 에서 멈춤); 더 호출해도 불변 |
| `TestTunerBounded` (속성) | 난수 시퀀스 10,000 스텝 (FULL/TIMEOUT, n, Size, Age, L 무작위) | 매 스텝 `C_min ≤ C ≤ C_cap`, `T_min ≤ T ≤ T_max`; `SetBatchParams` 호출마다 값이 이전과 다름 (no-op 호출 없음) |
| `TestTunerNoOscillationUnderSteadyLoad` | 합성 플랜트: λ 고정, `n = min(C, ⌊λ(T+L)⌉)`, `Size = max(0, λ(T+L) − C)`, L 고정. λ 를 {경부하, 경계, 포화} 세 값으로 | 각 경우 500 결정 뒤 마지막 200 결정에서 `SetBatchParams` 호출 수 == 0 (정적 상태). 경계 케이스에서 T 가 `T_max` 로 단조 수렴 (사이클 없음을 직접 확인) |
| `TestTunerResetOnStartView` | 임의 상태 → `Reset()` | `(C_init, T_0)`, pending nil, EWMA 0 |
| `TestTunerIgnoresDecisionWithoutSample` | `OnDecision` 만 호출 | 변경 없음, 패닉 없음 |
| `TestTunerConfigFromConfiguration` | `fastConfig`, `DefaultConfig`, `T_0 > Forward/4` 인 비정상 설정 | 표 5.2 의 값; 비정상 설정에서 `T_max < T_0` 이고 생성 직후 T 가 `T_max` 로 clamp |

### 8.3 통합 — `test/adaptive_batch_test.go` (커밋 e)

공통: `t.Parallel()`, `t.TempDir()`, `newNodeWithConfig` (`test_app.go:460`) 로 4노드, `cfg := fastConfig; cfg.SyncOnStart = true; cfg.AdaptiveBatch = true; cfg.ViewChangeTimeout = 10s; cfg.LeaderHeartbeatTimeout = 10s` (PR #1 테스트 `pipeline_config_test.go:21-26` 패턴). `Delivered` 는 버퍼 100 (`test_app.go:470`) 이라 노드별 drain 고루틴 필수.

| 테스트 | 시나리오 | 어서션 |
|---|---|---|
| `TestAdaptiveBatchGrowsUnderLoad` | 클라이언트 3개(`ClientID` 별 `ID` 1..100) 가 `doInBackground` (`basic_test.go:3455`) 로 리더(노드 1) 에 총 300건 제출 (풀 40 이라 `Submit` 이 블록·재시도되므로 고루틴에서) | (1) 4노드 모두 300건 수신, seq 순 레코드가 노드 간 `require.Equal`; `Deliver` 의 중복 패닉 없음. (2) 첫 배치 크기 ≤ `C_init`(3). (3) 배치 크기 10 (= `RequestBatchMaxCount`) 인 레코드가 존재 (상한까지 성장). (4) 배치 크기 최대값 ≤ 10 (상한 준수). (5) 리더 로그 훅 `"Adaptive batch: max count 10"` 가 관측됨 (`waitForSignal` 30 s 상한, `wal_truncation_test.go:86`) |
| `TestAdaptiveBatchRelaxesWhenIdle` | 위와 같이 300건 → 모두 전달 확인 → `2·IdleAfter`(200 ms)+여유 500 ms 대기 → 10건 한 번에 제출 | 유휴 뒤 첫 배치 크기 ≤ `C_init`(3) (완화되지 않았다면 10 이었을 것); 로그 훅 `"(RELAX)"` 관측; 10건 전부 전달·노드 간 동일 |
| `TestAdaptiveBatchOffIsBaseline` | 같은 입력, `AdaptiveBatch=false` | 모든 배치 크기 ≤ 10, `"Adaptive batch:"` 로그 **미발생** (1 s 부정 확인); 300건 전달·동일. 플래그 OFF 회귀 고정 |
| `TestAdaptiveBatchSurvivesViewChange` | 부하 중 리더 `Disconnect()` → 뷰 체인지 → 새 리더가 나머지 처리 | 전 노드 동일; 새 리더 로그 `"(RESET)"` 뒤 `"Adaptive batch:"` 조정 로그 존재; 뷰 체인지 완료 후 제출한 50건이 10 s 내 전달 (타이머 10 s 설정 덕분에 테스트 ≤ 30 s) |

W=1 회귀: 플래그 OFF 로 `go test ./internal/... ./pkg/...` (≈18 s) 와 `go test ./test -count=1` (≈2분 20초, test-harness.md 측정) 전부 통과가 병합 조건 (branch-policy 4절). 플래그 ON 통합 테스트 4개는 뷰 체인지 타이머를 10 s 로 줄여 각 ≤ 30 s.

---

## 9. 구현 순서 (커밋 단위)

브랜치 `feat/adaptive-batch` 를 `main`(d9d36397) 에서 딴다. 커밋 메시지 형식은 branch-policy 8절 (`Flag: AdaptiveBatch (default false = unchanged)`, `Refs: docs/notes/design-adaptive-batch.md`).

| # | 커밋 | 변경 파일 | 내용 | 플래그 의존 | 크기 추정 |
|---|---|---|---|---|---|
| a | `batcher: put batch parameters behind an atomic pointer and add SetBatchParams` | `internal/bft/batcher.go`, `internal/bft/controller.go` (인터페이스 2줄), `internal/bft/mocks/batcher.go` (재생성), `internal/bft/batcher_test.go`, `pkg/api/metrics.go` (게이지 2개만) | 3절 전체 + 7절의 게이지 2개 (`batcher_max_count`, `batcher_timeout_seconds`, 배처가 기록). 동작 동일성 테스트 8.1 | 무관 (OFF 에서도 동일) | 코드 ~80줄, 테스트 ~150줄, 목 재생성 ~40줄 |
| b | `requestpool: lock-free Size, SizeBytes and OldestAge getters; state: optional WAL append observer` | `internal/bft/requestpool.go`, `internal/bft/state.go`, `internal/bft/controller.go` (S9 카운터), 각 `_test.go` | 4.1절 S1·S2·S3·S7·S8·S9 getter/미러, S6 `OnAppend`. `Size()` 의미 불변 (기존 `TestReqPool*` 통과) | 무관 | 코드 ~70줄, 테스트 ~120줄 |
| c | `batcher: add BatchTuner, a pure control law for batch size and timeout` | `internal/bft/batchtuner.go` (신규), `internal/bft/batchtuner_test.go` (신규), `pkg/api/metrics.go` (카운터 2개) | 5절 전체. 어디서도 생성되지 않음 (dead code 상태로 병합 가능) | 무관 | 코드 ~250줄, 테스트 ~400줄 |
| d | `controller: drive BatchTuner from propose/decide/startView when AdaptiveBatch is on` | `internal/bft/controller.go` (`BatchTuner` 필드, 훅 4곳 각 2~3줄), `pkg/consensus/consensus.go` (`continueCreateComponents` 5줄), `internal/bft/controller_test.go` (OFF 에서 미호출·ON 에서 호출 확인) | 2.3절·4.0절 와이어링. `if c.BatchTuner != nil` 가드 | **여기서만** 플래그를 읽음 | 코드 ~40줄, 테스트 ~80줄 |
| e | `test: adaptive batch integration tests` | `test/adaptive_batch_test.go` (신규) | 8.3절 4개 | ON | ~250줄 |
| f | `docs: PR body` | `SmartBFT_Pipeline/docs/pr/feat-adaptive-batch.md`, `docs/progress.md` | PR 본문 사본 (무엇을/왜/안전성/플래그/테스트) | — | — |

a~c 는 각각 독립적으로 전체 테스트가 통과해야 하며, d 전까지 `AdaptiveBatch` 는 어디서도 읽히지 않는다 (PR #1 상태 유지). 리뷰어는 a 와 d 의 diff 만으로 "OFF = 원본" 을 확인할 수 있다.

---

## 10. 열린 질문 (리뷰어 결정 요청, 권고 포함)

1. **리더 전용 vs 전 노드 생성.** 권고: **전 노드에 객체 생성, 리더 경로에서만 발화** (2.2절). 리더 전용 생성은 `startView` 에서 리더십을 보고 만들고 뷰마다 지우는 코드가 필요해 더 복잡하다. 팔로워의 비용은 수백 바이트 메모리.
2. **S(바이트 상한) 적응 여부.** 권고: **고정** (1.1절 세 이유). `BatchParams.MaxSizeBytes` 필드는 두어 뒤에 열 수 있게 한다. 열 경우 `RequestMaxBytes ≤ S` 를 하한으로 삼아야 한다.
3. **`C_init = ⌈C_cap/4⌉` (설정 상한 아래에서 시작).** 권고: 채택. 대안 `C_init = C_cap` 은 "부하에서 커진다" 를 보일 수 없고 C 는 가드로 줄어들기만 한다. 논문 서술: "설정값은 상한, 제어기는 그 아래에서 탐색". 비교 실험에서 "적응 OFF" 는 상한 고정이므로 공정성 논점은 "적응형이 처음 ~20 결정 동안 작은 배치로 돈다" 는 warm-up 이며, 벤치는 warm-up 구간을 제외하고 측정하면 된다.
4. **기본 파라미터 값** (5.2절 표). 특히 `k=3`, `u/d = 1.25/0.75`, `γ=2.0`, `α=0.3`, `IdleAfter=10·T_0`. 권고: 표의 값으로 시작하고 벤치 (`bench/lib/`) 에서 `k ∈ {2,3,5}`, `γ ∈ {1.5, 2, 3}` 민감도만 스윕. 값은 `TunerConfig` 에 있으므로 테스트·벤치에서 주입 가능하되 **`types.Configuration` 에는 노출하지 않는다** (설정 필드 폭발 방지; 필요해지면 다음 브랜치에서).
5. **배치 종료 사유를 tuner 가 재계산 vs 배처가 `LastBatchReason()` 노출.** 권고: **재계산** (S4). 인터페이스가 하나 덜 늘고 바이트 경계 케이스는 S 고정·균일 요청 가정에서 무시 가능. 벤치에서 `batcher_count_of_batches{reason}` 와 tuner 의 판정 카운트가 어긋나면 그때 getter 로 전환.
6. **WAL append 지연을 제어 입력으로 쓸지.** 권고: 1차에서는 **메트릭·진단만** (S6). L 에 이미 포함되어 있고, 별도 입력으로 쓰려면 "WAL 이 느리면 C 를 어떻게" 라는 새 규칙이 필요한데 fsync 비용은 배치 바이트에 비례하므로 G2 가 같은 현상을 잡는다.
7. **`T_0 > RequestForwardTimeout/4` 인 설정** (예: MaxInterval 1 s, Forward 2 s). `Validate` 가 막지 않는다. 권고: tuner 생성 시 `Warnf` 하고 `T_max = T_0` 으로 올려 (`T_max = max(T_0, min(4T_0, Forward/4))`) 설정값 자체는 항상 허용 — "OFF 와 ON 의 시작 상태가 같다" 를 지키기 위함. 대안은 `Validate` 규칙 추가인데 기존 설정을 깨뜨릴 수 있어 비권고.
8. **제어기 실행 위치 (run 고루틴 이벤트 구동 vs 자체 티커).** 권고: **이벤트 구동** (4.0절 네 가지 이유). 티커는 결정이 없는 구간에 쓸 데 없이 돌고, 결정이 많은 구간에서는 샘플을 건너뛴다. 유휴 완화만 시간 기반이면 충분하다.
9. **뷰 시작 시 `Reset()` vs 상태 유지.** 권고: Reset (6.4절). 같은 노드가 sync 뒤 뷰를 재시작하는 경우 학습이 날아가지만, 뷰 체인지는 드문 이벤트이고 재수렴은 ≤ 21 결정이다.
10. **R2(TIMEOUT-high 에서 T 연장) 를 둘지.** 비포화 steady 부하에서 T 를 늘리면 처리량은 그대로이고(λ 가 결정) 지연만 ≤ 25 % 늘어난다 — 이득은 라운드당 고정비(WAL fsync 2회, 메시지 3N) 의 상각, 즉 CPU·디스크 효율이다. 권고: **둔다**. 이유는 (i) 과제 요구이고, (ii) 4번 브랜치에서 W>1 이면 라운드 수가 처리량의 병목이 되어 상각이 처리량으로 직결되며, (iii) `T_max ≤ Forward/4` 로 비용이 유계이다. 리뷰어가 지연 우선을 원하면 `φ_hi = 1.0` 으로 두어 R2 를 사실상 끌 수 있다 (파라미터만으로 끄기 가능하도록 `φ_hi ≤ 1` 허용).

---

## 부록 A. 인용한 코드 위치 요약 (d9d36397)

| 파일 | 행 | 내용 |
|---|---|---|
| `internal/bft/batcher.go` | 17-26 / 30-44 / 49-75 / 79-86 / 89-115 | 구조체 / 생성자(37-39 복사) / `NextBatch`(50 check, 56 `time.After`, 63 timeout, 68 submitted) / `countBatch` / `Close·Closed·Reset` |
| `internal/bft/requestpool.go` | 57-67 / 71-75 / 145-179 / 189-283 / 286-291 / 296-331 / 373-406 / 456-490 / 493-567 | 필드(65 sizeBytes) / `requestItem`(74) / `ChangeOptions`(149 stopped) / `Submit`(224-228 세마포어, 248-251 Forward 타이머, 259 timestamp, 262-265 push·게이지·카운터, 274-277 submittedChan, 279-280 bytes) / `Size`(287 Lock) / `NextRequests`(300-304 check, 313-316, 323-325 full 판정) / `RemoveRequest·deleteRequest`(385-387, 395-401) / `StopTimers·RestartTimers` / 타임아웃 3단 |
| `internal/bft/controller.go` | 30-35 / 38-47 / 77-132 / 256-279 / 363-384 / 386-414 / 416-434 / 437-461 / 463-475 / 477-514 / 516-569 / 796-828 / 844-875 / 887-920 / 935-941 / 1002-1006 | `Batcher` / `RequestPool` / 구조체 / 타임아웃 핸들러(259, 273, 278) / `startView`(373-381) / `changeView`(411-413 Reset) / `abortView` / `Sync·AbortView·ViewChanged`(439,448,458 Close) / `propose`(467, 468-470, 472-474) / `run`(487, 496-497) / `decide`(518, 523, 533-540, 543-552, 554-557, 566-568) / `Start`(802-803) / `Stop·StopWithPoolPause`(846, 863-864) / `Decide` / `removeDeliveredFromPool` / Deliver 지연 측정 |
| `internal/bft/view.go` | 118-122 / 333-357 / 361-378 / 380-457 / 539-546 / 584-635 / 882-932 / 989-1015 | `prePrepareBegin` / `prepared`(342, 352, 355) / 지연 기록·관측 / `processProposal`(415, 424, 427, 442, 455) / commit 저장·로그 / `verifyProposal`(586) / `decide·startNextSeq`(904, 909) / `Propose` |
| `internal/bft/state.go` | 40-56 | `Save`(55 `WAL.Append`) |
| `internal/bft/util.go` | 263, 286 | `ProposalMaker`, `NewProposer` |
| `pkg/consensus/consensus.go` | 108-165 / 186-261 / 393-456 / 458-469 | `Start`(140-151) / `reconfig`(192-194, 207, 221-230, 254-256) / `createComponents`(425-448) / `continueCreateComponents`(459-463) |
| `pkg/types/config.go` | 18-28 / 32-44 / 87-95 / 102-125 / 129-134 / 136-221 | 배치 필드 / 풀·타임아웃 / 새 플래그 / `DefaultConfig` / `EffectivePipelineDepth` / `Validate`(182-187, 188-193, 209-218) |
| `pkg/api/metrics.go` | 16-19 / 266-314 / 507-539 | reason 상수 / `MetricsBatcher` / 뷰 히스토그램·in-flight |
| `pkg/api/dependencies.go` | 33-37 / 40-44 / 55-68 | `Assembler` / `WriteAheadLog` / `Verifier`(57) |
| `pkg/wal/writeaheadlog.go`, `pkg/wal/metrics.go` | 104-107, 410-429 / 20-28 | `Options`, `Append`(424-426) / `wal_latency_append` |
| `test/test_app.go` | 28-46 / 213-222 / 448-454 / 460-531 | `fastConfig` / `VerifyProposal` / `newNode` / `newNodeWithConfig`(470 Delivered 버퍼, 475 metricsProvider, 481-527 Setup) |
| `test/basic_test.go` | 32-59 / 3455 / 3466 / 3483 / 3524 | `TestBasic` / `doInBackground` / `countCommittedBatches` / `waitForCatchup` / `startNodes` |
| `test/pipeline_config_test.go` | 14-75 | `newNodeWithConfig` 사용 패턴 |
| `fabric/orderer/consensus/smartbft/verifier.go` | 98-121, 263-329 | `VerifyProposal`, `verifyBlockDataAndMetadata` (개수 검사 없음) |
| `fabric/orderer/consensus/smartbft/util.go` | 450-458 | `createSmartBftConfig` (455-456 BatchSize → 라이브러리 설정) |
| `fabric/orderer/common/msgprocessor/sizefilter.go` | 39 | `AbsoluteMaxBytes` 는 envelope 단위 필터 |

## 11. 리뷰 결정 반영 (2026-11-02)

결정 전문은 [decisions-2026-11-02.md](decisions-2026-11-02.md). 이 문서에 미치는 변경:

- **§5 지연 가드 기준선 수정 (필수)**: `L_ref` 를 "전 구간 최소" 에서 **최근 `RefWindow`(기본 100) 결정의 슬라이딩 윈도우 최소** 로 바꾼다. 근거: RTT 가 영구 증가(netem 50→100 ms)하면 전 구간 최소 기준의 가드가 계속 발동해 C 가 천장에 갇히고, 30 결정 천장 기억이 풀려도 재발동한다. 구현: 고정 크기 링 버퍼 + 단조 큐(O(1) amortised) 또는 단순 O(RefWindow) 스캔(RefWindow=100 이면 무시 가능). `RefWindow` 는 `TunerConfig` 필드. 윈도우가 차기 전(결정 < RefWindow)에는 가드를 발동하지 않는다(warm-up 과 일치). 대안으로 EWMA 쪽으로 감쇠하는 최소(`L_ref ← L_ref + β(L_fast − L_ref)`, β≪α)도 가능하나 파라미터가 하나 더 늘어 비권고.
- **§5 γ 는 실험 변수** (기본 2.0). LAN 에서는 배치 크기가 라운드 지연에 거의 비례해 가드가 정상 배치 성장을 혼잡으로 오인할 수 있다. 설계 의도("지연 제약 내 TPS 최대화")와 일치하지만, LAN 결과가 baseline 보다 TPS 가 낮으면 이 가드 때문이다. `batcher_count_of_guard_trips{reason}` 를 결과 그래프에 함께 표시해 설명한다.
- **§2 팔로워 측 상한 없음 → 논문 필수 기재**: 팔로워(`view.go verifyProposal`)도 하네스도 Fabric `verifier.go` 도 배치의 요청 개수를 상한과 비교하지 않는다. 즉 비잔틴 리더는 baseline 에서도 임의 크기 배치를 보낼 수 있으므로 **적응형 배치 제어는 공격 표면을 추가하지 않는다**. 평가 설계: "상한 초과 감지 → 뷰 체인지" 류 실험은 배치 크기가 아니라 **파이프라인 깊이(수용 구간 밖 seq 폐기, design-pipeline-window §3)** 에만 적용한다.
- **§10 ③**: `C_init = ⌈C_cap/4⌉` 채택. 램프 1.25^6 ≈ 3.8배, k=3 이면 약 18 결정에 C_cap 도달 → **벤치 warm-up 최소 30 결정**.
- **§10 ④**: 튠 파라미터는 `TunerConfig` 에만 두되 **벤치·하네스에서 주입 가능**해야 한다(k, γ, 배율, RefWindow 스윕). 구현: `Consensus` 에 `BatchTunerConfig *TunerConfig`(nil = 기본값) 필드 또는 `SetTunerConfig` — `types.Configuration` 비노출과 양립.
- §10 ⑩ 및 나머지: 권고대로.
