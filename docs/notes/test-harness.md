# test-harness
- 날짜: 2026-11-02
- 대상 커밋: SmartBFT 32a628d6 (baseline)
- probe 브랜치: 없음 — 읽기만

## 체크리스트

### [x] 인메모리 네트워크 하네스의 구조: 노드 생성, 메시지 지연 주입 가능 여부, 비잔틴 노드 흉내 가능 여부

**파일 구성 (`test/`, 전부 `package test`, 일반 .go 파일이라 외부에서 import 가능)**

| 파일 | 내용 |
|---|---|
| `test/network.go` (252줄) | `Network`(id→`*Node` 맵), `Node`(수신 큐·손실 확률·mutating 훅), `handler` 인터페이스, 메시지 라우팅 |
| `test/test_app.go` (520줄) | `App` — `pkg/api` 의 모든 의존성(Verifier/Signer/Assembler/Synchronizer/Application/Comm 등)을 한 타입으로 구현. `fastConfig`, `newNode`, `Restart`, `Disconnect`, `MutateSend`, `LoseMessages`, 공유 `committedBatches` |
| `test/reconfig.go` (112줄) | 테스트용 `Configuration`(int64 미러) ↔ `types.Configuration` 변환 (`recconfigToUint`/`recconfigToInt`) — Reconfig 요청으로 설정을 바꿀 때 사용 |
| `test/test_messages.proto` / `.pb.go` | `FwdMessage{Payload}` — 클라이언트 요청 전달용 봉투 |
| `test/basic_test.go` (3531줄) | 37개 시나리오(`TestLeaderModifiesPreprepare` 는 서브테스트 5개) + 헬퍼 `startNodes`(3524), `accelerateTime`(3496), `countCommittedBatches`(3466), `waitForCatchup`(3483), `doInBackground`(3455) |
| `test/view_split_test.go` (442줄) | `viewTracker`(로그 훅으로 뷰 체인지 진행 추적, 26-180), `newSplitCluster`(185), `holdCommits`(339), `heal`(229), `deliverRequest`(242) |
| `test/wal_truncation_test.go` (278줄) | WAL 파일 검사 헬퍼 `walLogFileNames`(31), `lastWalLogRecord`(57), `waitForSignal`(86), `waitForPrepare`(96) |
| `test/network_test.go` | `TestNetwork` — 하네스 자체의 smoke test |

**노드 생성** — `newNode(id, network, testName, testDir, rotateLeader, decisionsPerLeader) *App` (`test/test_app.go:447`)
- 설정은 **`fastConfig` 복사본에서 시작** (`test_app.go:465` `config := fastConfig`), 바뀌는 필드는 `SelfID`, `SyncOnStart=true`, `LeaderRotation`, `DecisionsPerLeader` 넷뿐 (466-469).
- `fastConfig` (`test_app.go:28-46`): `RequestBatchMaxCount: 10`, `RequestBatchMaxInterval: 10ms`, `RequestPoolSize: 40`, `RequestForwardTimeout: 500ms`, `RequestComplainTimeout: 2s`, `ViewChangeTimeout: 1m`, `LeaderHeartbeatTimeout: 1m`, `LeaderHeartbeatCount: 10`, `NumOfTicksBehindBeforeSyncing: 10`, `CollectTimeout: 200ms`, `LeaderRotation: false`.
- `app.Setup` 클로저(471-516)가 WAL을 `filepath.Join(testDir, "node<id>")` 에서 `wal.InitializeAndReadAll` 로 열고 `consensus.Consensus{Config, Scheduler: app.clock.C(1s 틱), ViewChangerTicker: app.secondClock.C, Verifier/Signer/...: app, WALInitialContent, LastProposal, LastSignatures}` 를 조립한 뒤 `network.AddOrUpdateNode(id, c, app)` (513) 하고 `c.Comm = network.GetByID(id)` (514) 로 `*Node` 를 Comm 으로 꽂는다.
- 시작은 `startNodes(nodes, network)` (`basic_test.go:3524`): 각 `Consensus.Start()` 후 `network.StartServe()` (노드별 `serve()` goroutine).
- 테스트는 보통 `t.Parallel()` + `t.TempDir()` + `defer network.Shutdown()`.

**노드별 설정 전달 방법 (3가지)**
1. `newNode` 와 `startNodes` 사이에 `n.Consensus.Config.X = v` 직접 대입. 예: `basic_test.go:1910-1915` (`SyncOnStart=false`, `ViewChangeTimeout=10s`, `LeaderHeartbeatTimeout=20s`, `RequestForwardTimeout=25s`, `RequestComplainTimeout=30s`, `SpeedUpViewChange=false`), `basic_test.go:3373-3375`.
2. 타이머를 테스트가 쥐기: `n.heartbeatTime = make(chan time.Time, 1)` / `n.viewChangeTime = ...` 를 넣고 **`n.Setup()` 을 다시 호출**하면 `Setup` 이 `c.Scheduler`/`c.ViewChangerTicker` 를 이 채널로 바꿔 끼운다 (`test_app.go:505-512`). 이후 `accelerateTime` (`basic_test.go:3496`) 이 100ms 마다 "2초씩 앞선 시각"을 밀어넣어 하트비트·뷰 체인지 타임아웃을 가속한다. 로거 훅을 붙인 뒤에도 `n.Setup()` 재호출이 필요하다 (`basic_test.go:1281`, `view_split_test.go:200`).
3. Reconfig 요청으로 런타임 변경: `Request{Reconfig: Reconfig{InLatestDecision: true, CurrentNodes, CurrentConfig: recconfigToInt(types.Reconfig{CurrentConfig: newConfig}).CurrentConfig}}` (`reconfig_test.go:40-52`) → `App.Deliver` 가 `types.Reconfig` 반환 (`test_app.go:330-338`) → 라이브러리가 컴포넌트 재생성.

