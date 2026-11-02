# PR: `feat/metrics-round-latency` → `main`

- 브랜치: `feat/metrics-round-latency` (base `main` = ea1d499d; `internal/bft` 는 baseline 32a628d6 과 동일)
- 커밋: f1f819c1 `metrics: per-sequence round latency, WAL append, in-flight, pool bytes, batch reason`
- 작성일: 2026-11-02

## 무엇을

계측만 추가한다. 프로토콜·메시지 흐름·WAL 내용·기존 로그 문자열은 어떤 설정에서도 `main` 과 동일하다.

1. **시퀀스별 라운드 지연** — `View.beginPrePrepare time.Time` (단일 필드) 를 `prePrepareBegin map[uint64]time.Time` (제안 seq 키) 로 바꿨다. pre-prepare 수락 시 `recordPrePrepareBegin(seq)` 가 기록하고, commit 정족수 시 `observeBatchProcessingLatency(seq)` 가 **해당 seq 의** 시각으로 `view_latency_batch_processing` 을 관측한 뒤 항목을 삭제한다. 기록 지점 3곳 모두 교체: `view.go processProposal`, `state.go restoreInFlightRequests` (WAL 복구; seq 를 인자로 받도록 변경 — `recoverProposed` 는 `v.ProposalSequence` 가 아직 갱신되기 전에 호출되므로 WAL 의 pre-prepare seq 를 명시적으로 넘김), `viewchanger.go commitInFlightProposal` (in-flight 뷰). in-flight 가 1개인 현재 동작에서 관측값은 이전과 동일하다.
2. **신규 메트릭** (`pkg/api/metrics.go`, `pkg/wal/metrics.go`; 생성자·`With`·`Initialize`·disabled provider 모두 반영):
   | 이름 | 타입 | 기록 지점 | 의미 |
   |---|---|---|---|
   | `consensus_smartbft_view_in_flight_proposals` | Gauge | `view.go recordPrePrepareBegin` / `prepared()` decide 직후 | pre-prepare(prepare 송신) 했지만 아직 deliver 되지 않은 제안 수 (= `len(prePrepareBegin)`; 현재 0 또는 1) |
   | `consensus_smartbft_pool_size_bytes_of_elements` | Gauge | `requestpool.go Submit`/`RemoveRequest` (`rp.sizeBytes` 갱신 직후) | 풀의 요청 바이트 합계 |
   | `consensus_smartbft_batcher_count_of_batches{reason}` | Counter | `batcher.go NextBatch` | 반환된 **비어 있지 않은** 배치 수. `reason=FULL` (count/bytes 임계 도달) / `TIMEOUT` (타임아웃 만료). 유휴 풀의 빈 타임아웃 배치는 세지 않음 |
   | `consensus_smartbft_wal_latency_append` | Histogram | `pkg/wal/writeaheadlog.go Append` (mutex 획득 후 `append()` 전후) | marshal + write + fsync(+파일 전환) 시간. mutex 대기 시간은 제외 |
   - 새 메트릭 구조체 `api.MetricsBatcher` 를 `api.Metrics` 에 추가. `NewBatchBuilder` 가 마지막 인자로 `*api.MetricsBatcher` 를 받으며 `nil` 이면 disabled provider 로 대체 (호출자: `consensus.go` 1곳, 테스트).
   - `wal.Metrics` 에 `LatencyAppend` 추가. `wal.Options.Metrics` 가 nil 이면 기존처럼 disabled 로 채워지므로 호출자 변경 없음.
   - 라벨 상수 `api.ReasonBatchFull = "FULL"`, `api.ReasonBatchTimeout = "TIMEOUT"`.
3. **문서 수정(주석만, 이름·Help 문자열 변경 없음)** — `view_latency_batch_save` 가 실제로는 `Application.Deliver` 시간임을 `pkg/api/metrics.go` 와 `controller.go` 의 관측 지점에 주석으로 명시; `view_size_batch` 가 평균이 아닌 누적 Counter 임을 주석으로 명시.

## 왜

- 파이프라이닝(W>1) 에서는 seq n 의 pre-prepare 와 seq n+1 의 pre-prepare 가 겹치므로 단일 `beginPrePrepare` 는 덮어써진다 (`docs/notes/view-slots.md`). 뷰 구조를 바꾸는 `feat/pipeline-window` 보다 먼저 계측을 seq 기준으로 바꿔 두면, 이후 브랜치에서 W=1 과 W>1 의 라운드 지연을 같은 메트릭으로 비교할 수 있다.
- `docs/notes/batcher.md` 의 "없는 것" 목록 중 (1) 풀 바이트, (3) 배치 충만 사유, (5) WAL Append 지연, (7) in-flight 수를 채운다. 적응형 배처·깊이 제어기 설계와 논문 측정에 필요한 신호다.
- `view_latency_batch_save`/`view_size_batch` 의 이름 오류는 upstream 호환을 위해 이름을 유지하고 주석으로만 기록한다 (논문 비교 시 혼동 방지).

