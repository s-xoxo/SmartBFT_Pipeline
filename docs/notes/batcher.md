# F. 배치 (`internal/bft/batcher.go`, `requestpool.go`)
- 날짜: 2026-11-02
- 대상 커밋: SmartBFT 32a628d6 (baseline)
- probe 브랜치: 없음 — 읽기만

(참고: 로컬 `main` HEAD 는 ea1d499d 이지만 32a628d6 위에 docs 커밋 하나만 올라가 있고 `git diff --stat 32a628d6 HEAD -- internal/bft pkg smartbftprotos` 가 비어 있어 아래 라인 번호는 baseline 과 동일함.)

## 체크리스트

### [x] `NewBatchBuilder()` 25행: `maxMsgCount`, `maxSizeBytes`, `batchTimeout` 이 어디서 읽히는지 (`consensus.go` 에서 config → builder 한 번)
- 생성 지점은 단 한 곳: `pkg/consensus/consensus.go:459` (`continueCreateComponents()`)
  ```go
  batchBuilder := algorithm.NewBatchBuilder(c.Pool, c.submittedChan, c.Config.RequestBatchMaxCount, c.Config.RequestBatchMaxBytes, c.Config.RequestBatchMaxInterval)
  ```
  `continueCreateComponents()` 는 `Start()` (`consensus.go:151`) 와 `reconfig()` (`consensus.go:230`) 에서 호출된다. 즉 **런타임에 배치 파라미터가 바뀌는 유일한 경로는 reconfig (모든 컴포넌트 stop → 새 BatchBuilder 객체 생성)** 이다.
- `internal/bft/batcher.go:29-31` 에서 값은 평범한 구조체 필드로 복사된다 (`maxMsgCount: int(maxMsgCount)`, `maxSizeBytes`, `batchTimeout`). setter 없음, 락 없음. `NextBatch()` 는 이 필드를 `batcher.go:41,46,53,57` 에서 락 없이 읽는다 → 다른 goroutine 이 쓰면 data race. `SetBatchParams` 는 atomic 또는 mutex 가 필요.
- 설정 검증 (`pkg/types/config.go`): `RequestBatchMaxCount > 0` (119), `RequestBatchMaxBytes > 0` (122), `RequestBatchMaxInterval > 0` (125), 그리고 교차 제약 `RequestBatchMaxCount > RequestBatchMaxBytes → error` (161), `RequestMaxBytes > RequestBatchMaxBytes → error` (164). 기본값 `config.go:93-95`: Count 100, Bytes 10 MiB, Interval 50 ms. 적응형 제어기가 값을 바꿀 때도 이 제약을 지켜야 한다 (단, Validate 는 Start/reconfig 때만 호출되므로 런타임에는 제어기가 스스로 clamp 해야 함).

### [x] `NextBatch()` 40행: 블로킹 구조, 타임아웃 시작 시점(첫 요청 도착 vs 호출 시점)
- `batcher.go:40-63` 전체:
  ```go
  currBatch, full := b.pool.NextRequests(b.maxMsgCount, b.maxSizeBytes, true)
  if full { return currBatch }
  timeout := time.After(b.batchTimeout)           // 46행
  for {
      select {
      case <-b.closeChan:   return nil
      case <-timeout:       currBatch, _ = b.pool.NextRequests(b.maxMsgCount, b.maxSizeBytes, false); return currBatch
      case <-b.submittedChan: currBatch, full = b.pool.NextRequests(..., true); if full { return currBatch }
      }
  }
  ```