> **주의 (PipelineDepthMax 에 직접 영향)**
> - `Restart()` 는 클로저가 캡처한 `config` 로 다시 조립하는데, 이전 `Consensus.Config` 에서 가져오는 건 `DecisionsPerLeader`, `LeaderRotation` 둘뿐이다 (`test_app.go:481-484`). 즉 방법 1 로 넣은 `n.Consensus.Config.PipelineDepthMax = 4` 는 **`Restart()` 후 `fastConfig` 값으로 되돌아간다**. 플래그 ON + 재시작 테스트를 쓰려면 `Setup` 의 carry-over 목록에 `PipelineDepthMax`/`AdaptiveBatch` 를 추가하거나 `fastConfig` 에 두거나 `newNode` 인자를 늘려야 한다.
> - 방법 3 은 `test/reconfig.go` 의 `Configuration` 구조체와 `recconfigToUint`/`recconfigToInt` 두 변환 함수를 거친다. 새 필드를 여기 추가하지 않으면 reconfig 후 `PipelineDepthMax` 가 **0** 이 된다 → `types.Configuration.Validate()` (`pkg/types/config.go:115`) 에서 0 을 거부하거나 0 을 1 로 취급하는 규칙이 필요하다 (아래 열린 문제).

**메시지 라우팅 (`test/network.go`)**
- 송신: `Node.SendConsensus(targetID, m)` (191-201) → 대상별 `peerMutatingFunc[targetID]` 가 있으면 `proto.Clone` 후 변조 → `Network.send(src, dst, msg)`.
- `Network.send` (116-141): 손실 판정 후 **비차단** 전달.
  ```go
  p := dstNode.lossProbability        // 수신측 전체 손실
  q := srcNode.lossProbability        // 송신측 전체 손실
  w := srcNode.peerLossProbability[target]
  r := rand.Float32()
  if r < p || r < q || r < w { return }
  select {
  case dstNode.in <- msgFrom{from: int(source), message: msg}:
  default:
      fmt.Println("Dropped msg from", source, "to", target, "due to overflow")  // incBuffSize = 1000
  }
  ```
- 수신: `Node.serve()` (213-233) 가 `in` 채널을 읽어 `*smartbftprotos.Message` 면 `app.messageLost(msg)` 필터(225-227, true 면 폐기)를 거쳐 `handler.HandleMessage`, `FwdMessage` 면 `handler.HandleRequest` 로 넘긴다. 따라서 **합의 메시지 처리 goroutine 은 노드당 1개**(순서 보존, 재정렬 없음).
- 클라이언트 요청 전달: `Node.SendTransaction` (204-206) → `FwdMessage` 로 같은 경로.
- 동기화(상태 전송) 는 네트워크를 타지 않는다: `createCommittedBatches` (`network.go:235-245`) 가 네트워크의 **모든 노드가 하나의 `committedBatches` 를 공유**하게 만들고, `App.Sync()` (`test_app.go:97-133`) 가 `cb.readAll(a.latestMD)` 로 다른 노드가 커밋한 레코드를 바로 읽어 `Deliver` 한다.

**장애 주입 훅 (전부 `test/test_app.go`)**

| 훅 | 위치 | 동작 |
|---|---|---|
| `Disconnect()` / `Connect()` | 158-183 | `Node.lossProbability = 1 / 0` — 송·수신 양방향 전부 드롭 (파티션) |
| `DisconnectFrom(t)` / `ConnectTo(t)` | 165-176 | `peerLossProbability[t] = 1.0` / delete — 단방향 링크 끊기 |
| `LoseMessages(filter)` | 199-201 | **수신측** 필터. 예: `return msg.GetCommit() != nil && blockCommits.Load() == 1` (`basic_test.go:2274-2279`) — 메시지 타입별·시점별 선택 드롭 |
| `MutateSend(target, f)` / `ClearMutateSend` | 186-197 | **송신측** 대상별 변조 훅. 복제본에 적용되므로 대상마다 다른 내용 전송(equivocation) 가능 |
| `Restart()` / `RestartSync(sync bool)` | 136-149 | `Consensus.Stop()` → `Setup()`(같은 WAL 디렉터리 재읽기) → `Config.SyncOnStart = sync` → `Start()`. "크래시 후 재기동" 모델. 네트워크 큐(`Node.in`)는 유지 |
| `DelaySync(c <-chan struct{})` | 151-155 | `Sync()` 반환 전에 `<-c` 대기 — 상태 전송 지연 |
| `Mute()` / `UnMute()` | 82-89 | 로그 레벨 토글 |
| `Network.StopServe()` | `network.go:84-92` | 모든 노드의 수신 goroutine 정지 (합의는 살아있음) |
| 로그 훅 | 예 `basic_test.go:1920-1940` | `zap.Hooks` 로 특정 로그 문자열을 WaitGroup/채널로 바꿔 **프로토콜 단계 동기화** |

**메시지 지연 주입 가능 여부 — [x] 범용 지연 훅은 없음**
- `Network.send` 에 지연·재정렬 경로가 없다 (즉시 `dstNode.in` 에 push).
- 유일한 지연 수단은 `MutateSend` 훅 안에서 블로킹하는 것 — `holdCommits` (`view_split_test.go:339-355`):
  ```go
  nodes[id-1].MutateSend(target, func(_ uint64, msg *smartbftprotos.Message) {
      if msg.GetCommit() != nil { <-releaseCh }
  })
  ```
  단, 같은 파일 332-338 주석대로 훅이 **송신 goroutine 위에서 동기 실행**되므로 "커밋 하나를 잡으면 그 노드는 release 까지 다른 메시지도 못 보낸다" (하트비트는 별도 goroutine 이라 계속 나감). 링크별 지연(netem 유사)을 넣으려면 `Network.send` 에 지연 큐(예: `time.AfterFunc(d, func(){ dstNode.in <- ... })`)를 추가하거나 `c.Comm` 을 지연 래퍼로 감싸야 한다 — 벤치에서 추가해야 할 항목.