## 안전성에 미치는 영향

없음. 변경은 (a) 메트릭 `Set/Add/Observe` 호출 추가, (b) 뷰 내부의 시각 저장 자료구조 교체, (c) 생성자 인자 추가로 한정된다.

- 메시지 송수신·검증·WAL 레코드·`State.Save` 호출·결정 순서는 건드리지 않았다. `smartbftprotos`, `pkg/api` 인터페이스, `Batcher`/`RequestPool` 인터페이스(mocks) 변경 없음.
- goroutine 소유권: `prePrepareBegin` 은 `Start()` 이후 뷰 goroutine (`processProposal`/`prepared`) 만 접근한다. `Start()` 이전의 쓰기 2곳 (`PersistedState.Restore`, `ViewChanger.commitInFlightProposal`) 은 `go v.run()` 의 happens-before 로 보호되므로 락 없이 안전하다 (`go test -race ./internal/bft/ ./pkg/wal/` 통과). `Start()` 는 맵을 초기화하지 않으므로 사전 시딩이 보존된다.
- `observeBatchProcessingLatency` 는 기록이 없는 seq 에 대해 관측하지 않는다 (기존 코드는 zero `time.Time` 기준으로 수십 년 값을 관측했을 경로지만, 실제 세 경로 모두 항상 기록되므로 관측 횟수는 동일 — `view_test.go` 의 WAL 복구 테스트가 `view_latency_batch_processing` 관측 1회를 그대로 검증).
- 기존 로그 문자열 변경 없음 (`test/` 의 로그 훅 테스트 영향 없음).
- 알려진 한계(원본 동작, 수정하지 않음): `Pool.Close()` 는 `deleteRequest` 를 직접 호출해 `rp.sizeBytes` 를 줄이지 않으므로, 닫힌 풀의 `pool_size_bytes_of_elements` 는 마지막 값으로 남는다. `sizeBytes` 를 고치면 `NextRequests(check=true)` 의 판단이 달라질 수 있어 이 PR 범위 밖으로 둔다.

## 플래그

- 플래그: 없음 (계측만; `docs/branch-policy.md` 1-1 "계측·메트릭 추가" 해당)
- 기본값(OFF)에서 원본 동작과 동일함: **예** — 플래그가 없으며, 변경된 코드 경로는 메트릭 호출과 뷰 내부 시각 자료구조만이다. 메시지 흐름은 `git diff ea1d499d -- internal/bft pkg` 를 읽어 송신/저장 지점에 변경이 없음을 확인했고, 통합 테스트(`TestBasic`, `TestRestartKeepsViewAndInFlightProposal`) 와 `internal/bft` 전체 단위 테스트(-race 포함) 가 수정 없이 통과한다.

## 테스트

실행 환경: macOS (darwin/arm64), go1.27.3, worktree `/Users/liam/Desktop/smartbft/wt-feat-metrics`, 커밋 f1f819c1.

- [x] `go build ./... && go vet ./...` — 통과, 0.47 s (캐시 상태)
- [x] `go test ./internal/... ./pkg/...` — 통과, 8.6 s wall
  - `internal/bft` ok 5.1 s · `pkg/api` ok 0.19 s · `pkg/types` ok 0.35 s · `pkg/wal` ok 7.96 s
