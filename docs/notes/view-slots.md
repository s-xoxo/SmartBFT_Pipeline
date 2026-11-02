# view-slots
- 날짜: 2026-11-02
- 대상 커밋: SmartBFT 32a628d6 (baseline). 로컬 `main`(ea1d499d)은 baseline 위에 `.github/PULL_REQUEST_TEMPLATE.md` 1개만 추가된 커밋이라 `internal/bft/*` 는 32a628d6 과 바이트 단위로 동일함 (`git diff --stat 32a628d6 HEAD` 로 확인). 아래 행 번호는 모두 32a628d6 기준.
- probe 브랜치: `probe/view-slots` — worktree `/Users/liam/Desktop/smartbft/wt-view-slots` (커밋 b1ef7663 `probe: measure pre-prepare drops when leader bursts W consecutive proposals`). 추가 파일 1개: `internal/bft/probe_view_slots_test.go`. 제품 코드는 수정하지 않았음.

## 체크리스트

### [x] `View` 구조체 72행: current(`prePrepare/prepares/commits`)·next(`nextPrePrepare/nextPrepares/nextCommits`) 외에 시퀀스에 종속된 필드 전부 나열 (`inFlightProposal`, `inFlightRequests`, `currPrepareSent`, `currCommitSent`, `prevPrepareSent`, `prevCommitSent`, `myProposalSig`, `lastVotedProposalByID`, `beginPrePrepare`)

`internal/bft/view.go:72-127` 의 런타임 필드를 "시퀀스 하나에 묶인 것 / 뷰 전체에 묶인 것" 으로 분류했다.

| 필드 (`view.go` 행) | 생명주기 | 쓰는 곳 | 읽는 곳 | 윈도우에서 |
|---|---|---|---|---|
| `ProposalSequence` (90) | 뷰 전체, 단조 증가 | `startNextSeq` 867 (`++`), `Restore`(state.go:173,230) | 거의 모든 분기: 220,227,233,336,401,459,487,577,813,857,912,969 | **low watermark** (최초 미결정 seq) 로 의미 고정. "다음에 제안할 seq" 는 별도 `headSeq` 가 필요 |
| `DecisionsInView` (91) | 뷰 전체 | 868 (`++`) | 582 검증, 913 메타데이터 | 슬롯 k 의 기대값 = `base + (k - low)` 로 계산해야 함 |
| `Phase` (93) | **현재 seq 1개의 상태기계** (`COMMITTED→PROPOSED→PREPARED→COMMITTED`) | `doPhase` 285-302 | `run` 273-283 | 슬롯별 `phase` 로 이동. 뷰 수준에는 `ABORT` 만 남음 |
| `lastVotedProposalByID` (96) | 뷰 전체 (sender→마지막 commit). 뷰 시작 시 `make` (134) | `discoverIfSyncNeeded` 779 | 785-798 | 뷰 수준 유지 (seq 비교 조건만 수정) |
| `incMsgs` (97) | 뷰 전체, cap `InMsgQSize` (132) | `HandleMessage` 193 | 277,372,454,540 | 유지 |
| `myProposalSig` (98) | **seq 1개** | `processPrepares` 485, `Restore` state.go:239 | `decide` 860 | 슬롯별 |
| `inFlightProposal` (99) | **seq 1개** | 359(nil), 422, Restore 155,215 | 330,446 | 슬롯별 |
| `inFlightRequests` (100) | **seq 1개** | 360(nil), 423, Restore 253 | 341, 350 | 슬롯별 |
| `lastBroadcastSent` (101) | **seq 1개** (현재 seq 의 prepare 또는 commit) | 361(nil), 419, 518, Restore 166,227 | `doPhase` 288, 291 (재전송) | 슬롯별 `lastSent`; 재전송 루프는 모든 열린 슬롯을 순회 |
| `currPrepareSent`/`currCommitSent` (103-104) | **seq 1개**, `Assist=true` 복제본 | 355-358, 420-421, 516-517 | `processProposal` 355-356 (prev 로 이동만) | 슬롯별 `prepareSent/commitSent` |
| `prevPrepareSent`/`prevCommitSent` (107-108) | **직전 seq 1개** | 355-356 | `handlePrevSeqMessage` 741, 750 | 결정된 슬롯을 즉시 삭제하지 않고 "최근 결정 W개" 링으로 보존 |
| `prePrepare` (110) | 현재 seq, `chan` cap 1 (137) | 323 | 374 | 슬롯별 `prePrepare *protos.Message` (채널 불필요, 아래 참조) |
| `prepares`/`commits` (111-112) | 현재 seq, `voteSet` | 250, 259 | 456, 542, 528(cap) | 슬롯별 |
| `nextPrePrepare`/`nextPrepares`/`nextCommits` (114-116) | 다음 seq | 248, 257, 323 | swap 886-905 | 슬롯 맵으로 흡수 |
| `beginPrePrepare` (118) | **seq 1개** (지연 메트릭 시작점) | 399 | 348 | 슬롯별 |
| `blacklistSupported` (121) | 뷰 전체(캐시) | 1102 | 1083 | 유지 |
| `abortChan`/`stopOnce`/`viewEnded` (122-124) | 뷰 전체 | 131-135 | - | 유지 |
| `ViewSequences` (126) | 뷰 밖 공유 (`*atomic.Value`) | 268-271, 878, util.go:313 | heartbeat/controller (아래 항목) | 발행값 의미 재정의 |