- 확률 손실: `lossProbability` 가 float32 지만 공개 setter 는 0/1 만 있음. 같은 패키지 안에서는 `n.Node.lossProbability = 0.3` 직접 대입 가능.

**비잔틴 노드 흉내 — [x] 송신측 변조로 가능, 서명은 검증 안 함**
- `TestLeaderModifiesPreprepare` (`basic_test.go:1194-1315`) 가 리더에 5가지 변조를 걸어 뷰 체인지 후 올바른 커밋을 확인한다: `"wrong view"` (`PrePrepare.View = 1`), `"conflicting proposals"` (대상별 다른 payload = equivocation), `"next sequence"` (`Seq = 2`), `"wrong verification sequence"` (`VerificationSequence = 3`), `"wrong number of decisions"` (`ViewMetadata.DecisionsInView: 1`).
- 서명은 전부 통과: `VerifySignature` 는 `nil` 반환 (`test_app.go:258-260`), `SignProposal` 은 `aux` 를 그대로 서명값으로 (273-279), `VerifyConsenterSig` 는 `signature.Msg` 반환 (249-251). 즉 **위조 서명/잘못된 서명을 거부하는 시나리오는 하네스로 못 만든다**.
- 레이어 제한: 변조는 `smartbftprotos.Message` 레벨에서만. `FwdMessage`(요청) 변조 훅은 없음.

**벤치 관점에서 중요한 하네스 제약**
- `App.Delivered` 는 **버퍼 100** (`test_app.go:457`). `Deliver` 가 `a.Delivered <- record` (326) 로 밀어넣으므로 소비하지 않으면 합의 스레드가 블록된다. 벤치는 노드별 drain goroutine 필수.
- `Deliver` 는 같은 seq 두 번 전달 시 `Panicf("Committed sequence %d twice")` (322-324) — 파이프라인에서 "중복 커밋 없음" 을 공짜로 검사해 준다.
- `SignProposal` 은 `aux` 가 비어 있으면 `Panicf("didn't receive prepares from anyone")` (274-277) — `messageLost` 가 설정된 노드만 예외.
- WAL 은 실제 디스크 (`t.TempDir()`), 레코드마다 `logFile.Sync()` (`pkg/wal/writeaheadlog.go:476`) — 로컬 벤치 지연의 하한을 fsync 가 정한다. `wal.Options` 에는 `FileSizeBytes`, `BufferSizeBytes`, `Metrics` 만 있고 fsync 끄는 옵션은 없다 (`writeaheadlog.go:103-107`).

### [x] 기존 테스트 중 "리더 크래시 중 in-flight", "뷰 체인지 중 in-flight" 케이스 — 다중 in-flight 로 확장할 후보

모두 `W=1` 가정(in-flight 제안이 정확히 하나)이 시나리오와 어서션에 박혀 있다. 번호는 아래 "플래그 ON 테스트 계획" 의 클론 원본 참조용.

