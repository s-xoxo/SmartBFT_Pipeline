# design — `feat/pipeline-window` (브랜치 4) 상세 설계

- 날짜: 2026-11-02
- 대상 커밋: SmartBFT 포크 `d9d36397` (= baseline `32a628d6` + PR #2 metrics + PR #1 config). **아래 모든 `file.go:NNN` 은 이 체크아웃 기준**이며 probe 노트(32a628d6 기준)와 수 행 차이가 난다.
- 선행 노트: [00-summary](00-summary.md) §1–3 · [leader-token-flow](leader-token-flow.md) (R1–R7) · [view-slots](view-slots.md) (I1–I11) · [batcher](batcher.md) · [viewchange-trace](viewchange-trace.md) "안전성 보강" (D1/D2) · [prev-commit-dependency](prev-commit-dependency.md) 결론
- 플래그: `Configuration.PipelineDepthMax` (`pkg/types/config.go:92`, 기본 1, 0→1 정규화 `EffectivePipelineDepth()` `:129-134`). 아래에서 **W** = `EffectivePipelineDepth()`.
- 전제: `Validate()` 가 `W>1 ⇒ LeaderRotation==false ∧ DecisionsPerLeader==0` 와 `W×RequestBatchMaxCount ≤ RequestPoolSize` 를 강제한다 (`config.go:210-218`). 따라서 W>1 경로에서는 직전 커밋 서명(`view.go:991-993, 619-632`), 블랙리스트(`:680-688, 1065-1069`), 로테이션(`controller.go:559-564`) 이 모두 비활성이다. 이 코드들은 **건드리지 않는다**.

이 문서는 구현 착수 전 리뷰용이다. 모든 신규 식별자는 `(신규)` 로 표시했고, 기존 코드의 조건은 그대로 인용했다.

---

## 1. 범위와 비범위

### 1.1 이 브랜치가 바꾸는 것

| 영역 | 변경 | 절 |
|---|---|---|
| `internal/bft/view.go` | 2슬롯(`current/next`) 상태기계를 `slots map[seq]*seqState` 윈도우로 재구성. 라우팅 `[low-W, low) / [low, low+W] / 그 밖`, `processPrepares/processCommits` 의 블로킹 루프를 슬롯별 non-blocking 스텝으로, in-order Decide 게이트, `ViewSequences` 에 low watermark 발행, prev 재전송 링(W개), **팔로워 prepare 선행 조건 D2**, **리더 pre-prepare 재브로드캐스트(W>1 전용)**, `GetMetadata/Propose` 가 `head` 기준 | §3 §4 §5 |
| `internal/bft/controller.go` | `leaderToken` 을 cap W 세마포어로, `propose()` 를 `proposerLoop` 고루틴으로 분리, 회수 규칙 R1–R7, `decide` 로테이션 경로 중복 발급 수정, drain 루프 3곳, `maybePruneInFlight` → `clearUpTo`, 전달 후 `InFlight.pruneDelivered` | §6 |
| `internal/bft/requestpool.go` | 예약 상태(`requestItem.reserved`), `Reserve/UnreserveAll` (신규), `NextRequests` 가 예약 건 건너뜀 | §7 |
| `internal/bft/util.go` | `InFlightData` 를 `map[seq]` 로 다중화, `StorePrepares(view, seq)` 가 인자 사용, `clearUpTo/pruneDelivered` (신규) | §8 |
| `pkg/consensus/consensus.go` | `Controller.PipelineDepth`, `ProposalMaker.PipelineDepth/PrePrepareResendInterval` (신규 필드) 에 설정 복사 | §2 |
| `internal/bft/mocks` | `RequestPool` 인터페이스 변경에 따른 mockery 재생성 (`.mockery.yml`) | §7 |
| 테스트 | §11 | |

### 1.2 명시적으로 남기는 것과 W>1 에서의 임시 동작

| 남기는 브랜치 | 이 브랜치의 W>1 동작 | 안전성이 유지되는 이유 |
|---|---|---|
| **5 `feat/state-wal-window`** (WAL 윈도우 복원) | `View.processProposal` 은 지금처럼 모든 pre-prepare 를 `State.Save(msg, truncateTo=true)` 로 기록한다 (`view.go:442`). `Restore` 는 마지막 1~2 레코드만 읽는다 (`state.go:124-137, 184-199`). 따라서 **W>1 상태에서 재시작하면 가장 최근 pre-prepare 하나만 in-flight 로 복원되고 그 아래의 미결정 seq 에 대해 보낸 prepare/commit 을 잊는다.** 이는 PBFT 의 "prepared 망각" 이므로 **정확하지 않다 — 문서화된 한계**. 이 브랜치의 모든 W>1 테스트는 in-flight 가 있는 노드를 재시작하지 않는다 (§11). 벤치는 파이프라인을 비운 뒤 정지한다. | (한계) |
| **6 `feat/viewchange-multi-inflight`** | `ViewData.in_flight_proposal` 은 1건이다. 이 브랜치는 `InFlightData` 의 **최저 seq 항목**을 싣는다 (§8). 새 리더의 `CheckInFlight` 는 `expectedSequence = maxLastDecision+1` 하나만 판정한다 (`viewchanger.go:852`). 그보다 높은 in-flight 는 모든 노드에서 **폐기**되고 새 뷰에서 재제안된다(요청은 풀에 남아 있고 `UnreserveAll` 로 다시 뽑힌다). 새 리더가 2 이상 뒤처지면 `checkLastDecision` 이 ViewData 를 거부해(viewchange-trace 열린 문제 1) 뷰 체인지가 `ViewChangeTimeout` 뒤 재시도/sync 로 넘어간다 — **안전하지만 라이브니스 최적이 아님.** | 안전성은 Deliver 기준이다(viewchange-trace D 정의). 어떤 정직 노드가 seq k 를 전달했다면 그 노드의 체크포인트 ≥ k 이므로 `maxLastDecision ≥ k` 이고 k 는 더 이상 in-flight 판정 대상이 아니다(뒤처진 노드는 sync 로 받는다). 전달되지 않은 k 는 어느 값으로 다시 결정되어도 두 정직 노드가 다른 값을 전달하는 일이 없다. 폐기되는 슬롯의 투표·인증서는 뷰 객체와 함께 소멸한다(`abortView → currView.Abort()`, `controller.go:431`). `InFlightData` 의 상위 항목이 남는 노출은 baseline 이 단일 항목에서 겪는 것과 같은 종류다(§8.4). 완전한 D1(`clearAbove(base+m)`)은 6 에서. |
| **7 `feat/config-barrier`** | 설정 트랜잭션이 in-flight 에 섞이면 그 뒤 seq 의 `verifyProposal` 이 `VerificationSequence` 불일치(`view.go:613-617`)로 실패 → `Complain + Sync + stop` (`:417-421`) → 뷰 체인지. | 거부는 안전하다(거부된 제안은 prepare 되지 않음). 테스트는 W>1 에서 reconfig 를 하지 않는다. |
| **8 `feat/api-prev-proposal`** | `Assembler.AssembleProposal(metadata, requests)` (`pkg/api/dependencies.go:36`) 가 직전 제안이 전달되기 전에 호출된다. 라이브러리 하네스의 `App.AssembleProposal` (`test/test_app.go:282-288`) 과 `VerifyProposal` (`:213-222`) 은 직전 블록에 의존하지 않으므로 하네스에서는 문제 없음. Fabric 은 8·9 에서. | 라이브러리 범위 밖 |

로테이션 ON 은 §0 전제대로 W>1 에서 설정상 불가능하므로 설계 대상이 아니다. W=1 경로는 로테이션·서명 체인·블랙리스트 코드를 그대로 지나간다.

---

## 2. 플래그 분기 원칙

원칙: **코드 경로는 하나이고 W 로 매개화한다.** `if W == 1` 분기는 아래 표의 두 곳에만 두며 각각 이유를 적었다. "W=1 바이트 동일" 의 정의는: *같은 입력 이벤트 열에 대해 (i) 송신되는 합의 메시지의 집합과 내용, (ii) 같은 seq 에 대한 메시지 순서, (iii) WAL 레코드 열, (iv) `ViewData` 바이트, (v) 테스트가 훅을 거는 로그 문자열(§11.4)* 이 baseline 과 같다. 서로 다른 seq/노드 간 유니캐스트의 인터리빙은 baseline 에서도 비결정적이므로 포함하지 않는다.

### 2.1 바뀌는 함수와 W=1 경로

| 함수 (현재 위치) | 변경 | W=1 에서 왜 동일한가 |
|---|---|---|
| `View.processMsg` `view.go:201-267` | 라우팅을 `low/W` 로 일반화 (§3.3) | W=1: prev `{low-1}`, 수용 `{low, low+1}`, 그 밖 → `discoverIfSyncNeeded`. 현재 `:224`(`== ProposalSequence-1`), `:231`(`!= S && != S+1`) 과 같은 분할 |
| `View.processPrePrepare` `:308-331` | 채널 cap 1 → 슬롯의 `prePrepare` 포인터 nil 검사 | 중복 pre-prepare 드롭 + 같은 Warn 문자열(`:329`). 추가 Warn 1건 가능(§3.9 비고) — 송신 없음 |
| `View.run` `:269-287` / `doPhase` `:289-306` | `default: doPhase()` 스핀 구조를 이벤트 구동(`incMsgs`/검증 서명/abort/재전송 틱)으로; `doPhase` 는 열린 슬롯을 seq 순으로 non-blocking 스텝 | W=1: 열린 슬롯은 `low` 하나. 단계 진입 시 1회 브로드캐스트(prepare→`:292`, commit→`:295`)를 `prepareBroadcast/commitBroadcast` 플래그로 재현. 복원 슬롯은 플래그 false 로 시작 → 첫 `doPhase` 가 `lastBroadcastSent` 를 송신(현재 `:292/:295` 복원 동작) |
| `View.processProposal` `:380-457` | 인자가 슬롯. `prev*Sent` 교체(`:381-387`) 제거(슬롯 링이 대신) | 동일 순서: verify → metric → `recordPrePrepareBegin` → `createPrepare` → `Save(..., true)` → `prepareSent` 복제 → 리더면 pre-prepare 브로드캐스트 → `"Processed proposal with seq %d"` |
| `View.processPrepares` `:471-548` → `stepPrepares(slot)` (신규) | 블로킹 select 루프를 "쌓인 표를 모두 소비, 정족수면 전이" 로 | 표는 `incMsgs` 처리 직후 `doPhase` 에서 소비되므로 정족수 도달 시점·송신(commit 1회)·WAL `Save(commit,false)`·로그 동일 |
| `View.processCommits` `:550-582` → `stepCommits(slot)` (신규) | 서명 검증 고루틴은 유지(`:570-572`), 결과를 뷰 단일 채널 `verifiedSigs` 로 받음(seq 태그) | 검증 결과 도착이 `run` 의 select 를 깨우므로 현재의 `validVotes` case(`:573`) 와 동치 |
| `View.prepared` `:333-357` + `decide` `:882-888` + `startNextSeq` `:890-932` → `onCommitQuorum(slot)` + `tryDeliver()` (신규) | 정족수 시 로그·메트릭, **slot.seq == low 일 때만** `advanceLow()`(= startNextSeq 의 ++/Store/로그) 후 `Decider.Decide` | W=1: 정족수가 차는 슬롯은 항상 `low` → 즉시 전달. 로그 순서 `"%d processed commits…"` → `"Deciding on seq %d"` → `"Sequence: %d-->%d"` → `Decide` 동일 |
| `View.handlePrevSeqMessage` `:749-787` | `prevPrepareSent/prevCommitSent` 대신 `slots[s].prepareSent/commitSent` | W=1: 링 크기 1 = 직전 seq. §2.3 의 예외 1 참고 |
| `View.discoverIfSyncNeeded` `:789-849` | **변경 없음** | 같은 뷰의 commit 이 여기 오는 경로는 `:231` 분기(수용 구간 밖)뿐이고, 수용 구간이 `[low, low+W]` 이면 그런 commit 은 항상 `seq > low+W ≥ low+1` 이어서 `:839` 조건(`vote.seq <= ProposalSequence`)은 거짓 → 동작 동일 |
| `View.verifyProposal` `:584-635` | `:603` `md.LatestSequence != v.ProposalSequence` → `!= slot.seq`; `:608` `md.DecisionsInView != v.DecisionsInView` → `!= v.expectedDecisions(slot.seq)` (§6.7) | W=1: 처리되는 슬롯은 `low` 만 → `slot.seq == ProposalSequence`, `expectedDecisions(low) == DecisionsInView`. 로그 문자열·에러 문자열 동일 |
| `View.GetMetadata` `:935-963` | `LatestSequence: v.head`, `DecisionsInView: expectedDecisions(head)`; `seqLock` 보호 | W=1: 토큰이 있으면 in-flight 0 ⇒ `head == low` (I2) → 현재와 같은 값. 로그 `"GetMetadata with view %d, seq %d, dec %d"` 동일 |
| `View.Propose` `:989-1015` | `seq := v.ProposalSequence` (`:995`) → `seq := v.head; v.head++` | 위와 같은 이유로 동일 seq |
| `Controller.Start` `controller.go:796-828` | `leaderToken = make(chan struct{}, c.PipelineDepth)`; `go c.proposerLoop()` | W=1: cap 1. `decisionChan` cap 1 유지(`:803`) |
| `Controller.run` `:477-514` | `case <-c.leaderToken: c.propose()` (`:496-497`) 제거 → proposerLoop | 토큰 소비 주체만 바뀜. 영향 분석 §6.2 |
| `Controller.propose` `:463-475` | `Reserve` 호출 추가, `currentView()` 스냅샷 + `Stopped()` 검사(R7) | 메시지 동일. 예외 §2.3 의 2 |
| `Controller.startView` `:363-384` | `acquireLeaderToken()` 1회 → `acquireLeaderTokens(W - restored)`; 앞서 `RequestPool.UnreserveAll()` | W=1: `restored∈{0,1}` → 1 또는 0 개 = 현재 `:374-379`. 두 로그 문자열 유지 |
| `Controller.abortView` `:416-434`, `Stop` `:851-855`, `StopWithPoolPause` `:868-872` | 비블로킹 1회 수신 → `drainLeaderTokens()` (빌 때까지) | cap 1 채널에서 "빌 때까지" = 1회 |
| `Controller.decide` `:516-569` | `rotated` 플래그로 `:566-568` 생략(R4); `InFlight.pruneDelivered(seq)` 호출 | W=1 채널 상태 동일(§6.4). `pruneDelivered` 는 항목이 1개면 no-op(§8.2) |
| `Controller.maybePruneInFlight` `:695-718` | `InFlight.clear()` (`:717`) → `clearUpTo(syncedSeq)`; 비교(`:709`)는 항목별 | 항목 1개면 동일 판정 |
| `InFlightData.*` `util.go:184-260` | map 다중화 | 항목 1개면 모든 getter 가 그 항목을 돌려줌 |
| `Pool.NextRequests` `requestpool.go:296-331` | 예약 건 건너뜀, `check` 가 미예약 수/바이트 사용 | W=1 에서 호출 시점에 예약 건이 0 임을 §7.3 에서 증명 |
| `PersistedState`, `viewchanger.go`, `heartbeatmonitor.go`, `statecollector.go` | **변경 없음** | |

### 2.2 `if W == 1` 분기를 두는 곳 (2곳)

1. **리더 pre-prepare 재브로드캐스트 틱** — `resendTick` 채널은 `W>1 && SelfID==LeaderID` 일 때만 생성되고, W=1 이면 nil 채널(select 에서 영원히 발화하지 않음). 이유: baseline 은 pre-prepare 를 **단 1회** 보낸다(`view.go:451-453`). 주기 재전송을 W=1 에서 켜면 메시지 집합이 달라진다. §5.
2. **중복 pre-prepare 로그 레벨** — 슬롯에 이미 같은(`proto.Equal`) pre-prepare 가 있고 `W>1` 이면 Debug, 그 외(W=1 또는 내용이 다름)는 기존 Warn (`"Got a pre-prepare for %s sequence without processing previous one, dropping message"`, `view.go:329`). 이유: 재브로드캐스트가 매 틱 모든 팔로워에서 W개의 Warn 을 만들기 때문. `TestTwoPrePreparesInARow` (`view_test.go:1346-1347`) 는 같은 메시지를 두 번 보내고 Warn 을 기대하므로 W=1 은 그대로 둔다. 네트워크 송신과 무관.

### 2.3 W=1 에서 의도적으로 허용하는 차이 (리뷰어 확인 요청)

1. **prev 재전송 응답의 정확성** — baseline 은 `prevPrepareSent/prevCommitSent` 를 다음 seq 의 `processProposal` 진입 시에만 갱신한다(`view.go:381-382`). `decide(S)` 반환 후 `processProposal(S+1)` 진입 전의 한 select 반복 동안 S 에 대한 질의가 오면 **S-1 의 메시지**를 보낸다. 슬롯 링은 S 의 메시지를 보낸다. 응답 내용이 그 창에서만 다르며 baseline 쪽이 결함이다. 재현을 요구하면 `delivered` 슬롯의 `prepareSent/commitSent` 를 `processProposal(S+1)` 시점까지 숨기는 플래그로 복제할 수 있으나 권하지 않음(§13-8).
2. **죽어가는 뷰에 대한 제안 억제** — baseline 은 `propose()` 가 `run` 고루틴에서 순차 실행되므로 `NextBatch()` 가 돌아온 뒤 abort 가 그 뒤에 처리되어 pre-prepare 가 한 번 더 나갈 수 있다. proposerLoop 는 `view.Stopped()` 를 보고 배치를 버린다(토큰도 폐기, R7). 그 경쟁 창에서 "뷰 중단 직전의 무의미한 pre-prepare 1건" 이 나가지 않을 수 있다. 정족수에 도달할 수 없는 메시지이므로 프로토콜 관찰 동작은 같다.
3. **중복 pre-prepare 에 대한 Warn 1건 추가** — baseline 에서 `processProposal` 이 `v.prePrepare` 를 소비한 뒤 도착한 같은 seq 의 중복은 빈 채널에 들어가 `startNextSeq` 의 드레인(`:915-917`)으로 조용히 사라진다. 슬롯 설계는 `prePrepare != nil` 로 Warn 을 남긴다. 송신 없음.

---

## 3. View 슬롯 상태 머신

### 3.1 `seqState` (신규) — 슬롯별 vs 뷰별

view-slots 노트의 분류표(`view.go:90-131`)를 그대로 따른다. 슬롯별 16개 필드가 `seqState` 로 묶이고, 뷰 수준에는 `ProposalSequence`(=low), `DecisionsInView`(=low 의 기대값), `lastVotedProposalByID`, `incMsgs`, `abortChan/stopOnce/viewEnded`, `blacklistSupported`, `ViewSequences`, `prePrepareBegin`(PR #2, 이미 seq별 map) 이 남는다.

```go
// internal/bft/view.go (신규)
type slotPhase uint8
const (
    slotEmpty     slotPhase = iota // 표 또는 pre-prepare 가 도착했으나 아직 처리(검증·prepare 송신) 전
    slotProposed                   // pre-prepare 검증·WAL 기록·prepare 송신 (현 PROPOSED)
    slotPrepared                   // prepare 정족수 → commit 서명·WAL 기록·commit 송신 (현 PREPARED)
    slotCommitted                  // commit 정족수 수집 완료, 전달 대기 (seq != low 이면 보류)
    slotDelivered                  // Decider.Decide 완료. prepareSent/commitSent 만 재전송용으로 보존
)

type seqState struct {
    seq   uint64
    phase slotPhase
    // view.go:98-116 의 seq 종속 필드를 그대로 이동
    prePrepare       *protos.Message      // 현 prePrepare/nextPrePrepare chan(cap 1). 두 번째는 드롭 (I1)
    proposal         *types.Proposal      // 현 inFlightProposal
    requests         []types.RequestInfo  // 현 inFlightRequests
    prepares         *voteSet             // 현 prepares/nextPrepares (lazy, clear(N))
    commits          *voteSet             // 현 commits/nextCommits
    sigCollector     *voteVerifier        // 현 processCommits 의 signatureCollector. validVotes 대신 v.verifiedSigs 에 seq 태그로 송신
    signatures       []types.Signature    // 수집된 commit 서명 (slotCommitted 에서 보관)
    mySig            *types.Signature     // 현 myProposalSig
    lastBroadcastSent *protos.Message     // 현 lastBroadcastSent (prepare 또는 commit)
    prepareSent      *protos.Message      // 현 currPrepareSent (Assist=true 복제본)
    commitSent       *protos.Message      // 현 currCommitSent
    prepareBroadcast bool                 // doPhase :292 의 "단계 진입 시 1회 송신" 재현
    commitBroadcast  bool                 // doPhase :295
    voterIDs         []uint64             // processPrepares 의 voterIDs (PreparesFrom)
}

type View struct {
    // ... 기존 설정 필드 (view.go:74-94) 유지. 의미 재정의:
    ProposalSequence uint64   // = low: 최초 미전달 seq. 전달 시점에만 +1 (I3, I4)
    DecisionsInView  uint64   // = seq low 의 제안이 가져야 할 DecisionsInView. low 와 함께 +1
    Phase            Phase    // 유지: Restore(state.go:114,170,227)·viewchanger(:1316) 의 핸드오프 입력 + view_phase 메트릭 출력(= slots[low] 의 phase)
    PipelineDepth    uint64   // (신규) W. 0 은 1 로 취급 (viewchanger 의 in-flight View 는 설정하지 않음)
    resendTick       <-chan time.Time  // (신규) W>1 리더만 non-nil (§5)
    // 복원 핸드오프 (state.go:154-172, 214-242, viewchanger.go:1328-1350 가 그대로 씀) — Start() 가 slots[low] 로 옮긴다
    inFlightProposal *types.Proposal; inFlightRequests []types.RequestInfo; lastBroadcastSent *protos.Message; myProposalSig *types.Signature
    // Runtime (신규)
    seqLock sync.Mutex        // ProposalSequence/DecisionsInView/head: 뷰 고루틴이 쓰고 proposer 고루틴(GetMetadata/Propose)이 읽음
    head    uint64            // 리더: 다음에 제안할 seq. 불변식 low ≤ head ≤ low+W. 팔로워: == low
    slots   map[uint64]*seqState  // 키 ∈ [low-W, low+W]
    verifiedSigs chan verifiedSig  // (신규) {seq uint64; sig types.Signature}, cap N*(W+2)
    // 삭제: prePrepare/nextPrePrepare chan, prepares/commits/nextPrepares/nextCommits, curr*/prev*Sent
}
```

`depth()` 헬퍼: `if v.PipelineDepth == 0 { return 1 }; return v.PipelineDepth`.

### 3.2 슬롯 수명주기

```
(없음) ──첫 표 또는 pre-prepare──▶ slotEmpty ──processProposal 성공──▶ slotProposed ──prepare 정족수──▶ slotPrepared
   ──commit 정족수──▶ slotCommitted ──(seq == low) tryDeliver──▶ slotDelivered ──(seq < low-W) gc──▶ 삭제
```

- **lazy 생성** (`v.slot(s)`): 표가 pre-prepare 보다 먼저 올 수 있고 그 표는 보존돼야 한다(현 `nextPrepares` 수용 `:250-257` 의 성질). 생성 시 `prepares.clear(N)`, `commits.clear(N)` 2회 할당.
- **`Start()` 에서의 복원 변환**: `Phase ∈ {PROPOSED, PREPARED}` 이면 `slots[low]` 를 핸드오프 필드로 만든다 — `proposal=inFlightProposal, requests=inFlightRequests, lastBroadcastSent=lastBroadcastSent, mySig=myProposalSig, phase = PROPOSED→slotProposed / PREPARED→slotPrepared, prepareBroadcast=commitBroadcast=false`. `head = low + 1`. 그렇지 않으면 `head = low`. 이로써 `state.go` 와 `viewchanger.go:1296-1350`(in-flight View 가 `Phase: PREPARED` 와 네 필드를 직접 대입하고 `Start()`) 을 수정하지 않는다. 복원 슬롯의 `prepareSent` 는 baseline 과 같이 nil(복원되지 않음).
- **`slotDelivered` 의 메모리**: `release()` 가 `proposal/requests/prepares/commits/sigCollector/signatures/prePrepare` 를 nil 로 두고 `prepareSent/commitSent` 만 남긴다.
- **gc**: `advanceLow()` 뒤 `for s := range slots { if low > W && s < low-W { delete } }`. 맵 크기 ≤ 2W+1.

### 3.3 라우팅 규칙 (`processMsg`)

S = `v.ProposalSequence`(low), s = `proposalSequence(m)`, W = `depth()`.

| 구간 | 조건 | 처리 | baseline (W=1) 대응 |
|---|---|---|---|
| 과거(링 밖) | `s < low && low - s > W` | `Infof("%d got message from %d with sequence %d but our sequence is %d")` + `discoverIfSyncNeeded` (commit 만 의미) | `:231-235` (s ≤ S-2) |
| prev 링 | `s < low && low - s ≤ W` | `handlePrevSeqMessage(slots[s])` — `slotDelivered` 슬롯의 `prepareSent/commitSent` 로 유니캐스트, `Assist` 규칙 유지 | `:224-227` (s == S-1) |
| 수용 | `low ≤ s ≤ low+W` | `slot := v.slot(s)`; pre-prepare → `storePrePrepare(slot)`, 자기 표 무시, prepare/commit → `slot.prepares/commits.registerVote` | `:237-266` ({S, S+1}) |
| 미래 | `s > low+W` | 위 "과거" 와 같은 로그 + `discoverIfSyncNeeded` | `:231-235` (s ≥ S+2) |

**수용 구간이 W+1 칸인 이유.** 정직한 리더는 `s < low_L + W` 인 pre-prepare 만 보낸다(I2). 정상 상태의 팔로워는 리더보다 전달이 1 뒤처질 수 있다(리더는 commit 정족수를 먼저 모아 `low_L = low_F + 1`). 그러면 팔로워가 받는 최고 seq 는 `low_F + W`. baseline 의 `next` 슬롯(S+1)이 바로 이 1칸이다. 따라서 W=1 → `{S, S+1}` 로 정확히 환원된다.

**처리 구간은 W 칸.** `doPhase` 는 `[low, low+W)` 의 슬롯만 `processProposal`(검증·WAL·prepare 송신)한다. 슬롯 `low+W` 는 **수용만** 한다(표·pre-prepare 보관) — baseline 의 `nextPrePrepare` 가 `startNextSeq` 의 swap 까지 대기만 하는 것과 같다(`:400` 은 `v.prePrepare` 만 읽음). 이것을 지키지 않으면 W=1 에서 팔로워가 S 전달 전에 S+1 의 prepare 를 보내고 `Save(.., true)` 로 S 의 WAL 레코드를 절단하게 된다.

```go
func (v *View) processMsg(sender uint64, m *protos.Message) {
    if v.Stopped() { return }
    msgViewNum, s := viewNumber(m), proposalSequence(m)
    if msgViewNum != v.Number { /* view.go:209-222 그대로 */ return }
    low, W := v.ProposalSequence, v.depth()
    if s < low {
        if low-s <= W { v.handlePrevSeqMessage(s, sender, m); return }   // :224-227 일반화
        v.Logger.Infof("%d got message from %d with sequence %d but our sequence is %d", v.SelfID, sender, s, low)
        v.discoverIfSyncNeeded(sender, m); return
    }
    v.Logger.Debugf("%d got message %s from %d with seq %d", v.SelfID, MsgToString(m), sender, s)  // :229
    if s > low+W {
        v.Logger.Infof("%d got message from %d with sequence %d but our sequence is %d", v.SelfID, sender, s, low)  // :232
        v.discoverIfSyncNeeded(sender, m); return
    }
    slot := v.slot(s)                                   // lazy
    if pp := m.GetPrePrepare(); pp != nil { v.storePrePrepare(slot, pp, m, sender); return }
    if sender == v.SelfID { return }                    // :246-248
    if m.GetPrepare() != nil { slot.prepares.registerVote(sender, m); return }
    if m.GetCommit()  != nil { slot.commits.registerVote(sender, m);  return }
}

func (v *View) storePrePrepare(slot *seqState, pp *protos.PrePrepare, m *protos.Message, sender uint64) {
    if pp.GetProposal() == nil { Warnf(":310"); return }
    if sender != v.LeaderID    { Warnf(":314"); return }
    if slot.prePrepare != nil {                         // I1: 최초 1개만
        if v.depth() > 1 && proto.Equal(slot.prePrepare, m) { v.Logger.Debugf("duplicate pre-prepare for seq %d (resend), ignoring", slot.seq); return }  // §2.2-2
        currentOrNext := "current"; if slot.seq != v.ProposalSequence { currentOrNext = "next" }
        v.Logger.Warnf("Got a pre-prepare for %s sequence without processing previous one, dropping message", currentOrNext)  // :329 문자열 유지
        return
    }
    slot.prePrepare = m
}
```

### 3.4 `run` 과 `doPhase`

baseline `run` 은 `default: doPhase()` 로 돌지만 스핀하지 않는 이유는 `processProposal/processPrepares/processCommits` 가 내부에서 `incMsgs` 를 기다리며 블록하기 때문이다(`:394-413, 476-491, 562-577`). 슬롯 스텝이 non-blocking 이 되면 `default` 는 busy-loop 이 되므로 `run` 을 이벤트 구동으로 바꾼다. 이벤트 집합은 baseline 의 세 블로킹 루프가 기다리던 것과 같다: `abortChan`, `incMsgs`, 검증된 commit 서명(현 `signatureCollector.validVotes` `:573`), 그리고 W>1 리더 전용 재전송 틱.

```go
func (v *View) run() {
    defer v.viewEnded.Done()
    defer func() { v.ViewSequences.Store(ViewSequence{ProposalSeq: v.ProposalSequence, ViewActive: false}) }()  // :272-275
    v.doPhase()   // 복원 슬롯이 있으면 lastBroadcastSent 를 1회 송신 (현 :292/:295 의 복원 동작)
    for {
        select {
        case <-v.abortChan: return
        case msg := <-v.incMsgs:  v.processMsg(msg.sender, msg.Message)
        case vs  := <-v.verifiedSigs: v.acceptVerifiedSig(vs)         // slots[vs.seq].signatures = append(...)  (슬롯이 없거나 delivered 면 무시)
        case <-v.resendTick:      v.resendPrePrepares()                // nil 채널이면 영원히 발화 안 함 (W=1)
        }
        if v.Stopped() { return }
        v.doPhase()
    }
}

func (v *View) doPhase() {
    v.tryDeliver()                                       // 앞선 패스에서 committed 된 low 슬롯
    low, W := v.ProposalSequence, v.depth()
    for s := low; s < low+W; s++ {                       // 처리 구간 [low, low+W), seq 순 (D2 의 접두사 성질)
        slot := v.slots[s]
        if slot == nil { break }                         // 아무것도 안 온 seq: 그 위도 처리 불가 (D2)
        if slot.phase == slotEmpty {
            if slot.prePrepare == nil { break }          // D2: 간극. 상위 슬롯의 proposal 처리 중단 (표는 계속 쌓임)
            if !v.processProposal(slot) { return }       // 실패 시 Complain+Sync+stop (:417-421) — 뷰 종료
        }
        if slot.phase == slotProposed {
            if !slot.prepareBroadcast { v.Comm.BroadcastConsensus(slot.lastBroadcastSent); slot.prepareBroadcast = true }  // :292
            v.stepPrepares(slot)
        }
        if slot.phase == slotPrepared {
            if !slot.commitBroadcast { v.Comm.BroadcastConsensus(slot.lastBroadcastSent); slot.commitBroadcast = true }    // :295
            v.stepCommits(slot)
        }
        // slotCommitted 는 tryDeliver 가 처리
    }
    v.tryDeliver()
    v.Phase = phaseOf(v.slots[v.ProposalSequence]); v.MetricsView.Phase.Set(float64(v.Phase))   // :305
}
```

W=1 환원: 처리 구간은 `{low}`. `processProposal` 성공 → 같은 패스에서 prepare 송신 → 쌓인 prepare 소비 → (정족수면) commit 생성·송신 → 쌓인 commit 소비 → (정족수면) `tryDeliver`. baseline 은 각 단계 사이에 `run` 의 select 를 한 번 거치지만, 그 사이 처리되는 `incMsgs` 는 표 등록·prev 유니캐스트·sync 판단만 하므로 **같은 seq 의 송신 순서(pre-prepare → prepare → commit)와 집합이 같다.**

### 3.5 슬롯 스텝 (non-blocking)

```go
func (v *View) processProposal(slot *seqState) bool {    // 현 :380-457 의 몸통. 반환 false = 뷰 종료
    pp := slot.prePrepare.GetPrePrepare(); prop := pp.GetProposal()
    proposal := types.Proposal{VerificationSequence: int64(prop.GetVerificationSequence()), Metadata: prop.GetMetadata(), Payload: prop.GetPayload(), Header: prop.GetHeader()}
    requests, err := v.verifyProposal(slot.seq, proposal, pp.GetPrevCommitSignatures())   // §6.7: slot.seq 로 검사
    if err != nil { Warnf(":417"); v.FailureDetector.Complain(v.Number, false); v.Sync.Sync(); v.stop(); return false }
    v.MetricsView.CountTxsInBatch.Set(float64(len(requests)))
    v.recordPrePrepareBegin(slot.seq)
    prepareMessage := v.createPrepare(slot.seq, proposal)
    savedMsg := ProposedRecord{PrePrepare: pp, Prepare: prepareMessage.GetPrepare()}
    if err = v.State.Save(savedMsg, true); err != nil { Panicf }     // truncateTo=true 유지 (§13-2)
    slot.lastBroadcastSent = prepareMessage
    slot.prepareSent = proto.Clone(prepareMessage); slot.prepareSent.GetPrepare().Assist = true
    slot.proposal, slot.requests = &proposal, requests
    if v.SelfID == v.LeaderID { v.Comm.BroadcastConsensus(slot.prePrepare) }   // :451-453
    v.Logger.Infof("Processed proposal with seq %d", slot.seq)                 // :455
    slot.phase = slotProposed
    return true
}

func (v *View) stepPrepares(slot *seqState) {             // 현 :471-548 의 select 루프를 drain 으로
    expectedDigest := slot.proposal.Digest()
    for len(slot.voterIDs) < v.Quorum-1 {
        select {
        case vote := <-slot.prepares.votes:
            if vote.GetPrepare().GetDigest() != expectedDigest { Warnf(":486"); continue }
            slot.voterIDs = append(slot.voterIDs, vote.sender)
        default: return                                    // 더 기다릴 표 없음 → 다음 이벤트에서 재시도
        }
    }
    v.Logger.Infof("%d collected %d prepares from %v", v.SelfID, len(slot.voterIDs), slot.voterIDs)   // :493
    // :502-528 그대로: PreparesFrom → SignProposal → slot.mySig → commitMsg(seq=slot.seq)
    if err := v.State.Save(&SavedMessage_Commit{commitMsg}, false); err != nil { Panicf }           // :539, truncate 안 함
    slot.commitSent = proto.Clone(commitMsg); slot.commitSent.GetCommit().Assist = true
    slot.lastBroadcastSent = commitMsg
    v.Logger.Infof("Processed prepares for proposal with seq %d", slot.seq)                           // :546
    slot.phase = slotPrepared
}

func (v *View) stepCommits(slot *seqState) {              // 현 :550-582
    if slot.sigCollector == nil { slot.sigCollector = &voteVerifier{v: v, proposal: slot.proposal, expectedDigest: slot.proposal.Digest(), seq: slot.seq} }
    for { select { case vote := <-slot.commits.votes: go slot.sigCollector.verifyVote(vote.Message); default: goto done } }  // :570-572 그대로; 결과는 v.verifiedSigs 로
done:
    if len(slot.signatures) < v.Quorum-1 { return }
    v.Logger.Infof("%d collected %d commits from %v", v.SelfID, len(slot.signatures), ids(slot.signatures))   // :579
    v.onCommitQuorum(slot)
}
```

`voteVerifier.verifyVote` (`:858-880`) 는 `vv.validVotes <- sig` 대신 `vv.v.verifiedSigs <- verifiedSig{seq: vv.seq, sig}` 를 보낸다. 채널 cap `N*(W+2)` ≥ (슬롯 수) × (발신자당 1표, `voteSet.voted` 로 보장 `util.go:128-134`) 이므로 송신이 블록되지 않는다 — baseline 의 `make(chan types.Signature, cap(v.commits.votes))` (`:554`) 와 같은 논거.

### 3.6 commit 정족수와 in-order 게이트

```go
func (v *View) onCommitQuorum(slot *seqState) {           // 현 prepared() :333-352 의 전반
    v.Logger.Infof("%d processed commits for proposal with seq %d", v.SelfID, slot.seq)   // :342 (테스트 훅)
    v.MetricsView.CountBatchAll.Add(1); CountTxsAll.Add(len(slot.requests)); SizeOfBatch.Add(size)   // :344-351
    v.observeBatchProcessingLatency(slot.seq)             // :352 (PR #2: seq 별)
    slot.phase = slotCommitted                            // 전달은 tryDeliver 가 seq == low 일 때만
}

func (v *View) tryDeliver() {                             // I3 게이트
    for {
        slot := v.slots[v.ProposalSequence]
        if slot == nil || slot.phase != slotCommitted || v.Stopped() { return }
        v.Logger.Infof("Deciding on seq %d", slot.seq)    // :883
        v.advanceLow()                                    // 현 startNextSeq :890-909: ++, Store, 메트릭, "Sequence: %d-->%d"
        signatures := append(slot.signatures, *slot.mySig)   // :886 — 슬롯의 서명만 (I6)
        v.Decider.Decide(*slot.proposal, signatures, slot.requests)   // 블로킹 (controller.Decide :887-920) — §6.5
        v.MetricsView.InFlightProposals.Set(float64(len(v.prePrepareBegin)))  // :355
        slot.phase = slotDelivered; slot.release()
        v.gcSlots()
    }
}

func (v *View) advanceLow() {
    v.seqLock.Lock()
    prev := v.ProposalSequence
    v.ProposalSequence++; v.DecisionsInView++             // :893-894 (둘이 항상 함께 — §6.7 의 근거)
    if v.head < v.ProposalSequence { v.head = v.ProposalSequence }   // 팔로워/복원 리더
    next := v.ProposalSequence
    v.seqLock.Unlock()
    v.ViewSequences.Store(ViewSequence{ProposalSeq: next, ViewActive: true})   // :904 — low watermark
    v.MetricsView.ProposalSequence.Set(...); DecisionsInView.Set(...)          // :906-907
    v.Logger.Infof("Sequence: %d-->%d", prev, next)                            // :909 (테스트 훅 T7)
}
```

`Decider.Decide` 가 블록하는 동안 뷰 고루틴은 메시지를 처리하지 않는다 — baseline 과 같다(`controller.go:915-919`). 연속 committed 슬롯은 루프에서 차례로 전달되며, 각 반복 전 `Stopped()` 를 확인하므로 abort 경로의 `go c.enqueueDecision(d)` (`controller.go:911`) 는 동시에 **최대 1건**만 존재한다 → 순서 역전 불가.

### 3.7 `low`/`head` 전진과 `ViewSequences`

- `low` 는 `advanceLow()` 에서만 +1 (I4). `head` 는 리더의 `Propose` 에서 +1, `advanceLow` 에서 `max(head, low)`.
- `ViewSequences.Store` 는 (a) `NewProposer` (`util.go:313-316`, 값 = 시작 seq = low), (b) `advanceLow` (= 새 low), (c) `run` 종료 defer (`ViewActive:false`). 세 곳 모두 **low watermark** 를 발행하므로 W=1 과 값이 같다.
- 독자 3곳이 수정 없이 동작하는 이유:
  1. 리더 하트비트 `heartbeatmonitor.go:355-358` 는 `ProposalSeq` 를 `HeartBeat.Seq` 에 싣는다 → low_L.
  2. 팔로워 `handleHeartBeat :232-251` 은 `ourSeq+1 < hb.Seq → Sync`, `ourSeq+1 == hb.Seq → followerBehind`. 두 노드의 low 차이는 **전달 지연** 차이이고 파이프라인 깊이와 무관하다(각 seq 의 전달은 각자의 commit 정족수 시점). 또한 리더의 pre-prepare/prepare/commit 을 하트비트로 변환하는 `controller.go:318-320, 350-361` 은 `Seq = proposalSequence(m)`(= head 부근)를 싣지만 `handleHeartBeat :233` 의 `if active && !artificial` 가드로 **인공 하트비트는 seq 비교를 건너뛴다**. 따라서 head 가 비교에 쓰이는 일은 없다 (I10).
  3. `respondToStateTransferRequest controller.go:334-348` → `Sequence: low`; `sync() :664` 의 `response.Seq == latestDecisionSeq+1` 은 low 의 의미 그대로; `run :506-510` 의 재시작 seq 도 low → 새 뷰가 low 부터 다시 제안(in-flight 는 풀에 남아 재제안).

### 3.8 prev 링과 `handlePrevSeqMessage`

`view.go:749-787` 의 로직을 유지하되 `v.prevPrepareSent/prevCommitSent` → `slots[s].prepareSent/commitSent` (`s` 는 질의 seq, `slotDelivered` 또는 아직 열려 있는 슬롯). Assist 메시지 무시(`:764-766, 773-775`), 응답은 요청자에게 유니캐스트(`:768, 777`), 로그 `"Got %s for previous sequence (%d) from %d, %s"` 유지. pre-prepare 는 `"Got pre-prepare for sequence %d but we're in sequence %d"` Warn 후 무시(`:750-753`). 열린 슬롯(`[low, low+W)`)의 prepare/commit 을 못 받은 뒤처진 노드는 baseline 과 같이 자기 표를 1회 보내고(그에 대한 응답이 `handlePrevSeqMessage`) 그 외에는 하트비트 기반 sync 에 의존한다 — **baseline 에는 prepare/commit 의 주기 재전송이 없다** (§5.1 참조).

### 3.9 메시지 큐

`incMsgs` cap 은 `InMsgQSize`(=`IncomingMessageBufferSize`, 기본 200) 그대로. 슬롯당 메시지는 ≈3N 이므로 W=4, N=4 에서 동시 미처리 ≤ 48. 넘치면 `HandleMessage :194-198` 가 `Controller.ProcessMessages` 호출자를 블록시켜 역압이 걸린다(baseline 동일). 측정 뒤 필요하면 `IncomingMessageBufferSize` 권고값만 문서화(§13-6).

---

## 4. 팔로워 prepare 선행 조건 (D2)

**조건.** 노드는 슬롯 k 에 대해 Prepare 를 보내기(= `processProposal(k)` 실행) 전에 `[low, k-1]` 의 모든 seq 에 대해 pre-prepare 를 **수신하고 `verifyProposal` 을 통과**했어야 한다. `prepared(k-1)` 은 요구하지 않는다.

**구현.** §3.4 `doPhase` 의 `for s := low; s < low+W; s++` 루프가 `slot == nil || slot.prePrepare == nil` 에서 `break` 한다. 슬롯은 seq 순으로만 `slotEmpty → slotProposed` 로 넘어가므로 다음 불변식이 성립한다: **`phase ≥ slotProposed` 인 슬롯의 집합은 `[low, p)` 형태의 접두사다.** (귀납: k 가 처리되는 순간 `[low, k-1]` 은 이미 처리됨; pre-prepare 는 검증 후 취소되지 않음.) 따라서 "간극" 은 항상 접두사 끝 `p` 하나이고, `break` 뒤에도 `[low, p)` 의 prepare/commit 스텝은 같은 패스의 앞 반복에서 이미 수행되었다.

**순서 뒤바뀐 pre-prepare.** k+1 의 pre-prepare 가 k 보다 먼저 오면 `slots[k+1].prePrepare` 에 보관(수용 구간이면)하고 `break`. k 의 pre-prepare 가 도착(또는 §5 재전송)하면 다음 `doPhase` 패스에서 k, k+1 이 연속 처리된다 — WAL 기록·prepare 송신도 seq 순. 수용 구간 밖(`> low+W`)의 pre-prepare 는 보관하지 않는다(I2; baseline 의 `≥ S+2` 폐기와 같은 정책).

**검증은 slot.seq 기준.** `verifyProposal(seq, proposal, prevCommits)` (시그니처에 seq 추가) 가 `md.LatestSequence != seq`, `md.DecisionsInView != v.expectedDecisions(seq)` 를 검사한다(§6.7). 접두사 규칙 덕분에 k 를 검사할 때 `low` 는 아직 `≤ k` 이고 `expectedDecisions(k) = DecisionsInView + (k-low)` 가 결정적이다.

**sync 와의 상호작용.** 간극이 영구히 메워지지 않는 경우(W=1 이면 pre-prepare 1회 송신 유실): baseline 과 동일하게 (a) 리더 하트비트 `ourSeq+1 == hb.Seq` 가 `NumOfTicksBehindBeforeSyncing` 틱 지속 → `Sync()` (`heartbeatmonitor.go:393-403`), (b) 더 앞선 seq 의 commit f+1 → `discoverIfSyncNeeded` → `Sync()`. W>1 에서는 (c) 리더 재브로드캐스트(§5)가 먼저 메운다. 간극 뒤 슬롯들에 쌓인 표는 sync 로 뷰가 교체될 때 뷰와 함께 버려진다.

**W=1 환원.** 처리 구간이 `{low}` 이므로 조건은 공허하게 참(Lemma 5).

**안전성 기여 (viewchange-trace D2).** 어떤 노드가 slot k 에 prepare 했다면 그 노드는 `[low, k]` 의 pre-prepare 를 모두 가진다. 따라서 "s 의 prepare 는 유실됐는데 s+1 의 commit 인증서는 완성" 인 상황은 s 의 pre-prepare 자체가 유실된 노드들에게만 생기고, 그 노드들은 s+1 에 prepare 하지 않았으므로 s+1 인증서에 기여하지 않는다. 즉 접두사 밖의 인증서는 접두사 안의 pre-prepare 를 가진 노드들만으로 만들어진다 — 브랜치 6 의 접두사 복구 논증의 전제.

---

## 5. 리더 pre-prepare 재브로드캐스트

### 5.1 baseline 사실 확인 (정정)

view-slots 노트는 `doPhase :292/:295` 를 "주기적 재브로드캐스트" 라고 썼으나 **정확하지 않다.** `run` 은 `default: doPhase()` 로 돌지만, `doPhase` 의 PROPOSED/PREPARED 분기는 `BroadcastConsensus(lastBroadcastSent)` 뒤 곧바로 `processPrepares/prepared` 로 들어가 정족수까지 **블록**한다(`:476-491, :562-577`). 그 함수들은 PREPARED/COMMITTED/ABORT 를 돌려주므로 같은 phase 로 `doPhase` 가 재진입하는 경우는 **WAL 복원 직후 1회**뿐이다. 결론: baseline 은 pre-prepare 도, prepare 도, commit 도 **단계 진입 시 정확히 1회** 보낸다. 유실 복구는 `handlePrevSeqMessage`(반응형, 직전 seq 만)와 하트비트/`discoverIfSyncNeeded` 기반 sync 가 전부다. 이 사실이 §2.2-1 의 W=1 분기 근거다.

### 5.2 규칙

- 리더(`SelfID == LeaderID`)이고 `W > 1` 일 때만 `resendTick` 을 만든다. 틱마다 `resendPrePrepares()`:
  ```go
  func (v *View) resendPrePrepares() {
      for s := v.ProposalSequence; s < v.head; s++ {          // 열린 슬롯 [low, head)
          slot := v.slots[s]
          if slot == nil || slot.prePrepare == nil || slot.phase >= slotCommitted { continue }
          v.Comm.BroadcastConsensus(slot.prePrepare)
      }
  }
  ```
  `slotCommitted` 이상은 리더 자신이 정족수를 모았으므로 재전송하지 않는다(뒤처진 팔로워는 prev 링/sync 로).
- 팔로워 중복 처리: §3.3 `storePrePrepare` — 같은 내용이면 Debug 로 폐기, 다른 내용이면 Warn 으로 폐기 (I1: 슬롯은 최초 pre-prepare 하나만 수용; equivocation 은 기존처럼 첫 것이 이긴다).
- 대역폭 상한: 틱당 ≤ W × (pre-prepare 크기 ≤ `RequestBatchMaxBytes` + 헤더) × (N-1).
- `lastBroadcastSent` 와의 관계: 재전송 대상은 `slot.prePrepare`(리더가 자기에게 보낸 메시지 `view.go:1013` 그대로)이며 `lastBroadcastSent`(prepare/commit) 는 건드리지 않는다. 리더의 prepare/commit 재전송은 추가하지 않는다(baseline 과 같은 비주기성 유지; 유실 시 팔로워의 표에 대한 prev 응답과 sync 로 회복).
- **주기** (§13-1): `PrePrepareResendInterval` 를 `ProposalMaker` 신규 필드로 두고 `consensus.go proposalMaker()` (`:327-347`) 에서 `LeaderHeartbeatTimeout / LeaderHeartbeatCount` (하트비트 주기; `DefaultConfig` 6s, `fastConfig` 6s) 로 채운다. 설정 필드는 추가하지 않는다. 테스트는 뷰 단위 테스트에서 `resendTick` 채널을 직접 주입한다.

### 5.3 W=1 결정

**W=1 은 재브로드캐스트 없음(바이트 동일) 을 유지한다.** 이유: (i) baseline 에 주기 재전송이 전혀 없음이 §5.1 로 확인됨 — 켜면 메시지 집합이 바뀐다; (ii) W=1 에서 pre-prepare 유실의 결과(해당 노드가 sync 로 따라옴)는 baseline 의 알려진 동작이며 기존 테스트(`TestNodeOneSequenceBehindRejoins` 주석 `view_split_test.go:369-371` "the pre-prepare … is not sent again") 가 그 동작을 명시적으로 서술한다. "slot > low 만 재전송" 변형은 W=1 에서 공집합이므로 동치이지만, 조건식보다 `resendTick == nil` 이 의도를 더 분명히 드러낸다.

---

## 6. 컨트롤러

### 6.1 토큰 = cap W 세마포어

```go
// controller.go (신규/변경)
PipelineDepth uint64                              // Controller 필드. consensus.go:425-448 에서 c.Config.EffectivePipelineDepth()
c.leaderToken = make(chan struct{}, c.PipelineDepth)   // :802 (0 이면 1)

func (c *Controller) acquireLeaderTokens(n uint64) {   // :761-767 일반화. 비블로킹 send n회; 가득 차면 조용히 버림(cap 이 상한을 보장)
    for i := uint64(0); i < n; i++ { select { case c.leaderToken <- struct{}{}: default: return } }
}
func (c *Controller) drainLeaderTokens() {            // :769-774 일반화. 빌 때까지
    for { select { case <-c.leaderToken: default: return } }
}
```

**불변식 T (토큰).** 리더이고 현재 뷰가 실행 중이면 `len(leaderToken) + (head-low) + restoredInFlight == W`; 팔로워 또는 뷰 중단 상태면 `len(leaderToken) == 0` (abortView 뒤) 또는 proposer 가 폐기 중인 1개. `head-low` 는 `View.inFlightCount()` (신규, 테스트 전용 접근자) 로 읽는다.

### 6.2 `proposerLoop` 분리

```go
func (c *Controller) Start(...) {       // :796-828
    ...; c.controllerDone.Add(2)        // run + proposerLoop
    c.leaderToken = make(chan struct{}, c.depth()); c.decisionChan = make(chan decision, 1)   // decisionChan cap 유지 (§6.5)
    ...; c.startView(startProposalSequence)
    go func() { defer c.controllerDone.Done(); c.run() }()
    go func() { defer c.controllerDone.Done(); c.proposerLoop() }()
}

func (c *Controller) proposerLoop() {
    for { select { case <-c.stopChan: return; case <-c.leaderToken: }; c.propose() }
}

func (c *Controller) propose() {                                      // :463-475
    if c.stopped() || c.Batcher.Closed() { return }                   // :464-466 토큰 폐기 (R7-a, 현 동작)
    nextBatch := c.Batcher.NextBatch()                                // 블록해도 run 루프 무관
    if len(nextBatch) == 0 { c.acquireLeaderTokens(1); return }      // :468-471 (R5)
    view := c.currentView()                                           // currViewLock RLock 스냅샷 (현 :472,474 는 락 없이 run 전용)
    if view.Stopped() { return }                                      // R7-b: 토큰 폐기 — 새 뷰의 R1 이 채움
    infos := make([]types.RequestInfo, len(nextBatch)); for i, r := range nextBatch { infos[i] = c.RequestInspector.RequestID(r) }
    c.RequestPool.Reserve(infos)                                      // §7
    metadata := view.GetMetadata()                                    // head 기준 (§6.7)
    proposal := c.Assembler.AssembleProposal(metadata, nextBatch)
    view.Propose(proposal)                                            // head++ (뷰가 중단됐으면 HandleMessage :194-196 가 버림)
}
```

`run` (`:485-512`) 에서 `case <-c.leaderToken: c.propose()` 를 제거한다. 영향: baseline 은 `NextBatch()` (최대 `RequestBatchMaxInterval`) 동안 `decisionChan/viewChange/abortViewChan/syncChan` 처리가 멈췄다. 분리 후 즉시 처리된다. W=1 에서 그 창에 도착 가능한 결정은 교체/중단된 뷰의 지연 결정만이고(`:533-552` 경로, 토큰 변동 없음), abort/viewChange 는 `Batcher.Close()` 를 먼저 호출해 `NextBatch` 를 깨우므로(`:439, 448, 458`) 결과가 같다 — 타이밍만 빨라진다.

`Stop/StopWithPoolPause` (`:844-875`): `close()` → `Batcher.Close()` 로 proposerLoop 의 `NextBatch` 가 nil 로 깨어나 `acquireLeaderTokens(1)` 후 다음 반복에서 `stopChan` 으로 종료. drain 은 `controllerDone.Wait()` **뒤**로 옮겨 잔여 토큰이 남지 않게 한다(채널은 `Start` 가 재생성하므로 `:802` 기준 무해하지만 불변식 T 를 깨끗하게 유지).

### 6.3 R1–R7 (현재 코드 기준 재서술)

| 규칙 | 현재 | 변경 |
|---|---|---|
| **R1 발급(뷰 시작)** | `startView :373-379`: `leader && (initPhase==COMMITTED \|\| ABORT)` → 1개, 아니면 "Not acquiring…" | `restored := 0; if initPhase==PROPOSED \|\| PREPARED { restored = 1 }` (현 `Restore` 는 1개만 복원, `state.go`); `n := W - restored`; `n>0` → `Debugf("Acquiring leader token when starting view with phase %s")` + `acquireLeaderTokens(n)`, `n==0` → `Debugf("Not acquiring leader token when starting view with phase %s")` (`:375,:378` 문자열 유지, 테스트 훅 `basic_test.go:1426`). 먼저 `c.RequestPool.UnreserveAll()` (§7) |
| **R2 반환(정상 결정)** | `decide :566-568`: 리더면 1개 | `!rotated && iAmTheLeader` 이면 `acquireLeaderTokens(1)` |
| **R3 회수(뷰 중단)** | `abortView :427` `relinquishLeaderToken()` 1개, 그 뒤 `:431 Abort()` | `drainLeaderTokens()` — 순서(drain → Abort) 유지. in-flight 였던 제안은 토큰으로 되돌리지 않음(뷰와 소멸) |
| **R4 로테이션** | `:559-564` changeView(=R3+R1) 후 `:567` 재발급(cap 1 로 실패) | `rotated=true` 면 R2 생략 (§6.4) |
| **R5 빈 배치** | `:468-471` 토큰 되돌림 | 동일. W개가 모두 빈 배치면 각 `NextBatch` 가 `RequestBatchMaxInterval` 만큼 대기하므로 busy-loop 아님 |
| **R6 정지** | `Stop/StopWithPoolPause :851-855, :868-872` 1개 drain | drain 루프; proposerLoop 는 `stopChan` 종료 |
| **R7 세대 검사** | 없음(run 고루틴 순차성에 의존) | `view := c.currentView()` 스냅샷 + `view.Stopped()`. 뷰 포인터가 세대 역할. 포인터 재조회 금지(새 뷰에 옛 토큰으로 제안하면 in-flight W+1 가능) |

`changeView :396-400` early-return(뷰가 이미 같은 설정으로 실행 중)은 토큰을 건드리지 않는 현 동작 유지. 이 경로에서 `Batcher` 가 Closed 로 남아 토큰이 `:464` 에서 소멸하는 가능성(leader-token-flow 열린 문제 1)은 §13-5.

### 6.4 `decide` 수정

```go
func (c *Controller) decide(d decision) {                 // :516-569
    reconfig := c.Deliver.Deliver(d.proposal, d.signatures); if reconfig.InLatestDecision { c.close() }   // :518-521
    c.removeDeliveredFromPool(d)                           // :523 — 예약은 삭제로 소멸 (§7)
    c.InFlight.pruneDelivered(seqOf(d.proposal))           // (신규) §8.2 — 항목 1개면 no-op
    close(d.delivered); if c.stopped() { return }          // :524-527
    ... :529-552 그대로 (교체된 뷰 / 중단된 뷰: 토큰 변동 없음)
    md := unmarshal(d.proposal.Metadata)
    rotated := false
    if c.checkIfRotate(md.GetBlackList()) {                // :559 — W>1 에서는 Validate 로 항상 거짓
        c.changeView(c.getCurrentViewNumber(), md.GetLatestSequence()+1, c.getCurrentDecisionsInView()); c.RequestPool.RestartTimers(); rotated = true
    }
    c.MaybePruneRevokedRequests()
    if !rotated { if iAm, _ := c.iAmTheLeader(); iAm { c.acquireLeaderTokens(1) } }   // R2/R4
}
```

W=1 상태 동일성: 로테이션 경로에서 `changeView → startView` 가 새 리더에게 1개를 채운 뒤 baseline 의 `:567` 은 가득 찬 채널에 실패한다. 생략해도 채널 상태는 같다. W>1 에서도 `startView` 가 W 를 채우므로 추가 발급은 실패하지만, `restored>0` 로 `W-restored` 만 채우는 경우(재시작 직후 첫 뷰; 로테이션 ON 은 W>1 에서 불가라 현재는 도달 불가)에 대비한 위생 수정이다.

### 6.5 `Decide`/`decisionChan` 순서 — 블로킹 유지

`Controller.Decide :887-920` 과 `decisionChan` cap 1 (`:803`) 을 **그대로 둔다.** 근거:
1. in-order 게이트는 View 에 있다(§3.6). 컨트롤러는 "뷰가 넘긴 순서대로 1건씩" 만 보장하면 되고, 블로킹이 그것을 가장 단순하게 보장한다: 뷰 고루틴이 `delivered` 또는 abort 를 기다리므로 `decisionChan` 에 동시에 1건만 들어간다. abort 경로 `go enqueueDecision(d)` (`:911`) 도 §3.6 의 `Stopped()` 검사로 동시에 1건만 → 순서 역전 불가.
2. `d.view != currView` (`:533`), `d.view.Stopped()` (`:543`), reconfig `c.close()` (`:519-521`) 의 의미가 그대로 유지된다.
3. 비용: 전달 중 뷰가 다른 슬롯의 표를 처리하지 못한다. 그러나 (a) 메시지는 `incMsgs` 에 보존되고, (b) 파이프라이닝의 이득은 네트워크 왕복의 중첩에서 오며 전달 시간은 baseline 에서도 seq 당 1회 직렬이다. 처리량 상한이 `1/Deliver 시간` 으로 바뀌지 않는다(baseline 은 `1/(RTT 왕복 + Deliver)`).
4. 대안(비동기 전달 큐, `decisionChan` cap W, `delivered` 대기 제거)은 `MutuallyExclusiveDeliver :989-1000` 의 "이미 커밋됨 → sync 로 대체" 경로와 `enqueueDecision` 의 FIFO 를 다시 설계해야 한다. 벤치에서 `view_latency_batch_save`(= `Application.Deliver` 시간, `:1002-1006`) 가 라운드 지연에 비해 유의미하면 후속으로. §13-3.

### 6.6 `maybePruneInFlight` → `clearUpTo`

```go
func (c *Controller) maybePruneInFlight(syncResultViewMD *protos.ViewMetadata) {   // :695-718
    synced := syncResultViewMD.GetLatestSequence()
    lowest := c.InFlight.InFlightProposal()               // 최저 항목 (§8)
    if lowest == nil { Debugf("No in-flight proposal to prune"); return }
    if synced < seqOf(lowest) { Infof("In-flight sequence is %d but latest committed sequence is %d, will not delete in-flight"); return }   // :709-712 (항목별로는 아래가 처리)
    Infof("Synced to sequence %d, deleting in-flight as it is stale", synced)
    c.InFlight.clearUpTo(synced)                           // seq ≤ synced 만 삭제 (현 clear() :717 은 전부)
}
```

### 6.7 `GetMetadata` 와 `DecisionsInView` 예측 — 정확성 핵심

**현재 코드가 검사하는 것.** `verifyProposal :603-611`:
```go
if md.GetLatestSequence() != v.ProposalSequence { ... "invalid proposal sequence" }
if md.GetDecisionsInView() != v.DecisionsInView  { ... "invalid decisions in view" }
```
리더는 `GetMetadata :936-940` 에서 `LatestSequence: v.ProposalSequence, DecisionsInView: v.DecisionsInView` 를 싣는다. 두 값은 `startNextSeq :893-894` 에서 **항상 함께 +1** 되며 그 외에는 뷰 생성 시 `NewProposer(.., proposalSequence, .., decisionsInView, ..)` (`util.go:286, 304-305, 325-333`) 와 `Restore` (`state.go:172,177,229,234`: 복원 제안의 메타데이터에서 읽음) 만 설정한다.

**따름정리.** 한 뷰 인스턴스 안에서 `DecisionsInView − ProposalSequence` 는 상수다. baseline 의 정확 일치 검사가 통과한다는 것은 "리더와 팔로워가 같은 상수를 가진다" 와 동치이다 (seq 가 같고 두 값이 lockstep 이므로). 따라서

```go
func (v *View) expectedDecisions(seq uint64) uint64 { return v.DecisionsInView + (seq - v.ProposalSequence) }   // seq ≥ low 보장(라우팅)
```

는 **새 가정을 추가하지 않는다**: baseline 이 seq `low` 에서 요구하는 일치가 성립하면 모든 `k ≥ low` 에서도 성립한다. 뷰 시작값의 일치는 baseline 이 이미 의존하는 것이다 — 뷰 체인지 `changeView(newView, newSeq, 0)` (`controller.go:491`), sync `newProposalSequence = latestDecisionSeq+1, newDecisionsInView = latestDecisionDecisions+1` (`:644-645`) 또는 `(response.View, 0)` (`:678-679`), 시작 `setViewAndSeq` (`consensus.go:471-511`) 모두 노드 간에 같은 결정론적 입력에서 나온다.

**리더.**
```go
func (v *View) GetMetadata() []byte {                     // :935-963
    v.seqLock.Lock(); seq := v.head; dec := v.expectedDecisions(v.head); v.seqLock.Unlock()
    metadata := &protos.ViewMetadata{ViewId: v.Number, LatestSequence: seq, DecisionsInView: dec}
    v.Logger.Debugf("GetMetadata with view %d, seq %d, dec %d", ...)      // :942 (테스트 훅 basic_test.go:1423)
    ... :944-962 그대로 (RetrieveCheckpoint, 블랙리스트, 서명 다이제스트 — W>1 에서는 DecisionsPerLeader==0 으로 빈 값)
}
func (v *View) Propose(proposal types.Proposal) {          // :989-1015
    ... prevSigs (:991-993) 그대로
    v.seqLock.Lock(); seq := v.head; v.head++; v.seqLock.Unlock()      // :995 대체
    msg := PrePrepare{View: v.Number, Seq: seq, Proposal: ..., PrevCommitSignatures: prevSigs}
    v.HandleMessage(v.LeaderID, msg); v.Logger.Debugf("Proposing proposal sequence %d in view %d", seq, v.Number)
}
```
`GetMetadata` 와 `Propose` 는 proposerLoop 단일 고루틴에서 연속 호출되므로 둘 사이에 `head` 가 바뀌지 않는다. `GetMetadata` 는 부작용이 없다(baseline 과 같고, 빈 배치일 때 호출되지 않는 구조 `:468-472` 도 그대로).

**팔로워.** `verifyProposal(seq, ...)`: `md.LatestSequence != seq` → `"Expected proposal sequence %d but got %d"` (`:604`, 기대값 = seq); `md.DecisionsInView != v.expectedDecisions(seq)` → `"Expected decisions in view %d but got %d"` (`:609`). 리더의 `expectedDecisions(head)` 와 팔로워의 `expectedDecisions(k)` 는 `k == head` 인 pre-prepare 에서 같은 식이므로 일치한다.

**컨트롤러 측 `currDecisionsInView`** (`:191-203`, `decide :541` 에서 +1) 는 리더 선출(로테이션 OFF 에서는 무시: `getLeaderID util.go:86-88`)·`changeView :396-397` 의 동일성 검사·`startView :364` 의 `NewProposer` 인자에 쓰인다. 전달이 in-order 이므로 뷰의 `DecisionsInView`(전달 직전 +1)와 컨트롤러 값(전달 직후 +1)의 관계는 baseline 과 같다(일시적 1 차이).

**복원 리더 검증.** `TestLeaderProposeAfterRestartWithoutSync` (`basic_test.go:1405-1435`) 는 seq 1 을 PREPARED 로 복원한 리더가 seq 1 전달 뒤 **seq 2** 를 제안해야 하며 `"Expected proposal sequence 2 but got 1"` 이 나오면 패닉한다. 설계: `Start()` 가 `head = low+1 = 2`, R1 이 `W-1 = 0` 개 발급 → 전달 후 R2 로 1개 → `GetMetadata` 가 `head=2`. ✓

### 6.8 `startView` 와 `UnreserveAll`

```go
func (c *Controller) startView(proposalSequence uint64) {       // :363-384
    view, initPhase := c.ProposerBuilder.NewProposer(...)
    c.currViewLock.Lock(); c.currView = view; c.currView.Start(); c.currViewLock.Unlock()
    c.RequestPool.UnreserveAll()                                  // (신규) 예약은 뷰 인스턴스에 속함 (§7.2)
    role := Follower
    if leader, _ := c.iAmTheLeader(); leader { /* R1 */ role = Leader }
    c.LeaderMonitor.ChangeRole(role, c.currViewNumber, c.leaderID())
    c.Logger.Infof("Starting view with number %d, sequence %d, and decisions %d", ...)   // :383 (테스트 훅)
}
```

---

## 7. 요청 풀 예약

### 7.1 상태와 API

`NextRequests` 는 비파괴적이고(`requestpool.go:293-331`, 테스트 `requestpool_test.go:128-164` 가 같은 요청의 반복 반환에 의존) 제거는 `removeDeliveredFromPool` (`controller.go:935-941`) 뿐이므로, W>1 에서 결정 전에 다시 뽑으면 같은 요청이 재제안된다. 예약 상태를 `requestItem` 에 둔다 — 별도 인덱스 없음.

```go
type requestItem struct { request []byte; timeout *time.Timer; additionTimestamp time.Time; reserved bool }   // :71-75 + reserved
type Pool struct { ...; reservedCount int; reservedBytes uint64 }                                           // :50-68 + 2 카운터

// RequestPool 인터페이스 (controller.go:38-47) 에 추가 → internal/bft/mocks/request_pool.go 재생성 (mockery, .mockery.yml)
func (rp *Pool) Reserve(infos []types.RequestInfo)   // 각 info 에 대해 existMap 조회(:60) → item.reserved=true, 카운터 +. 없거나 이미 예약이면 무시
func (rp *Pool) UnreserveAll()                       // existMap 순회, reserved=false, 카운터 0. O(n), 뷰 시작 시에만

func (rp *Pool) NextRequests(maxCount int, maxSizeBytes uint64, check bool) ([][]byte, bool) {   // :296-331
    if check && (len(rp.existMap)-rp.reservedCount < maxCount) && (rp.sizeBytes-rp.reservedBytes < maxSizeBytes) { return nil, false }   // :300-304 를 미예약 기준으로
    // :306-321 순회에서 item.reserved 인 요소는 건너뜀 (count 는 미예약 수 기준)
}
func (rp *Pool) deleteRequest(...) { if item.reserved { reservedCount--; reservedBytes -= len } ... }   // :391-406. RemoveRequest/Prune/Close/AutoRemove 모두 여기로
```

### 7.2 수명주기

- **예약**: `Controller.propose` 가 `NextBatch()` 반환 직후, `Propose` 전에 (§6.2). 키는 `RequestInspector.RequestID(req)` — `removeDeliveredFromPool` 이 쓰는 `d.requests` (`Verifier.VerifyProposal` 결과) 와 같은 식별자 공간이다(하네스: `test_app.go:213-222` 와 `RequestID` 가 같은 `(ClientID, ID)`).
- **해제(전달)**: `decide → removeDeliveredFromPool → RemoveRequest → deleteRequest` 가 항목을 삭제하며 카운터를 조정한다. 별도 Unreserve 불필요.
- **해제(뷰 교체)**: `startView` 의 `UnreserveAll()` (§6.8). 뷰 중단과 다음 뷰 시작 사이에는 제안자가 없으므로 그 사이의 예약 잔존은 관찰되지 않는다. 재시작은 풀이 새로 만들어지고(`consensus.go:150`), reconfig 는 풀을 유지하지만(`:229 ChangeOptions`) 새 컨트롤러의 `Start → startView` 가 해제한다.
- **예약된 제안이 폐기된 경우** (`Propose` 가 중단된 뷰에 들어가 `HandleMessage :194-196` 에서 버려짐): 다음 `startView` 까지 예약 유지 → 그 사이 제안자 없음 → 무해.

### 7.3 W=1 불변 증명

주장: W=1 에서 `NextRequests` 가 호출되는 모든 시점에 `reservedCount == 0`. 토큰 1개는 (a) `startView` 직후(`UnreserveAll` 뒤), (b) `decide` 의 R2 (직전 제안의 요청이 `removeDeliveredFromPool` 로 삭제된 뒤; 제안에 들어갔으나 그 전에 `Prune`/`onAutoRemoveTO :559-567` 로 제거된 요청도 삭제로 해제됨), (c) R5 빈 배치(예약 없음) 에서만 발급된다. 어느 경우에도 예약 집합은 비어 있다. 따라서 `check` 와 순회의 결과가 baseline 과 같다. ∎

### 7.4 타임아웃·`Size`·메모리

- 예약된 요청의 타이머는 그대로 돈다(Submit 시각 기준 3단 체인 `:248-251, 493-567`). Forward 만료 → 팔로워가 리더로 전달 → 리더 `Submit` 은 `ErrReqAlreadyExists` (`:211-214`). Complain 만료 → 뷰 체인지 유발 가능 — W×MaxCount 백로그가 꼬리 체류 시간을 늘리므로 벤치 설정에서 `RequestComplainTimeout` 을 확인한다(batcher 노트 임계값 분석 그대로).
- **`Size()` 는 예약 포함 유지** (`:286-291`, `len(existMap)`). 세마포어(`QueueSize`)도 예약 건을 점유하므로 "풀에 있는 요청 수" 의 의미가 일관되고 W=1 과 같다. `W×RequestBatchMaxCount ≤ RequestPoolSize` (`config.go:215-217`) 가 바로 이 점유를 위한 제약이다.
- 메모리: 요청당 1 bool + 풀당 카운터 2개.

---

## 8. `InFlightData` 다중화

### 8.1 구조

```go
type InFlightData struct { lock sync.RWMutex; entries map[uint64]*inFlightProposalData }   // util.go:184-192: v 포인터 1개 → seq 키 맵

func (ifp *InFlightData) StoreProposal(prop types.Proposal)        // :232-239. 키 = ViewMetadata(prop.Metadata).LatestSequence; 언마셜 실패 시 키 0 (util_test.go:122-135 는 Metadata []byte{1} 로 저장 — 운영 경로는 verifyProposal 통과 후라 실패 불가)
func (ifp *InFlightData) StorePrepares(view, seq uint64)           // :242-253. entries[seq].prepared = true. 없으면 panic (현 nil 패닉 :245 와 같은 정책)
func (ifp *InFlightData) InFlightProposal() *types.Proposal         // :195-204. 최저 seq 항목
func (ifp *InFlightData) IsInFlightPrepared() bool                  // :207-216. 최저 항목
func (ifp *InFlightData) InFlightProposalAndPrepared() (*types.Proposal, bool)   // :220-229. 최저 항목 (viewchanger.go:469 호출자 변경 없음)
func (ifp *InFlightData) clear()                                    // :255-260. 전부
func (ifp *InFlightData) clearUpTo(seq uint64)                      // (신규) seq 이하 삭제 — maybePruneInFlight
func (ifp *InFlightData) pruneDelivered(seq uint64)                 // (신규) seq 이하 삭제, 단 **최고 항목은 남긴다** (len>1 인 동안만 삭제) — controller.decide
```

### 8.2 `pruneDelivered` 의 "최고 항목 보존" 규칙 — W=1 바이트 동일의 근거

baseline 은 전달 후 `InFlightData` 를 비우지 않는다. 그 결과 `prepareViewDataMsg :467-477` 는 `inFlightProposal, prepared := InFlightProposalAndPrepared()` 에서 전달 완료된 항목을 받아 `getInFlight :521-524` 로 `in_flight_proposal = nil` 을 만들지만 **`InFlightPrepared: prepared` 는 그 항목의 값(true)을 그대로 싣는다.** 즉 W=1 의 ViewData 바이트에는 "proposal 없음 + prepared=true" 가 들어간다(`CheckInFlight :857-860` 은 proposal nil 이면 prepared 를 보지 않으므로 프로토콜상 무의미하지만 바이트는 다르다). 전달 시 무조건 삭제하면 이 바이트가 달라진다. 반면 W>1 에서 전달된 k 를 남기면 최저 항목이 k 가 되어 실제 in-flight 인 k+1 을 가린다. 두 요구를 동시에 만족하는 규칙이 "k 이하를 지우되 맵이 비게 되면(= k 가 유일·최고) 남긴다" 이다: W=1 에서는 항상 항목 1개 → no-op(바이트 동일); W>1 에서는 `{k, k+1, …}` → `{k+1, …}`, 파이프라인이 비어 `{k}` 만 남으면 유지(W=1 과 같은 상황).

### 8.3 `prepareViewDataMsg` 가 싣는 것 (이 브랜치)

**최저 seq 항목.** 근거: 뷰 체인지 복구 프로토콜은 `expectedSequence = maxLastDecisionSequence+1` (`viewchanger.go:852`) 하나만 판정하고, `ValidateInFlight :774` 는 발신자의 `lastDecision+1` 과 일치해야 수용하며, `commitInFlightProposal :1277-1279` 는 `lastDecision+1` 이 아니면 Panicf 한다. 최저 미전달 seq = 체크포인트+1 이 유일하게 쓸모 있는 값이다. "최고 prepared" 는 검증에서 거부되거나 패닉을 일으킨다. §8.2 와 `getInFlight :521-530` 덕분에 전달 완료된 항목이 최저에 남아도 nil 로 정규화된다.

### 8.4 D1 가정 하의 임시 안전성

브랜치 6 이전에 뷰 체인지가 W>1 in-flight 를 만나면: (1) 접두사 길이 ≤ 1 (expectedSequence 하나). (2) 그 위 seq 의 슬롯 상태(표·commit 인증서·보류 서명)는 `abortView → Abort()` 로 뷰 객체와 함께 소멸한다 — D1 의 "슬롯 상태 파기" 부분은 자동으로 성립. (3) `InFlightData` 의 상위 항목은 남는다. 이 항목이 다시 보고되는 경로는 "그 seq 가 최저가 된 뒤의 두 번째 뷰 체인지" 이고, 그 전에 새 뷰의 같은 seq 제안이 오면 `StoreProposal` 이 seq 키로 덮어쓴다. baseline 도 단일 항목에 대해 똑같은 노출을 가진다(뷰 체인지 뒤 새 제안이 오기 전 옛 in-flight 가 그대로 보고됨). 즉 **노출의 종류는 같고 seq 수만 늘어난다.** 완전한 D1(`clearAbove(base+m)` 를 `processNewViewMsg` 접두사 커밋 직후에) 은 브랜치 6. (4) Deliver 기준 안전성(§1.2 표의 논증)은 (3) 과 무관하게 성립한다: 어떤 노드도 `MutuallyExclusiveDeliver :989-1000` 와 in-order 게이트를 지나지 않고는 전달하지 않는다.

---

## 9. 불변식 목록

| # | 불변식 | 유지하는 코드 지점 | 검사하는 테스트 (§11) |
|---|---|---|---|
| I1 슬롯 유일성 | (view, seq) 당 `seqState` 1개, pre-prepare 최초 1개 | `v.slot(s)` 맵, `storePrePrepare` nil 검사 | `TestTwoPrePreparesInARow` (기존), `TestViewSlotRouting` |
| I2 윈도우 경계 | 리더 `head-low ≤ W`(토큰), 팔로워 `s > low+W` 비수용, 처리 `< low+W` | `acquireLeaderTokens`, `processMsg`, `doPhase` 루프 상한 | `TestControllerTokenSemaphore`, `TestPipelineLeaderStopsAtWindow`, `TestViewSlotRouting` |
| I3 순서 전달 | `Decide(k)` 는 `k==low ∧ slotCommitted` 일 때만, 호출 후 low=k+1 | `tryDeliver` | `TestViewInOrderDeliver`, `TestPipelineDecideInOrder` |
| I4 단일 전진 | `ProposalSequence/DecisionsInView/ViewSequences.ProposalSeq` 는 `advanceLow` 에서만 +1 | `advanceLow` (유일한 쓰기) | `TestViewInOrderDeliver` (Store 값 검사) |
| I5 메타데이터 결정성 | 슬롯 k: `LatestSequence==k`, `DecisionsInView==expectedDecisions(k)` | `GetMetadata`, `verifyProposal(seq,…)` | `TestViewGetMetadataPredictsDecisions` |
| I6 자기 표 1회 | `slot.mySig` 는 그 슬롯의 `stepPrepares` 가 쓰고 `tryDeliver` 가 그 슬롯에만 합산 | `seqState.mySig` | `TestViewInOrderDeliver` (서명 수 == Quorum) |
| I7 WAL 보존 | (브랜치 5) 이 브랜치: `Save(pp, true)` 유지 → W>1 재시작 미지원 | — | 없음 (한계 문서화) |
| I8 재전송 가용성 | `[low-W, low)` 의 `prepareSent/commitSent` 보존, Assist 무응답 | `slotDelivered.release()`, `gcSlots`, `handlePrevSeqMessage` | `TestViewLaggingCatchup` (기존), `TestViewSlotRouting` prev 케이스 |
| I9 sync 임계 | 같은 뷰 `seq > low+W` commit f+1 → sync | `processMsg` 미래 분기 → `discoverIfSyncNeeded` (변경 없음) | `TestDiscoverDeliberateCensorship` (기존) |
| I10 하트비트 격차 | 하트비트/StateTransfer Seq = low; `!artificial` 가드 | `advanceLow` 의 Store, `heartbeatmonitor.go:233` | `TestNodeOneSequenceBehindRejoins` (기존), `TestMultiInFlightBasic` (sync 로그 0건) |
| I11 뷰체인지 in-flight | ViewData 는 최저 항목 | `InFlightProposalAndPrepared` | `TestInFlightDataMultiplex` |
| I12 D2 접두사 | `phase ≥ slotProposed` 집합은 `[low,p)` | `doPhase` 의 `break` | `TestViewFollowerPreparePrecondition` |
| T 토큰 | 리더·실행 중: `len(token)+(head-low)+restored == W`; 아니면 0 | R1–R7 | `TestControllerTokenSemaphore`, `TestControllerDecideRotationNoDoubleToken` |
| P 풀 | 예약 ⊆ existMap; `reservedCount/Bytes` 일치; W=1 에서 `NextRequests` 호출 시 예약 0 | `Reserve/UnreserveAll/deleteRequest` | `TestReqPoolReserve`, 기존 `TestReqPoolBasic` |
| F in-flight 맵 | 전달 후 최저 항목 > 체크포인트 (W>1) 또는 유일 항목 (W=1) | `pruneDelivered` | `TestInFlightDataMultiplex` |

---

## 10. 안전성 논증 스케치 (뷰 내부)

뷰 체인지가 없는 한 뷰 v 안에서, N ≥ 3f+1, 정족수 Q (`computeQuorum util.go:176-180`).

1. **seq 별 합의 (불변).** 슬롯 k 의 prepare 정족수(`Quorum-1` + 자기 표)와 commit 정족수는 baseline 과 같은 `voteSet`(발신자당 1표 `util.go:128-134`)과 같은 다이제스트 검사(`stepPrepares` 의 `:484-488`, `verifyVote :858-863`)로 모인다. 두 정족수의 교집합 논증은 seq 별로 독립이며 슬롯화는 표를 다른 seq 와 섞지 않는다(I1). 리더가 같은 k 에 다른 두 제안을 보내도 슬롯은 첫 pre-prepare 만 받고(I1) 다른 노드와 다이제스트가 어긋나면 prepare 정족수가 서지 않는다 — baseline 의 equivocation 처리와 동일(`TestLeaderModifiesPreprepare` "conflicting proposals").
2. **순서 전달.** `Decider.Decide` 는 `tryDeliver` 에서만, `k == low` 일 때만 호출되고 호출 전 `low++` (I3). 따라서 한 노드의 `Deliver` 호출 열은 seq 가 1씩 증가한다. `MutuallyExclusiveDeliver :989-1000` 의 `latest >= pending` 검사는 정상 경로에서 발화하지 않고(pending = latest+1), 뷰 체인지/sync 경쟁에서만 작동한다 — baseline 과 같은 역할.
3. **중복 전달 없음.** 슬롯은 `slotDelivered` 로 전이한 뒤 `release()` 되어 다시 committed 가 될 수 없고, `tryDeliver` 는 `slots[low]` 만 본다. 뷰 체인지 경로의 중복은 `MutuallyExclusiveDeliver` 가 체크포인트 비교로 막는다(기존 `TestTryCommittedSequenceTwice`). 하네스 `App.Deliver :322-324` 는 같은 seq 연속 전달에 패닉한다.
4. **요청 중복 제안 없음 (리더).** in-flight 제안의 요청은 `Reserve` 로 표시되어 `NextRequests` 가 건너뛴다(P). 해제는 전달(삭제) 또는 새 뷰 시작에서만 일어나므로, 한 뷰 인스턴스 안에서 같은 요청이 두 제안에 들어갈 수 없다. 뷰 교체 뒤의 재제안은 baseline 과 같은 의미(전달되지 않은 제안의 요청은 풀에 남아 재제안됨).
5. **한 노드 안의 두 리더 없음.** 토큰은 뷰 인스턴스에 귀속되며(R3 drain → R1 발급), proposer 는 뷰 포인터 스냅샷에만 제안한다(R7). 따라서 새 뷰에 옛 토큰으로 제안해 `head-low > W` 가 되는 일이 없다(T).

**뷰 간 안전성**은 브랜치 6 으로 미룬다. 임시 가정: (a) `LeaderRotation=false`; (b) 뷰 체인지는 기존 단일 in-flight 복구(`CheckInFlight`, `commitInFlightProposal`) 로 `maxLastDecision+1` 만 복구하고 그 위는 폐기; (c) 폐기된 seq 의 슬롯 상태는 뷰 객체와 함께 소멸; (d) Deliver 기준 정의에 따라 폐기는 안전(§1.2, §8.4). (e) 재시작 중 W>1 in-flight 는 범위 밖(§1.2).

---

## 11. 테스트 계획

### 11.1 단위 (`internal/bft`)

| 테스트 | 내용 | 어서션 |
|---|---|---|
| `TestViewSlotRouting` (table-driven) | W∈{1,2,4}, low=L. seq ∈ {L-W-1, L-W, L-1, L, L+W-1, L+W, L+W+1} 의 prepare/commit/pre-prepare 를 `processMsg` 에 넣음 | 과거/미래 → 슬롯 미생성 + Info 로그; prev → `SendConsensus` 유니캐스트(`prepareSent` 있을 때); 수용 → 슬롯 생성·표 등록; W=1 분할이 baseline `{S-1},{S,S+1}` 와 일치 |
| `TestViewInOrderDeliver` | W=3 팔로워. seq L+1 의 pre-prepare·prepare·commit 정족수를 L 보다 먼저 완성 | `Decider.Decide` 가 L, L+1 순으로 호출; `ViewSequences.ProposalSeq` 가 L→L+1→L+2; 각 Decide 의 서명 수 == Quorum; `view_in_flight_proposals` 가 0 으로 복귀 |
| `TestViewFollowerPreparePrecondition` | W=3. L+1 의 pre-prepare 를 먼저 보냄 | L 의 pre-prepare 전에는 `BroadcastConsensus` 호출 0(상대 prepare 없음); L 도착 후 prepare(L), prepare(L+1) 순 송신; WAL `Save` 순서 L, L+1 |
| `TestViewLeaderResendsPrePrepares` | W=2 리더, `resendTick` 주입; W=1 리더 | W=2: 틱마다 열린 슬롯 pre-prepare 재송신, committed 슬롯 제외; W=1: `resendTick==nil`, 추가 송신 0 |
| `TestViewDuplicatePrePrepareLogLevel` | W=2 에서 같은 pre-prepare 2회, 다른 내용 2회 | 같은 내용 → Warn 없음; 다른 내용 → 기존 Warn 문자열 |
| `TestViewGetMetadataPredictsDecisions` | 리더 뷰 `low=5, DecisionsInView=2, head=7` | `GetMetadata` → `(7, 4)`; 팔로워 뷰(`low=5, dec=2`) 의 `verifyProposal(7, …)` 통과, `(7,3)` 거부 문자열 `"Expected decisions in view 4 but got 3"` |
| `TestControllerTokenSemaphore` | W=3 컨트롤러 fixture(`controller_decide_test.go:98-117` 를 `PipelineDepth` 로 확장) | `startView`(COMMITTED) → `len==3`; PREPARED → 2; `decide` 정상 → +1 (cap); `abortView` → 0; `Stop` → 0 |
| `TestControllerDecideRotationNoDoubleToken` | 로테이션 분기 강제 | `changeView` 뒤 `len == W`, 추가 발급 없음 |
| `TestProposerLoopDiscardsTokenOnStoppedView` | 토큰 발급 후 뷰 `stop()` | `Propose` 미호출, 토큰 소비됨, 배치 요청은 풀에 잔존(예약은 `startView` 가 해제) |
| `TestReqPoolReserve` | 12건 Submit, `Reserve(앞 10건)` | `NextRequests(10, _, true)` → nil(미예약 2 < 10); `check=false` → 2건; `RemoveRequest` 로 카운터 감소; `UnreserveAll` 후 10건 다시 반환; `Size()==12` 내내 |
| `TestInFlightDataMultiplex` | `StoreProposal(k), (k+1)`, `StorePrepares(_, k+1)` | `IsInFlightPrepared()==false`(최저 k 는 미prepared); `pruneDelivered(k)` → 최저 k+1, prepared true; `{k}` 단독에서 `pruneDelivered(k)` → 유지; `clearUpTo(k+1)` → 빈 맵 |
| 기존 유지 | `TestTwoPrePreparesInARow`, `TestViewLaggingCatchup`, `TestViewPersisted`, `TestControllerDecideViewBookkeeping`(fixture 에 `PipelineDepth:1`), `TestDecideDoesNotDropDecisionOnAbortedView`, `requestpool_test.go` 전부, `util_test.go:122-135` | 변경 없이 통과 |

### 11.2 통합 (`test/`)

공통 설정: `newNodeWithConfig` (`test_app.go:460`) 로 `fastConfig` 에서 시작, `LeaderRotation=false, DecisionsPerLeader=0, PipelineDepthMax=W, RequestBatchMaxCount=1` (요청 1건 = 제안 1건), `RequestPoolSize ≥ W×1`. 로그 훅은 `zap.Hooks` + `waitForSignal`(30s 상한) 패턴.

| 테스트 | 시나리오 | 어서션 |
|---|---|---|
| `TestMultiInFlightBasic` | W=4, 4노드. `holdCommits` (`view_split_test.go:339`) 로 팔로워 commit 을 잡은 채 요청 4건 제출 → 리더가 seq 1..4 를 연속 제안 → release → 요청 4건 더 | 모든 팔로워에서 `"got message from %d with sequence %d but our sequence is %d"` 0건, `"dropping message"` 0건(= pre-prepare 손실 0, probe 의 `W-2` 손실 해소); 각 노드 `Delivered` 의 `LatestSequence` 가 1..8 단조·전 노드 바이트 동일; `"Heartbeat sequence is bigger than expected"` 0건(I10) |
| `TestPipelineDecideInOrder` | W=3. 팔로워 2,3 의 `MutateSend` 로 seq 1 commit 송신을 seq 2,3 commit 송신 뒤까지 블록 | 전 노드 Delivered 순서 1,2,3; 하네스 `"Committed sequence %d twice"` 패닉 없음; 전 노드 레코드 동일 |
| `TestPipelineLeaderStopsAtWindow` | W=3. `holdCommits` 전원 → 요청 5건 | 리더 로그 `"Proposing proposal sequence %d in view %d"` 가 release 전 1..3 만(4 는 `time.After(1s)` 로 부정 확인); release 후 4,5; 전 과정에서 `#Proposing − #"Deciding on seq"` ≤ 3 |
| `TestPipelineDepthOneEqualsBaseline` | 위와 같은 하네스, W=1 | `"Proposing proposal sequence 2"` 가 seq 1 의 `"Deciding on seq 1"` 전에 미발생 |
| W=1 회귀 | `go test ./internal/... ./pkg/... ./test` 전체 (기존 52개 통합 테스트 포함) | 전부 통과 |

### 11.3 테스트가 하지 않는 것 (한계)

W>1 in-flight 상태의 `Restart()`(브랜치 5), W>1 in-flight 중 뷰 체인지 결과의 라이브니스(브랜치 6), W>1 중 reconfig(브랜치 7).

### 11.4 보존해야 하는 로그 문자열

test-harness 노트의 15개(이 체크아웃 위치): `"%d collected %d prepares from %v"` view.go:493 · `"Processed proposal with seq %d"` :455 · `"Processed prepares for proposal with seq %d"` :546 · `"%d processed commits for proposal with seq %d"` :342 · `"Sequence: %d-->%d"` :909 · `"Starting view with number %d, sequence %d, and decisions %d"` controller.go:383 · `"ViewChanged, the new view is %d"` :455 · `"Aborting current view with number %d"` :430 · `"Heartbeat timeout expired, complaining about leader: %d"` :304 · `"get msg from syncChan"` :499 · `"Restored proposal with sequence %d"` state.go:178,244 · `"In-flight view %d with latest sequence %d has committed a decision"` viewchanger.go:1375 · `"Timeout expired waiting on In-flight %d with latest sequence view to commit %d"` :1383 · `"Node %d was unable to commit the in flight proposal, not changing the view"` :1200 · `"LogRecord appended successfully"` pkg/wal. 추가로 이 설계가 확인한 훅 4개: `"Not acquiring leader token when starting view with phase %s"` controller.go:378 · `"GetMetadata with view %d, seq %d, dec %d"` view.go:942 · `"Expected proposal sequence %d but got %d"` :604 (패닉 트리거) · `"Got a pre-prepare for %s sequence without processing previous one, dropping message"` :329. 신규 테스트가 쓰는 `"Proposing proposal sequence %d in view %d"` :1014 와 `"Deciding on seq %d"` :883 도 고정한다.

---

## 12. 구현 순서 (커밋 단위, 각 커밋 빌드·W=1 green)

| # | 커밋 | 내용 | 크기(추정) |
|---|---|---|---|
| a | `pool: reservation state for in-flight requests` | `requestItem.reserved`, 카운터, `Reserve/UnreserveAll`, `NextRequests` 건너뜀, `RequestPool` 인터페이스 + mockery 재생성, `TestReqPoolReserve`. 호출자 없음 → 동작 불변 | ~150 LOC |
| b | `inflight: key InFlightData by sequence` | 맵 다중화, `clearUpTo/pruneDelivered`, `StorePrepares` 가 seq 사용, `maybePruneInFlight` 교체, `decide` 에 `pruneDelivered`, `TestInFlightDataMultiplex`. `util_test/state_test/viewchanger_test` 통과 | ~120 LOC |
| c | `view: per-sequence slot state machine (depth 1)` | `seqState`, `slot()`, 라우팅(W 매개화, 이 커밋에서는 `PipelineDepth` 미설정=1), 이벤트 구동 `run`, non-blocking 스텝, `onCommitQuorum/tryDeliver/advanceLow`, prev 링, `Start()` 복원 변환, `verifyProposal(seq,…)`, `expectedDecisions`, `head/seqLock`, `verifiedSigs`. `view_test.go`·`viewchanger_test.go`·`state_test.go` 전부 통과. **가장 크고 위험한 커밋** — `TestTwoPrePreparesInARow`, `TestViewLaggingCatchup`, `TestViewPersisted` 가 W=1 동일성의 회귀 울타리 | ~500 LOC (삭제 ~250 포함) |
| d | `view: pipeline window, prefix rule and pre-prepare resend` | `PipelineDepth/PrePrepareResendInterval` 를 `ProposalMaker→View` 로 배선(`consensus.go:327-347`), `resendTick`, 중복 로그 레벨, D2 는 (c) 의 루프에 이미 포함되어 있음을 테스트로 고정. 컨트롤러는 아직 토큰 1개 → 통합 동작은 W=1 그대로. 단위 테스트 `TestViewSlotRouting/InOrderDeliver/FollowerPreparePrecondition/LeaderResendsPrePrepares/GetMetadataPredictsDecisions` | ~150 LOC + 테스트 ~400 |
| e | `controller: leader token semaphore and proposer loop` | `PipelineDepth` 배선(`consensus.go:425-448`), `acquireLeaderTokens/drainLeaderTokens`, `proposerLoop/propose`(Reserve, currentView, Stopped), `startView`(R1, UnreserveAll), `decide`(R4), drain 3곳, `controllerDone.Add(2)`. `controller_test.go` 11개·`controller_decide_test.go` 통과(fixture `PipelineDepth:1`). 단위 `TestControllerTokenSemaphore/DecideRotationNoDoubleToken/ProposerLoopDiscardsTokenOnStoppedView` | ~200 LOC + 테스트 ~200 |
| f | `test: multi in-flight integration tests` | `TestMultiInFlightBasic`, `TestPipelineDecideInOrder`, `TestPipelineLeaderStopsAtWindow`, `TestPipelineDepthOneEqualsBaseline` | ~300 LOC |

(a),(b) 는 독립이라 어느 순서든 가능. (c) 는 (b) 와 무관하지만 (e) 가 (a) 를 필요로 하고 (d),(e) 가 (c) 를 필요로 한다. 각 커밋 후 `go test ./internal/... ./pkg/...` (~18s) 와 `go test ./test` (~2분 20초) 를 돌린다.

---

## 13. 열린 질문 (리뷰어 결정 요청; 괄호는 권고)

1. **재브로드캐스트 주기의 출처와 값** — 새 설정 필드 없이 `LeaderHeartbeatTimeout/LeaderHeartbeatCount`(기본·fastConfig 모두 6s)를 쓸지, `RequestBatchMaxInterval` 의 배수를 쓸지, 아니면 설정 필드를 추가할지. 이벤트 구동(팔로워가 간극을 발견해 요청)은 새 메시지 타입이 필요해 제외했다. (권고: 하트비트 주기. 유실은 드물고 복구 지연 6s 는 `ViewChangeTimeout` 20s 보다 작다.)
2. **W>1 에서 pre-prepare `Save` 의 `truncateTo`** — 이 브랜치에서 `true` 유지(W=1 바이트 동일, WAL 크기 유계, 재시작은 어차피 미지원) vs `slot.seq == low` 로 바꿔 하위 미결정 레코드를 보존(브랜치 5 의 전제이지만 정상 파이프라이닝에서 절단이 거의 일어나지 않아 벤치 중 WAL 이 자란다). (권고: `true` 유지, 브랜치 5 가 `WindowMark` 와 함께 바꿈.)
3. **`Controller.Decide` 블로킹 유지** — §6.5 의 논거로 유지. 벤치에서 `view_latency_batch_save` 가 라운드 지연의 유의미한 비율이면 비동기 전달 큐를 후속 브랜치로. (권고: 유지.)
4. **중복 pre-prepare 로그 레벨의 `W>1` 분기** (§2.2-2) — 분기 없이 항상 Warn 으로 두고 로그 소음을 감수할지. (권고: 분기. 틱당 W×(N-1) Warn 은 운영 로그를 망친다.)
5. **Batcher Closed 시 토큰 폐기 vs 반환** — 현재(`:464-466`)처럼 폐기하면 `changeView :396-400` early-return 경로에서 `Batcher.Reset()` 이 호출되지 않아 토큰이 영구 소멸할 수 있다(leader-token-flow 열린 문제 1; 실제 도달 가능성 미확인 — 리더가 뷰 실행 중 `Sync()` 를 부르는 경로를 코드에서 찾지 못함). 반환하면 Closed 가 풀리기 전까지 busy-loop. (권고: 폐기 유지 + `Batcher.Reset` 을 early-return 경로에도 넣는 별도 수정은 baseline 결함 수정으로 분리.)
6. **`IncomingMessageBufferSize` 와 W** — 큐 cap 을 W 에 비례해 키울지. (권고: 그대로 두고 벤치에서 `HandleMessage` 블록 시간을 측정.)
7. **`StoreProposal` 의 seq 추출** — 메타데이터 언마셜(실패 시 키 0) vs 시그니처를 `StoreProposal(seq, prop)` 로 바꾸고 `state.go:58-67, 161, 221` 세 호출자를 수정(`state.go` 는 브랜치 5 영역). (권고: 메타데이터 추출; 운영 경로에서 메타데이터는 항상 유효.)
8. **prev 응답의 baseline 결함 재현 여부** (§2.3-1) — 슬롯 정확 응답(권고) vs 결함 복제.
9. **`Reserve` 키 계산 비용** — `RequestInspector.RequestID` 를 배치마다 호출. Fabric 의 `RequestID` 는 envelope 파싱이다. 대안: `NextRequests` 가 `RequestInfo` 도 반환(인터페이스 변경 폭 확대). (권고: 현 안; 리더가 어차피 `VerifyProposal` 에서 같은 파싱을 한다.)
10. **팔로워 수용 구간 W+1 칸** (§3.3) — `[low, low+W]` 수용/`[low, low+W)` 처리 분리가 맞는지 확인. 대안 `[low, low+W)` 만 수용은 W=1 에서 baseline 의 `next` 슬롯을 없애 즉시 회귀한다. (권고: W+1.)
11. **`view_in_flight_proposals` 의 의미** — PR #2 의 정의(pre-prepare 수신~전달)는 팔로워에서도 W 까지 오른다. 리더 전용 "제안~전달" 게이지를 따로 둘지. (권고: 그대로; 벤치는 로그 차이로 계산.)
12. **간극 지속 시 추가 sync 트리거** — W>1 에서 pre-prepare 간극이 재브로드캐스트로도 메워지지 않으면(리더 교체 직전 등) 기존 하트비트 behind 카운터에만 의존. (권고: 추가 없음; 뷰 체인지가 해결.)

## 14. 리뷰 결정 반영 (2026-11-02)

결정 전문은 [decisions-2026-11-02.md](decisions-2026-11-02.md). 이 문서에 미치는 변경:

- **§5 재브로드캐스트**: W>1 전용 확정. **1차 복구 경로는 W>1 에서도 sync 이고, 재전송은 지연 단축용 보조다.** 따라서 §10 안전성 논증과 §9 불변식은 재전송에 의존하지 않는다. 틱은 하트비트 주기(`LeaderHeartbeatTimeout/LeaderHeartbeatCount`), 새 설정 필드 없음. 한계: 6 s 복구 지연이 손실 실험 p99 에 드러남 → 후속 브랜치 후보 gap-triggered 1회 재전송(노드·슬롯당 1회 상한).
- **§2 W=1 편차 3개**: 승인. 조건으로 **§11 테스트 계획에 추가**: `TestMessageHistogramMatchesBaseline` — 하네스 `MutateSend` 훅으로 메시지 종류별(pre-prepare/prepare/commit/heartbeat/view-change) 송신 수를 세어 golden 파일(`baseline` 태그에서 1회 생성, 저장소에 커밋)과 비교. 대상 `TestBasic`(결정적, 정확 일치)과 `TestNodeInFlightThenViewChange`(타이밍 의존 heartbeat 는 범위 비교, 그 외는 정확 일치). W=1 main 에서 통과해야 "W=1 은 원본과 메시지 수준에서 동일" 문장의 근거가 된다.
- **§6.x Lemma 추가 (DecisionsInView)**: *Lemma (예측식 = 검사식).* 리더의 `expectedDecisions(head) = DecisionsInView + (head − low)` 와 팔로워의 `expectedDecisions(k)` 는 같은 식이며, 뷰 시작 쌍 `(ProposalSequence, DecisionsInView)` 가 노드 간 일치하면 뷰 내 모든 k 에서 일치한다. 뷰 시작 쌍은 `changeView(view, seq, dec)` (controller.go:386-414) 가 세 값을 함께 받아 `startView` (:363-384) → `NewProposer(..., proposalSequence, viewNumber, decisionsInView, ...)` 로 넘기며, 호출 4곳(:491 새 뷰 `dec=0`, :503 sync 후 상태의 dec, :510 뷰 재시작 시 현재 dec, :561 로테이션 시 현재 dec)이 모두 노드 간 동일한 프로토콜 입력에서 쌍을 유도한다. 뷰 시작 쌍의 일치는 baseline 의 `verifyProposal` 등식 검사가 이미 요구하는 조건이므로 새 가정이 아니다.
- **§6.5 `Controller.Decide` 블로킹 유지**: 한계 기록 — Deliver 완료까지 뷰 고루틴이 블록되어 다른 슬롯의 prepare/commit 이 `incMsgs` 에 적체된다. W 를 키워도 TPS 가 오르지 않으면 첫 용의자. 판별: `view_latency_batch_save`(= Deliver 소요, PR #2 주석) 를 라운드 지연과 함께 그래프에.
- **§1 한계 (d)**: 승인. 추가 요구 — **W>1 이고 WAL 에 in-flight 레코드가 있는 상태의 재시작을 `Restore`(또는 `Consensus.Start`)에서 명시적으로 거부(error 반환, panic 아님)**. 실험 중 실수로 밟았을 때 원인이 즉시 드러나게 한다. 브랜치 5 가 이 거부를 제거한다.
- §13 ①②③⑩ 및 나머지: 권고대로.