요약: "시퀀스 1개에 종속" 인 필드는 `Phase, myProposalSig, inFlightProposal, inFlightRequests, lastBroadcastSent, currPrepareSent, currCommitSent, beginPrePrepare, prePrepare, prepares, commits` 의 11개 + `prev*Sent` 2개 + `next*` 3개. 이 16개가 `seqState` 로 묶이고, 뷰 수준에는 `ProposalSequence(low)`, `DecisionsInView`, `lastVotedProposalByID`, `incMsgs`, `abortChan` 계열, `blacklistSupported`, `ViewSequences` 만 남는다.

추가로 **뷰 밖** 에 있는 단일 in-flight 가정 (본 섹션의 범위 밖이지만 `View` 와 직접 결합):
- `InFlightData` (`internal/bft/util.go:184-260`): `v *inFlightProposalData` 가 proposal **1개** + `prepared bool`. `PersistedState.Save` (`state.go:41-47`) 가 ProposedRecord 저장 시마다 `StoreProposal` 로 **덮어쓴다** (state.go:59-68). 뷰체인지의 `CheckInFlight` (`viewchanger.go:851-852`) 도 `expectedSequence := maxLastDecisionSequence(messages) + 1` 하나만 본다.
- `PersistedState.Restore` (`state.go:113-253`): WAL 의 마지막 2 엔트리 = (pre-prepare, commit) 1쌍만 복구. `Save(..., truncateTo=true)` 가 `processProposal` 416 에서 pre-prepare 마다 호출되어 이전 seq 의 기록을 잘라 버리므로, 윈도우에서는 **열린 슬롯 W개의 pre-prepare 를 모두 보존** 하도록 truncation 지점이 `low` 의 pre-prepare 로 바뀌어야 한다.

### [x] `processMsg()` 197행: 메시지 seq 를 `v.ProposalSequence` 와 비교해 current/next/prev/미래 로 분기하는 로직 — 윈도우로 바꿀 때 분기 조건

`internal/bft/view.go:197-263` 의 분기를 그대로 적으면 (S = `v.ProposalSequence`, s = `proposalSequence(m)`):

```go
// view.go:205  뷰 번호 불일치 → 비리더면 discoverIfSyncNeeded, 리더면 Complain(+Sync)+stop
if msgViewNum != v.Number { ... }
// view.go:220  s == S-1  → prev
if msgProposalSeq == v.ProposalSequence-1 && v.ProposalSequence > 0 {
    v.handlePrevSeqMessage(msgProposalSeq, sender, m); return
}
// view.go:227  s ∉ {S, S+1} → "미래/과거" → discoverIfSyncNeeded 후 폐기
if msgProposalSeq != v.ProposalSequence && msgProposalSeq != v.ProposalSequence+1 {
    v.Logger.Infof("%d got message from %d with sequence %d but our sequence is %d", ...)
    v.discoverIfSyncNeeded(sender, m); return
}
// view.go:233
msgForNextProposal := msgProposalSeq == v.ProposalSequence+1
```
이어서 `msgForNextProposal` 불리언 하나로 `nextPrepares/prepares`(247-251), `nextCommits/commits`(255-260), `nextPrePrepare/prePrepare`(317-320) 를 고른다. 즉 현재 수용 구간은 **`[S-1, S+1]` 3칸** 이고 그중 `S-1` 은 재전송 전용, `S, S+1` 두 칸만 투표를 축적한다. 미래 메시지(≥ S+2) 는 **버퍼링 없이 폐기** 되며, commit 일 때만 sync 판단 재료로 쓰인다. 자기 자신의 prepare/commit 은 242-244 에서 버려진다 (자기 표는 `myProposalSig` 로 `decide` 860 에서 합산).

윈도우(깊이 W) 로 바꿀 때 분기 조건 초안 (low = `v.ProposalSequence`, 즉 최초 미결정 seq):

| 구간 | 현재 | 윈도우 |
|---|---|---|
| prev (재전송 응답) | `s == S-1` | `low-W ≤ s < low` (최근 결정 W개까지 `prepareSent/commitSent` 보존 → `handlePrevSeqMessage` 참조) |
| 수용 (투표 축적) | `s ∈ {S, S+1}` | `low ≤ s < low+W` → `slot := v.slots.get(s)` (없으면 생성) |
| 미래 → sync 판단 | `s ≥ S+2` | `s ≥ low+W` (commit 만 `discoverIfSyncNeeded`) |
| 과거 (prev 보다 더 옛) | `s < S-1` 도 227 분기로 떨어져 `discoverIfSyncNeeded` 로 감 (commit 이 아니면 무시) | `s < low-W` 는 그냥 폐기 |