| # | 테스트 | 시나리오 | 어서션 | 다중 in-flight 로 바꿀 때 바뀌는 곳 |
|---|---|---|---|---|
| T1 | `TestNodeInFlightFails` `basic_test.go:1883` | 모든 노드가 seq1 커밋 메시지를 잃음(prepared 만 됨) → 리더 `Disconnect()` → 뷰 체인지 → 노드 2,3 은 view changer 의 **in-flight view** 로 seq1 커밋, 노드 4 는 커밋을 끝까지 못 받아 in-flight view 타임아웃 후 sync 로 따라옴 | `require.False(committedTooEarly)`; `d1==d2==d3`; 노드 4 는 1초 내 미커밋; 로그 `"In-flight view 0 with latest sequence 1 has committed a decision"`(id 2,3), `"Timeout expired waiting on In-flight 0 with latest sequence view to commit 1"`, `"Node 4 was unable to commit the in flight proposal, not changing the view"`; 최종 `d3==d4` (30s) | 훅 문자열이 단수 in-flight 전제 (`viewchanger.go:1375,1383,1200`). W개 중 일부만 prepared 인 경우의 결과 정의 필요 |
| T2 | `TestNodeInFlightThenViewChange` `basic_test.go:2260` | seq1 커밋 전부 손실 → 해제 → 노드 4 파티션 → 2번째 요청을 노드 1~3 에 제출 → 요청 타임아웃 complaint 로 뷰 체인지, in-flight seq1 + seq2 커밋 → 노드 4 재접속 후 sync 로 3개 수신 → 새 리더(노드 2) `Disconnect()` → 4번째 요청으로 다시 뷰 체인지 | `Delivered` 를 노드별로 정확히 2회/1회/3회 읽음 (개수로만 검사, 내용 비교 없음) | W개가 in-flight 인 상태에서 뷰 체인지 2회 반복. **이 테스트가 125초 걸림** (아래 실행 시간) |
| T3 | `TestNodeCommitTheRestPrepareAndCommittedNodeCrashesThenRecovers` `basic_test.go:2347` | **리더만** 커밋 수신 → 리더 seq1 커밋 후 `Disconnect()` (= 커밋 직후 리더 크래시) → 나머지 뷰 체인지 → in-flight seq1 재커밋 + seq2 → 옛 리더 재접속·sync → 새 리더 끊고 seq3 | `require.Len(firstReqs, 1)`, `require.Len(secondReqs, 1)` — seq 별 배치 바이트가 전 노드 동일 (fork 없음) | **리더 크래시 중 in-flight 의 정석 후보.** W=3 이면 리더만 seq1..3 커밋 후 크래시 → 나머지가 1..3 을 모두 같은 내용으로 복구해야 함 |
| T4 | `TestNodePreparesTheRestInPartitionThenPartitionHeals` `basic_test.go:2429` | **리더만** prepare 수신(리더만 prepared 상태, WAL 2회 기록을 `"LogRecord appended successfully"` 훅으로 확인) → 리더 `Disconnect()` → 나머지는 seq1 을 **버리고** 2번째 요청을 seq1 로 커밋 | 나머지 3노드 첫 `Delivered` 가 모두 2번째 요청 (`require.Len(secondReqs, 1)`); 옛 리더 sync 후 동일 | "prepared 아닌 in-flight 는 폐기" 규칙이 W개 각각에 적용되는지. 부분 prepared(seq1 prepared, seq2·3 미prepared) 혼합 케이스 추가 필요 |
| T5 | `TestRestartKeepsViewAndInFlightProposal` `wal_truncation_test.go:160` | 노드 4 는 pre-prepare 만 받음(`LoseMessages(msg.GetPrePrepare()==nil)`) → 제안을 WAL 에 persist → `Restart()` → 같은 뷰·같은 제안을 in-flight 로 복원하고 prepare 재전송 | 로그 `"Restored proposal with sequence"`; 재전송 prepare 의 digest == WAL 의 PrePrepare digest; `GetLeaderID()` 불변; 마지막 WAL 레코드 `proto.Equal` 동일; `RestartSync(false)` 10회에도 WAL 파일 수 불변 | 재시작 시 W개 제안 복원 (`state.go:179,245` 가 마지막 pre-prepare 하나만 복원) |
| T6 | `TestNodeOneSequenceBehindRejoins` `view_split_test.go:378` | 7노드, 뷰 0/1/2 로 분열 → `holdCommits({5,6}, all)` 로 seq2 를 in-flight 로 고정 → 뒤처진 노드 1,2 `heal` → "in-flight 인 seq 로 sync" → release 후 seq2 전달 | `viewsReached(…, 2, 90s)`, `deliverRequest(…, 2)` 가 7노드 모두 90s 안에 | 뒤처진 노드가 W 시퀀스 뒤에 있을 때 하트비트의 seq 보고·sync 판단 |
| T7 | `TestDecideAndAbortViewAtSameTime` `basic_test.go:3358` | 로그 훅 `"Sequence: 2-->3"`, `"Aborting current view with number"` 에서 블로킹해 **결정과 뷰 중단을 동시에** 만듦 | 4노드 2번째 레코드 동일, 15s 제한 | `"Sequence: %d-->%d"` (`view.go:883`) 는 W>1 에서 의미가 바뀜 |
| T8 | `TestTryCommittedSequenceTwice` `basic_test.go:3050` | 7노드, view changer 와 controller 가 같은 seq 를 둘 다 전달하려는 경쟁 | `Deliver` 의 `"Committed sequence %d twice"` 패닉이 안 나야 함 | W>1 에서 중복 전달 창이 커짐 |
| T9 | `TestViewChangeAfterTryingToFork` `basic_test.go:2533` | 7노드 중 3노드 끊어 쿼럼 붕괴 → 실패한 뷰 체인지 → 재접속 후 성공 | 모든 노드 동일 레코드 | 그대로 재사용 (W 무관 회귀) |
| T10 | `TestLeaderCatchUpWithoutSync` `basic_test.go:1316`, `TestLeaderProposeAfterRestartWithoutSync` `:1384` | 리더가 prepared 상태(`"Processed prepares for proposal with seq 1"`)에서 `RestartSync(false)` → WAL 에서 복원 후 커밋/다음 제안 | 전 노드 동일; 훅 `"Not acquiring leader token when starting view with phase PREPARED"`, `"Expected proposal sequence 2 but got 1"` | leaderToken 1개 전제 → `feat/leader-token-window` 가 깨뜨릴 가능성 큼 |
| T11 | `TestRestartAfterViewChangeAndRestoreNewView` `:684`, `TestRestoringViewChange` `:751`, `TestCatchingUpWithViewChange` `:549`, `TestNodeViewChangeWhileInPartition` `:61` | 뷰 체인지 도중/직후 재시작·파티션 | 전 노드 동일 레코드 + 로그 훅 | 회귀용 |

**로그 문자열 결합** — 테스트 동기화가 아래 메시지에 의존하므로 feat PR 에서 메시지를 바꾸면 테스트가 **hang** 한다 (대부분 WaitGroup 대기, 타임아웃 없음):
`"%d collected %d prepares from %v"` (`view.go:467`), `"Processed proposal with seq %d"` (`view.go:429`), `"Processed prepares for proposal with seq %d"` (`view.go:520`), `"%d processed commits for proposal with seq %d"` (`view.go:338`), `"Sequence: %d-->%d"` (`view.go:883`), `"Starting view with number %d, sequence %d, and decisions %d"` (`controller.go:383`), `"ViewChanged, the new view is %d"` (`controller.go:455`), `"Aborting current view with number %d"` (`controller.go:430`), `"Heartbeat timeout expired..."` (`controller.go:290`), `"get msg from syncChan"` (`controller.go:499`), `"Restored proposal with sequence %d"` (`state.go:179,245`), `"In-flight view %d with latest sequence %d has committed a decision"` (`viewchanger.go:1375`), `"Timeout expired waiting on In-flight %d ..."` (`viewchanger.go:1383`), `"Node %d was unable to commit the in flight proposal, not changing the view"` (`viewchanger.go:1200`), `"LogRecord appended successfully: ..."` (`pkg/wal/writeaheadlog.go:493`).