- **타임아웃은 `NextBatch()` 호출 시점에 시작된다** (46행), 첫 요청 도착 시점이 아니다. `config.go:24-27` 의 주석("from the time the batch was first created (i.e. the time the first request was added to it)")은 코드와 다르다. 풀이 비어 있는 상태에서 호출되면 50 ms 후 빈 배치를 반환하고, `controller.go:467-471` 의 `propose()` 가 `len(nextBatch)==0` 이면 `acquireLeaderToken()` 으로 다시 호출한다 → 유휴 시 `batchTimeout` 주기로 폴링하는 구조. 요청이 t 에 도착하면 실제 제안은 (마지막 폴링 시작 + 50 ms) 이지, (t + 50 ms) 가 아니다.
- `check=true` 의 의미 (`requestpool.go:299-303`): `len(existMap) < maxCount && sizeBytes < maxSizeBytes` 이면 **배치를 만들지 않고 `nil,false`** 를 반환한다. 즉 가득 차기 전까지는 요청을 꺼내 보지도 않는다. 타임아웃 뒤 `check=false` 로 호출해야 부분 배치가 나온다.
- 깨우기 채널: `submittedChan` 은 용량 1 (`consensus.go:149 make(chan struct{}, 1)`), `Submit()` 에서 non-blocking 송신 (`requestpool.go:274-277`).
- **블로킹의 범위**: `NextBatch()` 는 컨트롤러 run 루프 goroutine 안에서 실행된다 (`controller.go:477-498` 의 `case <-c.leaderToken: c.propose()`). 따라서 블로킹 중에는 `decisionChan`/`viewChange`/`abortViewChan`/`syncChan` 처리가 멈춘다. 이를 풀어 주는 수단이 `Close()` 다: `Sync()` (`controller.go:439`), `AbortView()` (448), `ViewChanged()` (458), `Stop()` (846), `StopWithPoolPause()` (863) 가 채널 송신 전에 `c.Batcher.Close()` 를 먼저 호출한다. 파이프라이닝에서 W 개 제안을 한 루프에서 만들 때 이 블로킹 구조를 그대로 두면 안 된다.
- **파이프라이닝에 결정적인 발견**: `NextRequests()` 는 요청을 풀에서 제거하지 않는다. `requestpool.go:305-320` 은 항상 `rp.fifo.Front()` 부터 순회만 하고, 제거는 decide 뒤 `controller.go:935-940 removeDeliveredFromPool()` → `RemoveRequest()` 에서만 일어난다. 따라서 seq n 이 결정되기 전에 `NextBatch()` 를 다시 부르면 **같은 요청들이 다시 반환된다**. W>1 에서는 in-flight 요청을 건너뛰는 커서/예약 집합이 풀 또는 배처에 필요하다.

### [x] `Reset()` 88행: 뷰 체인지 시 배치가 버려지는지, 요청이 풀로 돌아가는지
- `batcher.go:88-92`: `closeChan` 을 새로 만들기만 한다. `BatchBuilder` 는 배치 상태를 보유하지 않는다 (`currBatch` 는 `NextBatch()` 의 지역 변수). 위에서 본 대로 배처는 요청을 풀에서 빼내지 않으므로 "풀로 돌아간다"는 개념 자체가 없다 — 요청은 처음부터 풀에 그대로 있다.
- 호출 지점은 단 한 곳 `controller.go:411-413`:
  ```go
  if iAm, _ := c.iAmTheLeader(); iAm {
      c.Batcher.Reset()
  }
  ```
  `changeView()` 가 `startView()` 뒤에 새 리더만 Reset 한다.
- in-flight 제안의 요청 (`view.inFlightRequests`, `view.go:100`) 도 decide 전까지 풀에 남아 있으므로, 뷰 체인지 뒤 새 리더가 그대로 재제안하거나, 뷰 체인지 프로토콜의 in-flight 복구로 결정된다. 중복 제출은 `delMap` 으로 걸러진다 (`requestpool.go:242-246 ErrReqAlreadyProcessed`).
- 뷰 체인지 중 타이머는 `StopTimers()`/`RestartTimers()` 로 멈추고 다시 시작한다 (`viewChanger.RequestsTimer = c.Pool`, `consensus.go:467`; 리더 로테이션 시 `controller.go:559-563`; reconfig 시 `consensus.go:256`). `RestartTimers()` 는 모든 요청을 `ForwardTimeout` 부터 다시 시작한다 (`requestpool.go:470-488`).

### [x] `requestpool.go`: 풀 길이를 O(1)로 읽을 수 있는 API 가 있는지 (`Size()`?) — 적응형 제어 신호용
- 있음. `requestpool.go:284-290`:
  ```go
  func (rp *Pool) Size() int {
      rp.lock.Lock()
      defer rp.lock.Unlock()
      return len(rp.existMap)
  }
  ```
  O(1) 이지만 **배타 락** (`Lock`, `RLock` 아님) 을 잡는다. 제어기가 고빈도로 읽으면 Submit/NextRequests 와 경합한다 → `RLock` 으로 바꾸거나 `atomic.Int64` 미러를 두는 것이 좋다. `RequestPool` 인터페이스 (`controller.go:38-47`) 에도 `Size() int` 가 포함되어 있다.