주의: 현재 코드는 `s+1 == S` 로 next 를 인식하므로 **S+1 의 pre-prepare 를 리더가 보내도 "next" 슬롯에 들어간다** — 이것이 W=2 가 사실상 이미 반쯤 구현돼 있는 이유다 (다음 항목의 실측과 일치). 단 next 슬롯은 `decide` 가 끝나 `startNextSeq` 로 swap 될 때까지 **처리되지 않고 대기만** 한다 (`processProposal` 374 는 `v.prePrepare` 만 읽음).

### [x] `processPrePrepare()` 304행: `nextPrePrepare` 용량 1, 차 있으면 드롭 → 리더가 W개를 연달아 보내면 몇 개가 버려지는지 실측 (로그 삽입)

정적 경로 (`view.go:304-327`):
```go
prePrepareChan := v.prePrepare          // cap 1 (view.go:137)
if msgForNextProposal { prePrepareChan = v.nextPrePrepare }  // cap 1 (view.go:138, 892)
select {
case prePrepareChan <- m:
default:
    v.Logger.Warnf("Got a pre-prepare for %s sequence without processing previous one, dropping message", currentOrNext)
}
```
그러나 이 "채널 가득" 드롭은 **같은 seq 의 pre-prepare 가 두 번 올 때만** 발생한다. 서로 다른 seq 를 W개 보내면 `S, S+1` 이외는 `processPrePrepare` 에 도달하기 전에 `processMsg` 227-230 에서 이미 폐기된다. 두 드롭 경로 모두 리더에게 아무 신호도 보내지 않고, 리더는 pre-prepare 를 `processProposal` 426 에서 **단 1회** 브로드캐스트한다 (`doPhase` 288/291 가 재전송하는 `lastBroadcastSent` 는 prepare/commit 이지 pre-prepare 가 아님). 따라서 폐기된 pre-prepare 는 **영구 손실** 이며, 해당 seq 는 팔로워가 결코 prepare 하지 못한다.

실측 — 제품 코드 수정 없이 로그 훅으로 두 드롭 경로의 로그 문자열을 셈 (`wt-view-slots/internal/bft/probe_view_slots_test.go`, `TestProbePipelinedPrePrepareBurst`). 팔로워 View(N=4, Quorum=3, SelfID=2, LeaderID=1, `ProposalSequence=0`) 에 리더가 seq `0..W-1` 을 연속 전송. 결정적 타이밍을 위해 기존 `TestTwoPrePreparesInARow` (`view_test.go:1311-1316`) 와 같은 기법 사용: seq 0 의 `VerifyProposal` 모의 호출(동기, `incMsgs` 를 읽지 않는 구간) 안에서 seq `1..W-1` 을 투입 → 전부 `incMsgs` 에 쌓인 뒤 `processPrepares` 루프(454) 의 `processMsg` 로 순서대로 처리됨. 이후 seq 0 를 prepare→commit→decide 까지 진행시키고 seq 1 이 swap 되어 prepare 되는지 확인.

```
go test ./internal/bft/ -run TestProbePipelinedPrePrepareBurst -count=1 -v   (-race 도 통과)
PROBE W=1: sent=1, slots filled(prepared)=1 (next-slot used=false), channel-full drops=0, not-current/next drops=0, decided=1
PROBE W=2: sent=2, slots filled(prepared)=2 (next-slot used=true),  channel-full drops=0, not-current/next drops=0, decided=1
PROBE W=3: sent=3, slots filled(prepared)=2 (next-slot used=true),  channel-full drops=0, not-current/next drops=1, decided=1
PROBE W=4: sent=4, slots filled(prepared)=2 (next-slot used=true),  channel-full drops=0, not-current/next drops=2, decided=1
PROBE W=8: sent=8, slots filled(prepared)=2 (next-slot used=true),  channel-full drops=0, not-current/next drops=6, decided=1
```
결론: **W개 연속 전송 시 정확히 `max(W-2, 0)` 개가 버려지고**, 버려지는 지점은 `processPrePrepare` 의 채널이 아니라 `processMsg` 227 분기(`"got message from %d with sequence %d but our sequence is %d"` Info 로그) 다. `processPrePrepare` 의 `"dropping message"` Warn 경로는 중복 seq 에만 반응한다 (기존 테스트 `view_test.go:1346-1347` 가 그 경우를 다룬다). seq 1 은 `nextPrePrepare` 에서 대기하다가 seq 0 결정 직후 swap 되어 정상 prepare 됨(next-slot used=true). 따라서 깊이 W 파이프라인은 `nextPrePrepare` 용량을 늘리는 것으로는 안 되고, 227 분기의 수용 구간을 `[low, low+W)` 로 넓혀야 한다.

### [x] `startNextSeq()` 864행: swap 로직 — `ViewSequences.Store` 가 "다음 제안 seq" 를 발행하는데 하트비트·sync 가 이 값을 어떻게 쓰는지 (`heartbeatmonitor.go`, `statecollector.go`)