### [x] 라이브러리 단독으로 TPS/지연을 잴 수 있는 벤치가 있는지 — 없음

- `grep -rn "func Benchmark"` → `internal/`, `pkg/`, `test/`, `examples/` 모두 **0건**.
- `scripts/godoc.sh` 는 godoc 정적 생성 스크립트. `tools/` 는 lint·codegen 도구 go.mod 만.
- `examples/naive_chain/` (`chain.go`, `node.go`, `chain_test.go`): 4노드를 `map[int]map[int]chan proto.Message`(버퍼 128) 로 연결하는 최소 예제. `TestChain` (`chain_test.go:70`) 은 `BatchSize: 1, BatchTimeout: 10s` 로 tx 9개를 **하나씩 순차** 주문하고 블록 seq 를 확인한다. `Node.Sync()` 는 `panic("implement me")` (`node.go:48-50`) 라서 파티션·재시작·뒤처짐이 생기면 바로 죽는다 → 벤치/장애 실험 기반으로 부적합. 설정은 `bft.DefaultConfig` 기반 (`node.go:188-191`).
- 라이브러리 자체 메트릭 (`pkg/api/metrics.go`) 중 쓸 만한 것: `view_latency_batch_processing` — `view.go:399` 에서 `v.beginPrePrepare = time.Now()` (pre-prepare 처리 시작) 를 찍고 `view.go:348` 에서 커밋 쿼럼 처리 후 `Observe(time.Since(v.beginPrePrepare))` → **노드별 pre-prepare→commit 지연** 그 자체. 그 외 `view_count_txs_in_batch`, `view_count_txs_all`, `view_count_batch_all`, `view_size_batch`, `view_latency_batch_save`(WAL), `pool_latency_of_elements`(풀 체류 시간), `pool_count_of_elements`. 단, 저장소에는 `pkg/metrics/disabled` Provider 만 있어 (`pkg/metrics/provider.go:11` `Provider{NewCounter, NewGauge, NewHistogram}`) 벤치가 **수집용 Provider 를 직접 구현**해야 한다.
- **파이프라인과의 충돌**: `beginPrePrepare` 는 `View` 의 단일 필드다. W>1 에서 seq k+1 의 pre-prepare 가 seq k 커밋 전에 들어오면 덮어써서 지연값이 깨진다 → `feat/view-seq-window` 에서 `map[seq]time.Time` 으로 바꿔야 벤치 메트릭이 유효하다.

**`bench/lib/` 설계 스케치 (Fabric 없는 1차 비교용)**

재사용 (SmartBFT 포크 `test` 패키지를 import — `test_app.go`/`network.go`/`reconfig.go` 는 testify 등 테스트 전용 의존성이 없어 그대로 import 가능):
- `test.NewNetwork`, `*test.Network{AddOrUpdateNode, StartServe, Shutdown, GetByID, Nodes}`, `*test.Node`(= `api.Comm` 구현).
- `*test.App` 의 공개 메서드: `Submit(Request)`, `Delivered chan *AppRecord`, `Disconnect/Connect`, `DisconnectFrom/ConnectTo`, `MutateSend/ClearMutateSend`, `LoseMessages`, `Restart/RestartSync`, `DelaySync`, `Mute`.
- `test.Request{ClientID, ID}` / `test.AppRecord{Batch, Metadata}` (Metadata 는 `smartbftprotos.ViewMetadata` 직렬화 → seq 추출 가능).
- 복사해야 하는 비공개 심볼: `newNode` (`test_app.go:447-520`, ~70줄), `fastConfig`, `startNodes`, `requestFromBytes`/`batchFromBytes`(asn1). 또는 포크에 `test.NewNodeWithConfig(id, network, dir, cfg types.Configuration) *App` 같은 exported 생성자를 `area: test` 커밋으로 추가하는 편이 유지보수에 낫다 (플래그와 무관한 변경이라 branch-policy 위반 아님).

추가해야 하는 것:
1. **모듈 구성**: `bench/lib/go.mod` 에 `require github.com/hyperledger/SmartBFT v0.0.0` + `replace github.com/hyperledger/SmartBFT => <포크 경로>`. 비교군은 `git worktree add ../SmartBFT-baseline baseline` 으로 따로 체크아웃하고 `-modfile` 또는 환경변수로 replace 경로를 바꿔 **같은 벤치 바이너리 소스로 두 번 빌드**.
2. **노드별 설정**: `newNode` 복사본에서 `config := bench.BaseConfig` 로 바꾸고 `PipelineDepthMax`, `AdaptiveBatch`, `RequestBatchMaxCount`(fastConfig 10 → 100~1000), `RequestBatchMaxInterval`, `RequestPoolSize`(40 → 수천; `Pool.Submit` 은 세마포어가 꽉 차면 `RequestPoolSubmitTimeout` 까지 블록 후 에러, `requestpool.go:221-228`) 를 CLI 플래그로 받는다. 재시작 시 유지되도록 carry-over 도 포함.
3. **부하 생성기**: 클라이언트 C개 × open-loop(목표 rate) 또는 closed-loop(outstanding K). `ClientID` 는 클라이언트별 고유, `ID` 는 단조 증가 (`RequestInspector.RequestID` 가 `(ClientID, ID)` 로 중복 판정 → `ErrReqAlreadyExists`). `Submit` 은 리더가 아닌 노드에 넣으면 `RequestForwardTimeout` 뒤 포워딩되므로 TPS 측정은 리더에 직접 제출(또는 전 노드 분산 두 모드).
4. **타이밍 포인트**
   - end-to-end: `submit(t0)` 를 `map[(client,id)]time.Time` 에 기록, 노드별 `Delivered` drain goroutine 에서 `batchFromBytes` → 각 요청의 `t1` → 지연 히스토그램. TPS = 커밋된 요청 수 / 측정 구간.
   - pre-prepare→commit: (a) 수집용 `metrics.Provider` 로 `view_latency_batch_processing` 히스토그램 캡처(W>1 보정 전까지는 신뢰 불가), (b) 리더에 `MutateSend(target=첫 팔로워)` 훅으로 `PrePrepare.Seq` 별 송신 시각 기록 + `Delivered` 의 `ViewMetadata.LatestSequence` 로 커밋 시각 → seq 별 지연. (b) 는 라이브러리 수정 없이 동작.
   - 파이프라인 깊이 관측: 리더 로그 `"Processed proposal with seq %d"` 와 `"%d processed commits for proposal with seq %d"` 를 `zap.Hooks` 로 세어 "동시 in-flight 수" 시계열.