- 바이트 합계 `rp.sizeBytes` (`requestpool.go:65`) 는 Submit(279)/RemoveRequest(385) 에서 유지되지만 **getter 가 없다** → `SizeBytes()` 신설 필요.
- 요청별 `additionTimestamp` (`requestpool.go:74`) 가 있어 `fifo.Front()` 의 체류 시간(= 가장 오래된 요청 나이) 을 O(1) 로 뽑을 수 있으나 API 없음 → `OldestAge()` 신설 필요.
- 풀 용량 `QueueSize`(=`RequestPoolSize`, 기본 400) 는 semaphore (`requestpool.go:118,224`); 가득 차면 `Submit()` 이 `SubmitTimeout`(기본 5 s) 까지 블록하고 실패 시 `CountOfFailAddRequestToPool{reason=semaphore}` 증가 (`requestpool.go:225-228`). 이 카운터는 백프레셔 신호로 쓸 수 있다.

### [x] 요청 타임아웃(`RequestForwardTimeout`, `RequestComplainTimeout`, `RequestAutoRemoveTimeout`): 배치를 키우거나 W 를 늘려 지연이 늘면 complain 이 오발되는 임계값
- 3단 체인, 모두 **Submit 시각 기준** 으로 순차 시작:
  1. `ForwardTimeout` 타이머는 `Submit()` 에서 시작 (`requestpool.go:248-251`). 만료 → `onRequestTO` (491-521): `ComplainTimeout` 타이머 시작 후 `OnRequestTimeout` → 팔로워는 리더로 전달 (`controller.go:263-264 SendTransaction`), 리더는 무시 (259).
  2. 만료 → `onLeaderFwdRequestTO` (524-554): `AutoRemoveTimeout` 시작 후 `OnLeaderFwdRequestTimeout` → **팔로워: `Complain(view, true)`** (`controller.go:278`); **리더: `LeaderMonitor.StopLeaderSendMsg()`** (273) — 리더가 하트비트를 끊어 팔로워들의 heartbeat timeout 을 유도한다.
  3. 만료 → `onAutoRemoveTO` (557-565): `RemoveRequest`, `CountOfDeleteRequestPool++`.
- 기본값 (`config.go:98-100`): Forward 2 s, Complain 20 s, AutoRemove 3 min. 제약 Forward ≤ Complain ≤ AutoRemove (`config.go:167-172`).
- **임계값**: 요청은 decide+Deliver 뒤 `removeDeliveredFromPool` 까지 풀에 남는다. 따라서 요청의 풀 체류 시간 (큐 대기 + 배치 대기 + 라운드 지연 + Deliver) 이
  - `ForwardTimeout`(2 s) 을 넘으면 모든 팔로워가 리더에게 요청을 재전송 (대역폭 낭비, 리더 풀에 중복 Submit 시도);
  - `ForwardTimeout + ComplainTimeout`(22 s) 을 넘으면 팔로워 complain → 뷰 체인지 오발, 리더는 하트비트 중단.
  파이프라이닝으로 W 가 커져 리더 풀의 백로그가 W×MaxCount 로 커지면 꼬리 요청의 체류 시간이 그만큼 늘어난다. 또 **`W × RequestBatchMaxCount ≤ RequestPoolSize` 가 아니면 in-flight 요청만으로 semaphore 가 가득 차 `Submit()` 이 블록된다** (기본 100×4 = 400 = 정확히 꽉 참). 제어기의 하드 제약으로 넣어야 한다.
- 타임아웃 값은 런타임에 `ChangeOptions()` 로 바꿀 수 있으나 **`rp.stopped` 상태에서만** 허용된다 (`requestpool.go:149-152` `if !rp.stopped { Errorf(...); return }`), 즉 reconfig 경로 전용. 제어기가 타임아웃을 조정하려면 이 제약을 풀거나(이미 시작된 타이머는 그대로 두고 옵션만 교체) 조정 대상에서 제외해야 한다.