swap 본체 (`view.go:864-906`): `ProposalSequence++`, `DecisionsInView++` (867-868) → `ViewSequences.Store(ViewSequence{ProposalSeq: nextSeq, ViewActive: true})` (878) → 채널 swap: `v.prePrepare = v.nextPrePrepare`, 옛 current 채널은 **드레인 후 버리고 `make(chan, 1)` 새로 생성** (886-893) → `prepares/commits` 는 `tmpVotes.clear(v.N)` 후 next 자리로 재사용 (896-905). `ViewSequence` 타입은 `util.go:345-348` (`ViewActive bool; ProposalSeq uint64`). 다른 Store 지점: 뷰 생성 시 `util.go:313-316` (`ProposalSeq: proposalSequence`), 뷰 종료 시 `view.go:268-271` (`ViewActive: false`, 값은 그대로).

의미: W=1 에서는 "방금 결정한 seq+1" = "다음에 제안할 seq" = "최초 미결정 seq" 가 전부 같은 수라서 한 값으로 충분했다. 파이프라인에서는 이 셋이 분리된다 (head = 리더가 다음에 제안할 seq, low = 최초 미결정 seq, head-low ≤ W).

읽는 곳 3군데와 각각의 가정:

1. 리더 하트비트 `heartbeatmonitor.go:350-374` `leaderTick`: `sequence = vs.(ViewSequence).ProposalSeq` 를 `HeartBeat.Seq` 에 넣어 브로드캐스트.
2. 팔로워 수신 `heartbeatmonitor.go:214-255` `handleHeartBeat`: `active, ourSeq := hm.viewActive(hb)` (298-311, 자기 `ViewSequences`) 다음
   ```go
   if ourSeq+1 < hb.GetSeq() { ... "syncing and ignoring"; hm.handler.Sync(); return }   // 234-238
   if ourSeq+1 == hb.GetSeq() { hm.followerBehind = true; ... }                        // 239-245
   ```
   그리고 `followerTick` 397-402 에서 `behindCounter >= numOfTicksBehindBeforeSyncing` 이면 `Sync()`. **허용 격차는 정확히 1** 이다. 리더가 head 를 발행하면 정상 동작 중인 팔로워(low 가 head-W) 가 `ourSeq+1 < hb.Seq` 로 매 하트비트마다 sync 를 유발한다 (W ≥ 3 에서 즉시, W=2 에서는 `numOfTicksBehindBeforeSyncing` 틱 후).
3. 상태 전송 `controller.go:334-348` `respondToStateTransferRequest`: `Sequence: vs.(ViewSequence).ProposalSeq` 로 응답. `StateCollector.collectedEnoughEqualVotes` (`statecollector.go:103-127`) 는 **(View, Seq) 가 완전히 같은 응답이 f+1 개** 있어야 채택 (`votesMap[viewAndSeq]++ ... if count > uint64(s.f)`), 그리고 `controller.go:664` 는 `response.View > newViewNum && response.Seq == latestDecisionSeq+1` 일 때만 사용. 파이프라인에서 각 노드의 head/처리 위치가 다르면 f+1 개의 동일값을 모으기 어려워진다. 또 `controller.go:506-510` 는 sync 결과가 (0,0) 일 때 `ViewSequences.ProposalSeq` 를 그대로 새 뷰의 시작 seq 로 넘긴다.

설계 결정: `ViewSequences` 에 저장하는 값은 **low watermark(최초 미결정 seq, = 마지막 결정 seq+1)** 로 고정한다. 리더의 head 는 하트비트에 싣지 않는다 (또는 `HeartBeat` 에 별도 필드로 싣되 sync 판단에는 쓰지 않음). 이렇게 하면 W=1 에서는 현행 값과 동일하고, 세 읽기 지점의 "격차 1" 가정이 그대로 성립한다 — 팔로워가 뒤처진다는 것은 저장된 low 가 리더의 low 보다 작다는 뜻이고 그것은 윈도우 깊이와 무관하다. `Store` 호출 위치는 `startNextSeq` 가 아니라 "low 가 전진할 때" (연속 결정 블록을 전달한 직후) 로 옮긴다.

### [x] `handlePrevSeqMessage()` 723행: 뒤처진 노드를 위해 직전 seq 의 prepare/commit 을 재전송 — 윈도우에서는 "직전" 이 W개가 됨

`view.go:723-761`: pre-prepare 는 Warn 후 무시 (724-727). prepare/commit 중 `Assist` 플래그가 켜진 것(= 재전송 응답)은 무시 (738-740, 747-749) 하여 핑퐁을 막고, 그렇지 않으면 `v.prevPrepareSent` / `v.prevCommitSent` 를 **요청자 1명에게** `SendConsensus` 로 유니캐스트 (742, 751). `prev*Sent` 는 `processProposal` 355-356 에서 `curr*Sent` 로부터 **덮어써지는 1칸** 이라 보존 범위가 정확히 seq `S-1` 하나다. 트리거 조건은 `processMsg` 220 (`s == S-1`).