5. **네트워크 지연**: `Network.send` 에 링크별 지연 큐(`time.AfterFunc`) 를 넣는 변형 Network 가 필요 — `test` 패키지의 `Network.send` 는 비공개라 벤치에서는 `c.Comm` 을 감싸는 `delayComm{inner *test.Node, d time.Duration}` 로 처리 (`SendConsensus`/`SendTransaction` 을 `time.AfterFunc(d, …)` 로 지연, 큐 순서 보존 위해 per-target goroutine+채널 권장). 이것이 netem 전 단계의 "지연 증가 시 파이프라인 이득" 실험을 가능케 한다.
6. **종료·정합성 검사**: 전 노드 `Delivered` 레코드가 seq 순으로 동일한지 비교(기존 테스트의 `require.Equal(data[i], data[i+1])` 패턴), `Deliver` 의 중복 seq 패닉을 그대로 활용.
7. **출력**: `results/<yyyymmdd>-<topic>/` 에 CSV (W, batch, clients, rate, tps, p50/p95/p99 e2e, p50/p95 pp→commit, inflight max).

### 결론으로 적을 것: 플래그 ON 테스트 추가 계획 → 아래 "결론 / 설계 결정"

## 테스트 실행 시간

측정 환경: Apple M1 Pro (10코어, 16GB), Go 1.27.3 darwin/arm64, baseline 32a628d6, 빌드 캐시 warm 아님(첫 실행). 전부 1회 측정 wall-clock.

| 명령 | wall | 비고 |
|---|---|---|
| `go test ./internal/... ./pkg/...` | **17.7s** (user 33.6s, 244% CPU) | `internal/bft` 4.77s, `pkg/wal` 7.24s, `pkg/types` 0.94s, `pkg/api` 0.47s |
| `go test ./test -run 'TestBasic$' -count=1` | **2.1s** | 패키지 자체 1.11s |
| `go test ./test -run 'TestRestartKeepsViewAndInFlightProposal$' -count=1` | **1.7s** | 1.31s |
| `go test ./test -run 'TestNodeInFlightThenViewChange$' -count=1` | **125.1s** (user 0.97s, **1% CPU**) | 124.8s — 거의 전부 타이머 대기 |
| `go test ./test -count=1` (전체 통합 스위트, 최상위 Test 50개) | **139.3s** (2:19 wall, user 6.7s, **7% CPU**) | 패키지 자체 138.3s; 테스트가 `t.Parallel()` 이라 벽시계는 가장 느린 테스트(≈T2 류)에 수렴. AGENTS.md 의 "a few minutes" 와 일치 |

- 뷰 체인지가 들어가는 테스트는 CPU 가 아니라 `fastConfig` 의 실제 타이머에 묶인다. T2 를 `-v` 로 다시 돌려 귀속을 확인했다 (`go test ./test -run 'TestNodeInFlightThenViewChange$' -v`, 타임스탬프는 로그 그대로):
  - `17:06:47` 시작 → `17:06:51.49` 요청 포워드 타임아웃(500ms)+complain(2s) 경로로 노드 1·2·3 뷰 체인지 시작 → `17:06:53.17` in-flight seq1 커밋, `ViewChanged, the new view is 1`, seq2·3 까지 커밋. **여기까지 6초.**
  - 파티션에서 복귀한 노드 4 는 뷰 0 에 남아 있어 `17:07:49.17` **`LeaderHeartbeatTimeout`(1m) 만료** → `Heartbeat timeout expired, complaining about leader: 1` → 혼자 뷰 체인지 시작(쿼럼 불가).
  - `17:08:49.17` **`ViewChangeTimeout`(1m) 만료** → `get msg from syncChan` → `Sync()` 로 3건 전달 → `Starting view with number 1, sequence 4`.
  - `17:08:51.7` 두 번째 뷰 체인지(요청 타임아웃 경로, 2.5s) 완료. 총 125s = **6s + 60s + 60s**.
  - 즉 "뷰 체인지를 놓치고 복귀한 노드의 sync" 가 `LeaderHeartbeatTimeout + ViewChangeTimeout` 를 그대로 소비한다. 전체 스위트 138s 도 이 두 타이머 합(2분)에 하한이 걸려 있다고 보는 것이 합리적이다. 다중 in-flight 복구 테스트는 이 경로를 반드시 지나므로, 타이머를 줄이지 않으면 테스트당 2분이 기본값이 된다.