### [x] `pkg/metrics`: 이미 있는 메트릭 목록 — 라운드 지연·배치 크기·풀 길이 중 없는 것
- `pkg/metrics/provider.go` 는 Counter/Gauge/Histogram **쓰기 전용 인터페이스** 다 (`Add/Set/Observe` 만 있고 Get 없음, 11-18, 21-28, 69-79, 121-127). **제어기는 메트릭을 읽을 수 없다** → in-process 원시 신호를 따로 노출해야 한다.
- 정의된 메트릭 전수 (`pkg/api/metrics.go`, 네임스페이스 `consensus_smartbft_`):
  | 이름 | 타입 | 기록 지점 | 의미 |
  |---|---|---|---|
  | `pool_count_of_elements` | Gauge | `requestpool.go:263,394` | 풀 길이 (요청 수) |
  | `pool_count_of_fail_add_request{reason}` | Counter | `requestpool.go:196,225` | 크기 초과 / semaphore 실패 |
  | `pool_count_leader_forward_request` | Counter | `requestpool.go:519` | ForwardTimeout 발화 수 |
  | `pool_count_timeout_two_step` | Counter | `requestpool.go:552` | ComplainTimeout 발화 수 |
  | `pool_count_of_delete_request` | Counter | `requestpool.go:563` | AutoRemove 수 |
  | `pool_count_of_elements_all` | Counter | `requestpool.go:264` | 누적 Submit 수 (도착률 미분 가능) |
  | `pool_latency_of_elements` | Histogram | `requestpool.go:395` | 요청의 풀 체류 시간 (Submit→제거) |
  | `view_number`, `view_leader_id`, `view_proposal_sequence`, `view_decisions`, `view_phase` | Gauge | `view.go:301,880-881` 등 | 뷰 상태 |
  | `view_count_txs_in_batch` | Gauge | `view.go:398` | 마지막 배치의 요청 수 (모든 노드가 processProposal 에서 기록) |
  | `view_count_batch_all`, `view_count_txs_all` | Counter | `view.go:340-341` | 누적 배치/요청 수 |
  | `view_size_batch` | **Counter** | `view.go:343-347` | 누적 바이트 (payload+header+metadata+서명) — 이름은 "average" 지만 누적 |
  | `view_latency_batch_processing` | Histogram | `view.go:348` | `beginPrePrepare`→commit 정족수 (= 라운드 지연, pre-prepare 기준) |
  | `view_latency_batch_save` | Histogram | `controller.go:1002-1004` | **`Application.Deliver` 소요 시간** (이름과 달리 WAL 저장 아님) |
  | `consensus_reconfig`, `consensus_latency_sync` | Counter/Hist | `consensus.go:258` 등 | reconfig / sync |
  | `blacklist_count`, `node_id_in_blacklist`, `viewchange_*` | Gauge | — | 블랙리스트/뷰 체인지 |
  | `wal_count_of_files` | Gauge | `pkg/wal/writeaheadlog.go:697,745` | WAL 파일 수 |
- 히스토그램 버킷은 `{0.005, 0.01, 0.015, 0.05, 0.1, 1, 10}` (`metrics.go:437,446`) — 제어기 해상도로는 거칠다.
- **없는 것**: (1) 풀 바이트 합계, (2) 가장 오래된 요청 나이, (3) 배치가 "꽉 차서" 나왔는지 "타임아웃으로" 나왔는지 (fill ratio), (4) `NextBatch()` 대기 시간, (5) WAL `Append` 지연 (현재 WAL 메트릭은 파일 수만), (6) 리더 관점 제안 종단 지연 (`Propose`→decide), (7) in-flight 제안 수 (W 점유율), (8) 뷰 goroutine 의 incMsgs 큐 길이. 라운드 지연·배치 크기·풀 길이 자체는 존재하지만 Prometheus 전용이라 제어기가 읽을 수 없다.

## 결론 / 설계 결정

### `SetBatchParams()` 시그니처 (제안)
```go
// internal/bft/batcher.go
type BatchParams struct {
    MaxMsgCount  int
    MaxSizeBytes uint64
    Timeout      time.Duration
}

// SetBatchParams 는 다음 NextBatch() 호출(및 진행 중인 select 의 다음 반복)부터 적용된다.
// 진행 중인 time.After 타이머는 재무장하지 않는다 (최대 한 배치 지연 뒤 반영).
func (b *BatchBuilder) SetBatchParams(p BatchParams)
func (b *BatchBuilder) BatchParams() BatchParams
```
- 구현: `params atomic.Pointer[BatchParams]` 로 교체하고 `NextBatch()` 의 41/53/57행이 매 반복마다 `p := b.params.Load()` 를 읽도록 한다. `batchTimeout` 은 46행에서 한 번 캡처되므로 즉시 반영이 필요하면 `paramsChanged chan struct{}` 를 select 에 추가해 타이머를 재무장한다.
- `Batcher` 인터페이스 (`controller.go:30-35`) 에 `SetBatchParams(BatchParams)` 추가 → `internal/bft/mocks` 재생성 필요.
- `AdaptiveBatch=false` 기본값에서는 `NewBatchBuilder` 가 넣은 초기값이 바뀌지 않으므로 원본과 동일 동작.
- 제어기 측 clamp 규칙 (Validate 는 런타임에 안 돌므로 제어기가 직접): `1 ≤ MaxMsgCount ≤ MaxSizeBytes`, `RequestMaxBytes ≤ MaxSizeBytes`, `Timeout > 0`, `W × MaxMsgCount ≤ RequestPoolSize`, 예상 체류 시간 `< RequestForwardTimeout`.