윈도우에서: 뒤처진 노드는 `low_them ∈ [low_me-W, low_me)` 어디든 있을 수 있고 각 seq 에 대해 prepare/commit 을 요청할 수 있다. 따라서 (a) 결정이 끝난 슬롯을 즉시 삭제하지 않고 `prepareSent/commitSent` 만 남겨 `decided` 링(크기 W, 또는 `low-W` 미만 GC) 에 보존, (b) 220 조건을 `low-W ≤ s < low` 로, (c) 응답은 요청 seq 의 슬롯에서 꺼낸다. `Assist` 규칙은 그대로 둔다. `curr*Sent` 가 `proto.Clone` 후 `Assist=true` 로 만들어지는 로직(420-421, 516-517) 은 슬롯 생성 시 그대로 옮긴다. 또 뒤처진 노드가 열린 슬롯(`low ≤ s < low+W`) 의 prepare 를 못 받은 경우는 현재와 같이 `doPhase` 288/291 의 주기적 재브로드캐스트가 담당하므로, 재브로드캐스트 루프가 **모든 열린 슬롯의 `lastSent`** 를 순회해야 한다 (현재는 `v.lastBroadcastSent` 1개).

### [x] `discoverIfSyncNeeded()` 763행: 받은 메시지 seq 가 내 seq 보다 얼마나 앞서면 sync 를 트리거하는지 — 파이프라인에서는 W 만큼 앞선 게 정상이므로 임계값 조정 필요

`view.go:763-823`. commit 만 대상 (765-768). `threshold := f + 1` (776-777), `v.lastVotedProposalByID[sender] = commit` (779) 로 **발신자당 마지막 commit 1개** 를 뷰 수명 동안 누적(삭제 없음), 발신자 수가 f+1 미만이면 종료 (785). (digest, view, seq) 히스토그램에서 `count ≥ f+1` 이면서
```go
if vote.view < v.Number { continue }                                   // 808
if vote.seq <= v.ProposalSequence && vote.view == v.Number { continue } // 813
v.stop(); v.Sync.Sync()                                                // 819-820
```
즉 **같은 뷰에서 `seq ≥ S+1` 인 commit 이 f+1 개** 모이면 sync. 단 이 함수에 commit 이 도달하는 경로는 (i) 뷰 번호 불일치·비리더 (208), (ii) `s ∉ {S, S+1}` (229) 두 곳뿐이고, `S+1` 의 commit 은 229 에 오기 전에 257 `nextCommits` 로 흡수되므로 실질 트리거는 **`seq ≥ S+2` (= 수용 구간 바로 바깥)** 의 commit f+1 개다. 다른 뷰(`vote.view > v.Number`) 의 commit 은 seq 무관하게 f+1 이면 sync.

조정: 임계값 상수 자체는 `f+1` 그대로 두되, 813 의 비교를 `vote.seq < low + W` 로 바꾼다 (`low ≤ s < low+W` 는 슬롯에 흡수되므로 사실상 자동 충족되지만, 뷰 불일치 경로 (208) 로 들어온 같은-뷰 commit 은 없으므로 명시적으로 쓰는 것이 안전). 이렇게 해도 의미는 "수용 구간 밖의 commit 이 f+1" 로 동일하다. 주의할 점: 파이프라인에서 정직한 노드가 `low+W` 이상의 commit 을 보내는 일은 리더가 W 를 지키는 한 없으므로 임계값 상향(예: `f+1` → 더 큼)은 불필요하고, 오히려 `lastVotedProposalByID` 가 발신자당 1개만 기억하므로 W 개 슬롯의 commit 이 빠르게 교체되는 환경에서도 과거 항목이 더 오래 남지는 않는다.

### [x] `voteSet` (`util.go`): 시퀀스별로 하나씩 만들 때 메모리·초기화 비용

`internal/bft/util.go:102-136`:
```go
type voteSet struct {
    validVote func(voter uint64, message *protos.Message) bool
    voted     map[uint64]struct{}
    votes     chan *vote
}
func (vs *voteSet) clear(n uint64) { for len(vs.votes) > 0 { <-vs.votes }; vs.voted = make(map[uint64]struct{}, n); vs.votes = make(chan *vote, n) }
func (vs *voteSet) registerVote(...) { if !vs.validVote(...) {return}; if _, has := vs.voted[voter]; has {return}; vs.voted[voter] = struct{}{}; vs.votes <- &vote{...} }
```
- 비용: `clear(n)` 1회 = map 할당(버킷 n) + 채널 할당(버퍼 n 포인터). `vote` 구조체는 16B(포인터+uint64) 이고 메시지 자체는 공유 포인터. N=4..16 규모에서 seq 당 prepares+commits 2세트 = map 2개 + chan 2개 ≈ 수백 바이트~1KB 수준, W 배. 블록 1개(수십 KB~MB) 대비 무시 가능.
- 초기화 시점: 현재 `setupVotes` (`view.go:147-185`) 에서 뷰 시작 때 4세트, 이후 `startNextSeq` 에서 재사용(`clear`). 윈도우에서는 **첫 메시지(pre-prepare 또는 prepare/commit 어느 것이든) 도착 시 lazy 생성** 해야 한다 — 현재도 `nextPrepares` 에 pre-prepare 보다 먼저 prepare 가 들어올 수 있고(248) 그 표는 swap 뒤 소비되므로, 이 "pre-prepare 전 투표 수용" 성질을 유지해야 한다. `sync.Pool` 로 재사용하면 `clear` 와 같은 비용.
- 안전성: `votes` 는 cap N 이고 `voted` 로 발신자당 1표로 제한되므로 `registerVote` 의 송신(135)이 블록되지 않는다 — 슬롯별로도 동일 불변식이 성립해야 함 (한 슬롯에 N 초과 표 불가).
- `processCommits` 528 은 `cap(v.commits.votes)` 로 `validVotes` 채널 크기를 정하므로 슬롯의 voteSet 를 넘겨야 한다.