- PR 게이트 비용 정리: `go test ./internal/... ./pkg/...` ≈ 18s, `go test ./test -run <단일>` ≈ 2s(뷰 체인지 없음) ~ 125s(뷰 체인지+복귀 sync), 전체 통합 ≈ 2분 20초. 전부 합쳐도 3분 미만이라 feat PR 마다 OFF 전체 + ON 신규를 돌리는 데 부담은 없다.
- 가속하려면 T7/T6 처럼 `heartbeatTime`/`viewChangeTime` 채널 + `accelerateTime` 을 쓰거나 T1 처럼 `Config.ViewChangeTimeout`/`LeaderHeartbeatTimeout` 을 10~20s 로 낮춘다. 새 플래그 ON 테스트는 처음부터 이 패턴으로 쓰기를 권장.

## 결론 / 설계 결정

1. **하네스는 그대로 재사용한다.** 노드 생성(`newNode`), 파티션(`Disconnect`), 선택 드롭(`LoseMessages`), 변조(`MutateSend`), 재시작(`Restart`), 타이머 가속(`accelerateTime`) 이 다 있고 upstream 통합 테스트 50개가 그 위에 있다. 부족한 것은 (a) 링크 지연, (b) exported 설정 가능 생성자, (c) 부하 생성·측정 — 전부 벤치(`bench/lib/`) 쪽에서 추가한다.
2. **하네스 쪽 선행 수정 (플래그 무관, `test:` 커밋 1개)**: `test_app.go` `Setup` 의 carry-over 에 `PipelineDepthMax`/`AdaptiveBatch` 추가; `test/reconfig.go` `Configuration` + 두 변환 함수에 같은 필드 추가. 이게 없으면 재시작/리컨피그 테스트에서 플래그가 조용히 꺼진다.
3. **벤치는 `bench/lib/` 에 별도 Go 모듈**로, 포크의 `test` 패키지를 import 하고 `replace` 로 baseline worktree 와 main 을 번갈아 가리킨다 (위 스케치). 1차 지표: TPS, e2e p50/p95/p99, pre-prepare→commit p50/p95(MutateSend 기반), 최대 동시 in-flight. 네트워크 지연은 `delayComm` 래퍼로 0/10/50/100ms.
4. **플래그 ON 테스트 추가 계획** (PR 당 최소 1개, branch-policy 3절 순서):

| PR | 테스트 이름 | 시나리오 | 어서션 | 클론 원본 |
|---|---|---|---|---|
| `feat/adaptive-batch` | `TestAdaptiveBatchConvergesUnderLoad` | 4노드, `AdaptiveBatch=true`, 클라이언트 3개가 각 100건 제출 (`doInBackground` 로 밀어넣기) | 전 노드 레코드 동일; 총 300건 전부 커밋; 각 배치 크기가 `[min,max]` 안; 부하 구간 후반 평균 배치 크기 > 초기 (`len(record.Batch.Requests)` 로 계산) | `TestBasic` `basic_test.go:32` + `countCommittedBatches` `:3466` |
| `feat/adaptive-batch` | `TestAdaptiveBatchOffIsBaseline` | 같은 입력, 플래그 OFF | 배치 크기가 `RequestBatchMaxCount`(10)/`MaxInterval` 규칙과 정확히 일치 (회귀 고정) | 위와 동일 |
| `feat/leader-token-window` | `TestPipelineLeaderProposesUpToDepth` | 4노드, `PipelineDepthMax=3`; 팔로워 3노드에 `holdCommits` → 리더에 요청 5건 제출 | 리더 로그 훅 `"Processed proposal with seq 1/2/3"` 는 커밋 전에 발생, `"...seq 4"` 는 release 전 **미발생** (1s `time.After` 로 부정 확인); release 후 5건 전 노드 동일·seq 단조 증가 | `TestBasic` + `holdCommits` `view_split_test.go:339` |
| `feat/leader-token-window` | `TestPipelineDepthOneEqualsBaseline` | 같은 테스트, `PipelineDepthMax=1` | `"Processed proposal with seq 2"` 가 seq1 커밋 전 미발생 | 위와 동일 |
| `feat/view-seq-window` | `TestPipelineOutOfOrderPrepares` | W=3; 팔로워 2번의 `MutateSend` 로 seq1 prepare 를 seq2 prepare 송신 뒤까지 블록(채널로 순서 강제) | 커밋 순서는 1→2→3, 전 노드 동일; `Deliver` 중복 패닉 없음 | `TestLeaderModifiesPreprepare` 의 `MutateSend` 패턴 `basic_test.go:1194` |
| `feat/view-seq-window` | `TestPipelineLatencyMetricPerSequence` | W=3 + 수집용 Provider | `view_latency_batch_processing` 관측 수 == 커밋 수, 음수/0 없음 | 신규 (unit: `internal/bft/view_test.go`) |
| `feat/prev-commit-dependency` | `TestPipelineCommitSignatureChain` | W=3, 정상 경로 | 각 커밋 `signatures` 길이 ≥ 2f+1 이고 `VerifyConsenterSig` 호출 횟수 = 커밋 수 × 서명 수; 의존성 해소 방식(설계 결정 후)에 맞는 메타데이터 검사 | `TestBasic` (설계 노트 `prev-commit-dependency.md` 확정 후) |
| `feat/viewchange-multi-inflight` | `TestMultiInFlightLeaderCrashThenRecovers` | W=3; **리더만** 커밋 수신 → 리더 seq1..3 커밋 후 `Disconnect()` → 나머지 뷰 체인지 | 나머지 3노드가 seq1..3 을 리더와 **같은 내용·같은 순서**로 커밋 (`map[seq]set` 크기 1), 이후 seq4 커밋 | T3 `basic_test.go:2347` |
| `feat/viewchange-multi-inflight` | `TestMultiInFlightPartiallyPrepared` | W=3; 팔로워는 seq1 만 prepare 수신, seq2·3 은 pre-prepare 만 → 리더 끊김 | seq1 은 복구 커밋 또는 재제안 중 하나로 **정확히 한 번**; seq2·3 의 옛 내용은 prepared 아니면 커밋되지 않음; 전 노드 동일 | T4 `basic_test.go:2429` + T1 의 `LoseMessages` 시점 제어 |
| `feat/viewchange-multi-inflight` | `TestMultiInFlightViewChangeTimeoutThenSync` | T1 과 동일 구조, W=3 | 복수형으로 바뀐 로그 훅(예 `"In-flight view ... committed %d decisions"`)으로 노드 2,3 복구; 노드 4 는 타임아웃 후 sync 로 3건 수신, `d3==d4` | T1 `basic_test.go:1883` (타이머 10/20s 로 단축된 설정 그대로) |
| `feat/viewchange-multi-inflight` | `TestRestartKeepsMultipleInFlightProposals` | W=3; 고립 노드가 pre-prepare 3개 받고 `Restart()` | `"Restored proposal with sequence"` 3회; digest 별 prepare 재전송 3개; WAL 마지막 레코드들 `proto.Equal`; 10회 재시작에 WAL 파일 수 불변 | T5 `wal_truncation_test.go:160` |
| `feat/viewchange-multi-inflight` | `TestNodeWSequencesBehindRejoins` | 7노드 분열, `holdCommits` 동안 리더가 seq2..4 제안 | 뒤처진 노드 1,2 가 뷰 2 로 sync 후 release 뒤 seq2..4 전부 수신 (`deliverRequest` ×3) | T6 `view_split_test.go:378` |
| `feat/config-barrier` | `TestReconfigFlushesPipeline` | W=3; 일반 5건 → Reconfig 요청(`PipelineDepthMax` 를 2 로 바꾸는 설정) → 일반 5건 | reconfig 배치는 단독 커밋; 리더 훅으로 reconfig seq 의 `"Processed proposal"` 이 직전 seq 의 `"processed commits"` **이후**에만 발생; 이후 `"Processed proposal with seq k+2"` 가 k 커밋 전 미발생(새 W=2 적용 확인); 전 노드 동일 | `TestBasicReconfig` `reconfig_test.go:12` (test/reconfig.go 필드 추가 선행) |