### 제어기가 읽을 신호와 측정 지점
| 신호 | 측정 지점 | 현재 API | 필요 작업 |
|---|---|---|---|
| 풀 길이 | `Pool.Size()` `requestpool.go:285` | 있음 (배타 락) | `RLock` 또는 atomic 미러 |
| 풀 바이트 | `rp.sizeBytes` `requestpool.go:65` | 없음 | `Pool.SizeBytes()` 신설 |
| 가장 오래된 요청 나이 | `fifo.Front().additionTimestamp` `requestpool.go:74` | 없음 | `Pool.OldestAge()` 신설 |
| 도착률 | `Submit()` 성공 지점 `requestpool.go:264` | Counter(쓰기 전용) | atomic 카운터 + 제어기 주기 미분 |
| 백프레셔 | `semaphore.Acquire` 실패 `requestpool.go:225-228` | Counter(쓰기 전용) | atomic 카운터 |
| 배치 결과 (full/timeout, 개수, 바이트) | `NextBatch()` 반환 지점 `batcher.go:42,53,58` | 없음 | `BatchBuilder.LastBatchStats()` 또는 콜백 |
| 라운드 지연 (PP→commit 정족수) | `view.go:348 time.Since(v.beginPrePrepare)` | Histogram(쓰기 전용) | View 에 `OnRoundDone(seq, dur)` 훅 → 컨트롤러 |
| Deliver 지연 | `controller.go:1002-1004` | Histogram(쓰기 전용) | 같은 훅 |
| WAL Append 지연 | `state.go:56 ps.WAL.Append` 전후 | 없음 | `PersistedState.Save` 에서 측정 |
| 타임아웃 발화 (forward/complain) | `requestpool.go:519,552`, 리더는 `controller.go:269-274` | Counter(쓰기 전용) | atomic 카운터; complain 발화는 즉시 W/배치 축소 트리거 |
| in-flight 수 (W 점유) | 파이프라인 신규 코드 | 없음 | 파이프라인 윈도우에서 노출 |

- 측정은 모두 리더 노드 in-process 로 하고, 팔로워는 제어하지 않는다 (배치 파라미터는 리더만 사용; `view_count_txs_in_batch` 처럼 팔로워가 기록하는 값은 참고용).

## 열린 문제
1. **in-flight 요청 중복 반환**: `NextRequests()` 가 항상 `fifo.Front()` 부터 읽으므로 (`requestpool.go:308`) W>1 에서는 "제안됐지만 미결정" 요청을 건너뛸 메커니즘(풀의 예약 플래그 vs 배처의 커서 vs 별도 in-flight 집합) 을 정해야 한다. 뷰 체인지/abort 시 예약 해제 경로도 필요.
2. **run 루프 블로킹**: `NextBatch()` 가 컨트롤러 run goroutine 을 점유한다 (`controller.go:496`). W 개를 연속 제안하려면 제안 goroutine 분리 또는 non-blocking `TryNextBatch()` 가 필요하고, 그러면 `Close()` 로 깨우는 현재 abort 프로토콜을 재검토해야 한다.
3. **타임아웃 의미론 불일치**: 주석(첫 요청 도착 기준) vs 코드(호출 시점 기준). 적응형 배처가 "첫 요청 도착 기준" 으로 바꿀지, 원본 동작을 그대로 둘지 결정 필요 — `PipelineDepthMax=1, AdaptiveBatch=false` 에서는 원본 유지가 조건. 리뷰 반영: proposer 고루틴을 분리하면 타임아웃은 "토큰을 받은 시점부터 T" 가 되므로 적응형 제어의 T 의미는 **"제안 가능해진 뒤 최대 대기"** 로 정리된다. `config.go:24-27` 주석과 코드의 불일치는 upstream 이슈 후보로 기록.
4. **`ChangeOptions` 의 stopped 제약**: 요청 타임아웃을 제어기가 런타임에 조정하려면 `requestpool.go:149` 조건을 완화해야 하는데, 이미 무장된 타이머와의 상호작용(RestartTimers 가 모두 ForwardTimeout 으로 재시작) 을 검토하지 않았다. 1차 범위에서는 타임아웃을 고정하고 W/배치만 조정하는 쪽이 안전.
5. **`Size()` 의 배타 락**: 제어기 샘플링 주기와 경합 비용을 측정하지 않았다 ([?]).
6. 히스토그램 메트릭 이름 오류 (`view_latency_batch_save` = Deliver 시간, `view_size_batch` = 누적 Counter) 는 원본 그대로 두고 새 메트릭을 추가하는 것으로 한다 (논문 비교 시 혼동 방지를 위해 노트에 기록).