### [x] `decide()` 856행 → `Decider.Decide`: 순서대로만 호출되는지, 아니면 윈도우에서 out-of-order 커밋을 막는 장치를 새로 넣어야 하는지

현재 호출 순서는 **구조적으로 순서가 보장** 된다:
- `decide` (`view.go:856-862`) 는 `prepared()` 350 에서 뷰 goroutine 이 **동기적으로** 호출하고, `prepared()` 는 `doPhase` 292 에서 `Phase==PREPARED` 일 때만 실행된다. 한 뷰는 `run()` 273-283 의 단일 goroutine 이 돌리고 Phase 는 하나이므로 seq S 의 결정 전에는 S+1 의 prepare 수집(`processPrepares`) 조차 시작되지 않는다.
- `decide` 는 `startNextSeq()` 를 **먼저** 호출해 `ProposalSequence` 를 올리고(859) 그 다음 `Decider.Decide` 를 호출한다(861). `Controller.Decide` (`controller.go:887-920`) 는 `decisionChan` (cap 1, `controller.go:803`) 에 넣고 `delivered` 가 닫힐 때까지 **블록** (915-919) — 즉 어플리케이션 `Deliver` 가 끝날 때까지 뷰 goroutine 은 다음 seq 의 메시지를 처리하지 못한다. 이 블록 중 뷰가 abort 되면 `enqueueDecision` (911, 928-) 로 백그라운드 전달.
- `Controller.decide` (`controller.go:516-569`) 는 Deliver 후 `incrementCurrentDecisionsInView`, 리더 회전 체크, 그리고 리더이면 **`acquireLeaderToken()`** (566-568) → `run()` 496-497 `propose()` → 다음 제안. 따라서 **리더가 다음 pre-prepare 를 만들 수 있는 유일한 시점이 "직전 결정이 전달된 뒤"** 이다. 이것이 컨트롤러 수준의 단일 in-flight 가정이며, 뷰를 윈도우로 바꿔도 이 토큰 로직을 바꾸지 않으면 파이프라인이 열리지 않는다 (타 섹션 담당이지만 뷰 설계의 전제).

윈도우에서는 **새 장치가 필요** 하다: 슬롯 k+1 의 commit 정족수가 k 보다 먼저 찰 수 있으므로 (네트워크 순서, 검증 goroutine 544-546 비동기), 슬롯 phase 가 `COMMITTED` 가 되어도 `k == low` 일 때만 `Decider.Decide` 를 부르고, 그렇지 않으면 슬롯에 `(signatures)` 를 보관해 두고 low 가 k 에 도달하는 순간 연쇄 전달한다 (아래 불변식 I3). 또 `Decider.Decide` 가 블록하는 동안 다른 슬롯의 투표 처리가 멈추는 문제는 (a) 전달을 별도 goroutine 으로 빼고 뷰 goroutine 은 `decidedCh` 로 완료를 받거나, (b) `Controller.Decide` 를 non-blocking 으로 바꾸되 `d.view != currView` (533) 검사와 `enqueueDecision` 의 보존 의미를 유지해야 한다. `DecisionsInView` 증가(868) 와 `decide` 내부의 `signatures = append(signatures, *v.myProposalSig)` (860) 는 슬롯의 값으로 바꿔야 한다 (현재 `myProposalSig` 는 뷰 1개라 k+1 의 서명이 k 의 것을 덮어쓸 수 있음).

## 결론 / 설계 결정

### 상태 구조 초안