- [x] `go test ./internal/bft/ ./pkg/wal/ -race -count=1` — 통과, 9.0 s wall (data race 없음)
- [x] `go test ./test -run 'TestBasic$|TestRestartKeepsViewAndInFlightProposal$' -count=1` — 통과, 1.8 s wall (`TestBasic` 0.94 s, `TestRestartKeepsViewAndInFlightProposal` 1.17 s)
- [x] `golangci-lint run ./...` (tools/go.mod 고정 버전 v2.15.0, `.golangci.yml`: gci·gofumpt·gofmt·goimports 포함) — `0 issues`, 4.7 s. `gofmt -l internal pkg` 출력 없음.
- [ ] 플래그 ON 테스트 추가: 해당 없음 (플래그 없는 계측 브랜치). 대신 추가한 단위 테스트:
  - `internal/bft/view_metrics_internal_test.go` `TestViewPrePrepareLatencyPerSequence` (package `bft`): seq 7·8 을 기록 → in-flight 게이지 1→2; seq 8 관측 시 seq 8 의 시각으로 측정(< 1분)되고 seq 7 은 남음; seq 7 관측은 1시간 전 시각으로 측정(≥ 3600 s) 후 맵 비움; 기록 없는 seq 관측은 무시; 재기록 시 새 측정 시작.
  - `internal/bft/batcher_test.go` `TestBatcherCountsBatchesByReason`: 빈 타임아웃 미집계, count 임계 FULL, bytes 임계 FULL, 부분 배치 TIMEOUT, 대기 중 제출로 가득 찬 FULL, `nil` 메트릭 허용.
  - `internal/bft/requestpool_test.go` `TestReqPoolSizeBytesMetric`: Submit/중복 Submit/RemoveRequest 에 따른 바이트 게이지 값.
  - `pkg/wal/metrics_test.go` `TestWriteAheadLogFile_AppendObservesLatency`: `Initialize` 의 0 관측 + Append 2회 = 관측 2회 추가, 빈 데이터 거부는 미관측, 메트릭 없는 WAL 도 Append 정상.
  - 테스트용 provider: `internal/bft/metrics_recorder_test.go` (`metricsRecorder`, gauge/counter 기록).
  - 기존 회귀: `view_test.go` (WAL 복구 후 `view_latency_batch_processing` 1회), `viewchanger_test.go` (in-flight 뷰 지연 관측) 수정 없이 통과.

## 셀프 리뷰 체크리스트

### A. 공통 (모든 feat/ PR)

- [x] 변경이 설정 플래그 뒤에 있고, 플래그 기본값이 원본 동작과 동일하다 — 플래그 없음(계측만). 모든 설정에서 동작 동일; 근거: 위 "안전성" 절, `git diff ea1d499d..f1f819c1 -- internal pkg` 에 송신·저장·검증 경로 변경 없음.
- [x] 플래그 OFF 상태에서 `baseline`과 메시지 흐름이 동일하다 — diff 검토(변경 지점: 메트릭 호출, `prePrepareBegin` 맵, `NewBatchBuilder` 인자) + `TestBasic`/`TestRestartKeepsViewAndInFlightProposal` 통과 + `internal/bft` -race 통과.
- [x] 플래그 OFF: `go build ./... && go vet ./...` 통과 (0.47 s)
- [x] 플래그 OFF: `go test ./internal/... ./pkg/...` 통과 (8.6 s)
- [x] 플래그 OFF: `go test ./test/...` 통과 — `-run 'TestBasic$|TestRestartKeepsViewAndInFlightProposal$'` (1.8 s). 전체 `./test` 는 미실행(느림; 병합 전 오케스트레이터가 실행 권장).
- [ ] 플래그 ON 테스트를 최소 1개 추가했다 — 해당 없음(플래그 없음). 대체로 신규 단위 테스트 4개 추가 (위 "테스트" 절).
- [x] 커밋 메시지가 `<area>: <요약>` 형식이며 `Flag:` 줄이 있다 — `metrics: per-sequence round latency, WAL append, in-flight, pool bytes, batch reason` / `Flag: none (instrumentation only)` / `Refs:` 포함, 서명·attribution 트레일러 없음.
- [x] `git rebase main` 완료, 다른 `feat/` 브랜치 위에 스택되지 않았다 — `main`(ea1d499d) 에서 직접 분기, 커밋 1개(f1f819c1).
- [x] 관련 설계 노트가 `docs/notes/` 에 있고 `Refs:` 로 연결했다 — `docs/notes/batcher.md`(pkg/metrics 절), `docs/notes/view-slots.md`(`beginPrePrepare` 항목), `docs/notes/state-wal.md`(WAL fsync·지연 메트릭 부재).

B~E 절은 프로토콜·Fabric·실험 변경이 없어 해당 없음.

## Refs

- docs/notes/batcher.md — `pkg/metrics` 절 (기존 메트릭 목록, 없는 것, 이름 오류)
- docs/notes/view-slots.md — `beginPrePrepare` 단일 필드 항목
- docs/notes/state-wal.md — 매 Append fsync, WAL 지연 메트릭 부재
- docs/branch-policy.md — 1-1, 2, 4, 8


## 추가 게이트 (오케스트레이터 실행, 2026-11-02)

- `go test ./test -count=1` (전체 통합 스위트, 브랜치 f1f819c1): `ok github.com/hyperledger/SmartBFT/test 127.986s`