모든 플래그 ON 테스트는 `t.Parallel()`, `t.TempDir()`, 타이머 단축(T1 식) 또는 `accelerateTime` 사용, 로그 훅 대기에는 `waitForSignal`(30s 상한, `wal_truncation_test.go:86`) 을 써서 hang 대신 실패하게 한다.

## 열린 문제

1. **복귀 노드 sync 2분 경로의 단축 방법**: T2 분석대로 뷰 체인지를 놓친 노드는 `LeaderHeartbeatTimeout`(1m) 뒤 complain, `ViewChangeTimeout`(1m) 뒤에야 sync 한다. 새 테스트에서는 `Config.LeaderHeartbeatTimeout`/`ViewChangeTimeout` 을 T1 처럼 10~20s 로 낮추거나 `accelerateTime` 을 쓰면 되지만, **벤치**에서 "리더 장애 중 파이프라인 복구 시간" 을 지표로 삼을 때는 Fabric 기본값(`DefaultConfig`: 하트비트 1m, 뷰 체인지 20s) 을 쓸지 결정해야 한다. 또 전체 스위트가 정확히 어떤 테스트에 하한이 걸리는지는 `go test ./test -v -count=1 | grep -- "--- PASS"` 로 테스트별 시간을 뽑아 확인하지 않았다 [?].
2. **W>1 에서 "prepared 아닌 in-flight" 의 정의**: T4 규칙(폐기)과 T1/T3 규칙(복구 커밋)이 seq 단위로 섞일 때 gap 없는 시퀀스를 어떻게 보장하는지는 `feat/viewchange-multi-inflight` 설계 노트에서 결정해야 테스트 어서션을 쓸 수 있다.
3. **`PipelineDepthMax == 0` 처리**: reconfig 경로(`test/reconfig.go`)와 Fabric 설정 블록에서 필드 누락 시 0 이 들어온다. `Validate()` 에서 거부할지, 0 을 1 로 정규화할지 결정 필요 (정규화가 branch-policy "기본값 = 원본 동작" 과 더 맞음).
4. **벤치 지연 하한 = WAL fsync**: 로컬 벤치는 레코드마다 `fsync` 라 pre-prepare→commit 이 ms 대 하한을 갖는다. 네트워크 지연을 `delayComm` 으로 10ms 이상 넣어야 파이프라인 효과가 fsync 노이즈 위로 올라온다. tmpfs(`/Volumes/RAMDisk`) 사용 여부 결정.
5. **`test` 패키지 import 의 비공개 심볼**: `newNode`/`fastConfig`/`startNodes` 를 벤치에 복사하면 upstream 변경 추적이 끊긴다. 포크에 exported 생성자(`NewNodeWithConfig`)를 추가하는 `test:` 커밋을 먼저 넣을지 결정.
6. **서명 검증 비잔틴 시나리오 불가**: 하네스가 서명을 전혀 검증하지 않으므로 "잘못된 prev_commit_signatures 를 가진 제안을 거부" 같은 테스트는 `internal/bft` 단위 테스트(mocks) 로 내려가야 한다.