```go
// internal/bft/view.go (초안)
type slotPhase uint8
const (
    slotEmpty    slotPhase = iota // 투표만 먼저 도착, pre-prepare 아직 없음
    slotProposed                  // pre-prepare 검증·WAL 기록·prepare 송신 완료 (현 PROPOSED)
    slotPrepared                  // 정족수 prepare → commit 송신 완료 (현 PREPARED)
    slotCommitted                 // 정족수 commit 수집 완료, 전달 대기 (low 가 아니면 보류)
    slotDelivered                 // Decider.Decide 완료; prepareSent/commitSent 만 재전송용으로 보존
)

type seqState struct {
    seq      uint64
    phase    slotPhase
    // 현 View 의 seq 종속 필드 (view.go:93,98-118) 를 그대로 이동
    prePrepare       *protos.Message        // 현 prePrepare/nextPrePrepare chan(cap 1) 대체. 중복은 nil 검사로 드롭
    proposal         *types.Proposal        // inFlightProposal
    requests         []types.RequestInfo    // inFlightRequests
    prepares         *voteSet               // lazy: 첫 prepare 또는 pre-prepare 때 clear(N)
    commits          *voteSet
    lastSent         *protos.Message        // lastBroadcastSent (doPhase 재전송 대상)
    prepareSent      *protos.Message        // currPrepareSent (Assist=true 복제본)
    commitSent       *protos.Message        // currCommitSent
    mySig            *types.Signature       // myProposalSig
    signatures       []types.Signature      // processCommits 결과; slotCommitted 에서 보관
    decisionsInView  uint64                 // base + (seq-low), verifyProposal 582 의 기대값
    beginPrePrepare  time.Time
}

type View struct {
    // ... 기존 설정 필드 (72-94) 유지. ProposalSequence 의 의미 = low watermark.
    PipelineDepthMax uint64                 // W (config 로부터; 1 = 현행 동작)
    // Runtime
    low      uint64                         // == ProposalSequence. 최초 미결정 seq
    head     uint64                         // 리더 전용: 다음에 제안할 seq, head-low < W
    slots    map[uint64]*seqState           // 키 ∈ [low-W, low+W) ; low-W 미만은 GC
    lastVotedProposalByID map[uint64]*protos.Commit  // 뷰 수준 유지
    incMsgs  chan *incMsg
    abortChan chan struct{}; stopOnce sync.Once; viewEnded sync.WaitGroup
    blacklistSupported bool
    ViewSequences *atomic.Value            // 저장값 = ViewSequence{ProposalSeq: low, ViewActive}
    // Phase(93) 는 삭제 (뷰 수준엔 abort 만), lastBroadcastSent/curr*/prev*/next*/prePrepare 채널 삭제
}
```
`processMsg` 라우팅: `s < low-W → drop`, `low-W ≤ s < low → slots[s].prepareSent/commitSent 로 handlePrevSeqMessage`, `low ≤ s < low+W → slots[s] (lazy 생성) 로 pre-prepare/prepare/commit 등록`, `s ≥ low+W → discoverIfSyncNeeded (commit 만)`. `doPhase` 는 `for s := low; s < low+W; s++` 로 열린 슬롯을 순회해 각 슬롯의 phase 를 전진시키고 `lastSent` 를 재브로드캐스트한다. 현 코드의 `processPrepares/processCommits` 가 **블록하는 select 루프** (450-465, 536-551) 인 점이 윈도우에선 걸림돌이다 — 슬롯 k 의 prepare 를 기다리며 블록하면 k+1 의 메시지는 `processMsg` 로 라우팅만 되고 phase 전진은 못 한다. 따라서 이 두 함수는 "표 1개를 처리하고 정족수 여부를 반환" 하는 non-blocking 형태로 분해해야 한다 (`voteSet.votes` 채널을 그대로 쓰되 `select default`).

### 유지해야 하는 불변식

- **I1 (슬롯 유일성)** 한 (view, seq) 에 대해 `seqState` 는 하나, pre-prepare 는 최초 1개만 수용 (현 cap-1 채널의 "중복 드롭" 과 동일 의미, `view_test.go:1346-1347` 의 두 로그 메시지는 유지 또는 동등한 테스트로 대체).
- **I2 (윈도우 경계)** 리더: `head - low < W` 일 때만 `Propose` (W=1 이면 현행 "결정 후 제안" 과 동일). 팔로워: `s ≥ low+W` 인 pre-prepare 는 수용하지 않음 (리더가 W 를 어기면 지금처럼 폐기 + commit 시 sync 판단).
- **I3 (순서 전달)** `Decider.Decide(seq k)` 는 `k == low` 이고 `slots[k].phase == slotCommitted` 일 때만 호출되며, 호출 후 `low++`. 결과적으로 Decide 호출 순서 = seq 순서 (현행과 동일한 외부 관찰 가능 동작). `Checkpoint`/`RetrieveCheckpoint` 는 항상 `low-1` 을 가리킴.
- **I4 (단일 전진)** `ProposalSequence`(low), `DecisionsInView`, `ViewSequences.ProposalSeq` 는 I3 의 전달 시점에만, 1씩 증가. 세 값이 분기하지 않음.
- **I5 (메타데이터 결정성)** 슬롯 k 의 pre-prepare 메타데이터는 `LatestSequence == k`, `DecisionsInView(k) == DecisionsInView(low) + (k - low)` — 즉 `verifyProposal` 577/582 가 "현재값과 같다" 가 아니라 "슬롯 기대값과 같다" 로 바뀜. ViewId 검사(572) 는 그대로.
- **I6 (자기 표 1회)** 각 슬롯의 `mySig` 는 그 슬롯의 `processPrepares` 결과로만 쓰이고 `decide` 때 그 슬롯의 `signatures` 에만 합산 (현 860 의 뷰 수준 `myProposalSig` 공유 금지).
- **I7 (WAL 보존)** 열린 슬롯 `[low, head]` 의 ProposedRecord/Commit 은 모두 WAL 에 남아 있어야 함 → `State.Save(..., truncateTo=true)` 는 슬롯 k 의 pre-prepare 가 아니라 "low 가 k 로 전진한 뒤 첫 기록" 에서만. `Restore` 는 다중 슬롯 복구가 필요 (현 `state.go:113-253` 는 1쌍).
- **I8 (재전송 가용성)** `low-W ≤ s < low` 의 `prepareSent/commitSent` 를 보존하여 `handlePrevSeqMessage` 가 답할 수 있음; `Assist=true` 메시지는 응답하지 않음.
- **I9 (sync 임계)** 같은 뷰에서 `seq ≥ low+W` 인 commit 이 f+1 발신자에게서 (digest,view,seq) 일치로 모이면 sync. 다른 뷰의 commit 은 현행 그대로.
- **I10 (하트비트 격차)** 하트비트/StateTransferResponse 의 Seq 는 low. 팔로워의 "뒤처짐" 판정 `ourSeq+1 == hb.Seq`, "즉시 sync" `ourSeq+1 < hb.Seq` 는 **변경 없이** 유효 (리더 head 를 보내지 않으므로).
- **I11 (뷰체인지 in-flight)** `InFlightData` 는 현재 1개. 윈도우에선 최소 `low`(최초 미결정) 슬롯의 proposal/prepared 를 보고해야 하며, 열린 슬롯 전부를 보고하려면 `ViewData.InFlightProposal` 프로토 확장이 필요 — 본 섹션 범위 밖, 열린 문제로 넘김.

## 열린 문제

1. **리더 회전·블랙리스트와 파이프라인의 충돌 (미해결, 설계 필요).** `Propose` 981 은 `PrevCommitSignatures: prevSigs` 를, `GetMetadata` 934/996 은 `PrevCommitSignatureDigest` 를 **직전 seq 의 commit 서명** 으로 채우고, 팔로워는 `verifyProposal` 593-606 (`DecisionsPerLeader > 0` 일 때) 에서 이를 검증한다. 슬롯 k 를 제안할 때 k-1 이 아직 결정되지 않았으면 그 서명이 존재하지 않는다. 선택지: (a) `DecisionsPerLeader > 0` 이면 W 를 1 로 강제, (b) "마지막으로 결정된 seq(= low-1)" 의 서명을 싣고 검증도 그에 맞춰 완화(블랙리스트 갱신 결정성 재검토 필요). 본 probe 에선 판단 보류.
2. **`Controller.Decide` 의 블로킹 전달.** `controller.go:915-919` 가 delivered 까지 뷰 goroutine 을 세우므로 W > 1 에서 전달 지연이 다른 슬롯의 진행을 막는다. 뷰 goroutine 과 전달을 분리하되 `d.view != currView` / `enqueueDecision` 의미 보존이 필요. `decisionChan` cap 1 (803) 도 W 로 늘릴지 결정.
3. **리더 토큰.** `controller.go:566-568, 496-497` 의 "결정 후 토큰 → propose" 구조는 W=1 을 강제한다. 뷰가 `head-low < W` 를 보장하는 `CanPropose()` 를 노출하고 컨트롤러가 토큰을 결정과 무관하게 재획득하는 식으로 바꿔야 함 (섹션 외).
4. **WAL/Restore 다중 슬롯** (I7) 과 **뷰체인지 in-flight 다중화** (I11) 는 각각 `state.go`, `viewchanger.go` 담당 섹션과 맞춰야 함. 특히 `CheckInFlight` (`viewchanger.go:851-852`) 의 `expectedSequence = maxLastDecision+1` 단일값 가정.
5. **[?] 실측의 한계.** 측정은 뷰 단위 테스트(모의 Comm/Verifier) 로 `incMsgs` 에 W개를 결정적으로 적재한 것이며, 네트워크 타이밍에 따라서는 seq 0 이 `v.prePrepare` 에서 소비되기 전에 seq 0 중복이 와 `processPrePrepare` 채널-풀 경로가 추가로 발동할 수 있다(그 경우도 서로 다른 seq 의 손실 수는 `W-2` 로 동일). 실제 리더가 W개를 연속 전송하는 경로는 현 컨트롤러에 존재하지 않아(열린 문제 3) 통합 테스트(`test/`) 로는 재현 불가했다.
6. **`InMsgQSize` 와 W.** `incMsgs` 가 cap `InMsgQSize` 이고 `HandleMessage` 193 은 블록 송신이다. 윈도우에서 슬롯당 메시지가 N×(1+2) 개로 W 배 늘어나므로 큐 크기를 W 에 비례해 키우지 않으면 `Controller` 의 수신 goroutine 이 뷰에 막힐 수 있다 (정량 미확인).
