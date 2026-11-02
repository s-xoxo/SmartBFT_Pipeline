# 제안 직렬화 지점 — leaderToken 흐름 (controller.go)
- 날짜: 2026-11-02
- 대상 커밋: SmartBFT 32a628d6 (baseline)
- probe 브랜치: 없음 — 읽기만
- 비고: 현재 `main` HEAD 는 ea1d499d (PR 템플릿 추가 커밋) 이지만 `git diff 32a628d6 HEAD -- internal/bft/controller.go pkg/types/config.go internal/bft/view.go internal/bft/requestpool.go` 가 비어 있어 아래 행번호는 baseline 과 동일하다.

## 체크리스트

### [x] `leaderToken` 채널의 용량과 모든 송수신 지점 나열 (`acquireLeaderToken` 761행, `relinquishLeaderToken` 769행, `run` 477행)

**선언/생성**
- `internal/bft/controller.go:123` — `leaderToken chan struct{}` (Controller 필드)
- `internal/bft/controller.go:802` — `c.leaderToken = make(chan struct{}, 1)` (Start 안에서 매 시작마다 재생성, **용량 1**)
- 테스트 fixture 도 용량 1 로 만든다: `internal/bft/controller_decide_test.go:115`

**송신(토큰 발급) — 단일 함수**
```go
// controller.go:761-767
func (c *Controller) acquireLeaderToken() {
	select {
	case c.leaderToken <- struct{}{}:
	default:
		// No room, seems we're already a leader.
	}
}
```
비블로킹 send. 채널이 이미 가득(=토큰 1개 존재)이면 조용히 버린다. 이 "버림" 이 사실상 idempotent 재발급 수단으로 쓰이고 있다(아래 decide 로테이션 경로 참고).

호출 지점 (3곳):
| 위치 | 조건 |
|---|---|
| `controller.go:376` (`startView`) | `leader && (initPhase == COMMITTED \|\| initPhase == ABORT)` |
| `controller.go:469` (`propose`) | `len(nextBatch) == 0` — 빈 배치면 토큰을 되돌려 놓고 다음 run 루프에서 재시도 |
| `controller.go:567` (`decide`) | 정상 경로 끝에서 `if iAm, _ := c.iAmTheLeader(); iAm` |

**수신(토큰 소비/회수) — 4곳**
```go
// controller.go:769-774
func (c *Controller) relinquishLeaderToken() {
	select {
	case <-c.leaderToken:
	default:
	}
}
```
| 위치 | 의미 |
|---|---|
| `controller.go:496-497` (`run`) | `case <-c.leaderToken: c.propose()` — 유일한 "소비". 토큰 1개 = 제안 1회 허가 |
| `controller.go:427` (`abortView`) | `c.relinquishLeaderToken()` — 뷰 중단 시 **최대 1개** 회수 |
| `controller.go:851-855` (`Stop`) | 인라인 `select { case <-c.leaderToken: default: }` 1개 |
| `controller.go:868-872` (`StopWithPoolPause`) | 동일, 1개 |

`relinquishLeaderToken` 도 비블로킹 1회 수신이므로 **정확히 0 또는 1개만 회수**한다. 용량이 W 가 되면 이 함수는 그대로 쓸 수 없다(아래 결론).

**토큰 ↔ in-flight 불변식(현재)**: 리더일 때 `len(leaderToken) + (뷰에 in-flight 중인 제안 수) == 1`. 토큰은 `run:496` 에서 소비되어 `propose → currView.Propose` 로 바뀌고, 그 제안이 `decide` 에서 전달될 때 `567` 에서 다시 1개 발급된다. 팔로워일 때는 항상 0.

### [x] `decide()` 516행에서 `acquireLeaderToken()` 이 호출되는 조건 전부 — 각 경로에서 토큰 누수 / 중복 발급 가능성

`decide` 는 `run` 고루틴에서만 실행된다(`controller.go:487-488`). 경로별:

1. **reconfig / 정지** — `controller.go:518-527`
   ```go
   reconfig := c.Deliver.Deliver(d.proposal, d.signatures)
   if reconfig.InLatestDecision { c.close() }
   ...
   close(d.delivered)
   if c.stopped() { return }
   ```
   토큰 발급 없음. 이후 `Stop()`/`StopWithPoolPause()` 가 1개를 drain. 누수 없음(용량 1 기준).

2. **뷰 교체됨 (`d.view != currView`)** — `controller.go:533-540`
   ```go
   if d.view != currView {
       // ... the decision must not be accounted to the current view.
       c.MaybePruneRevokedRequests()
       return
   }
   ```
   `incrementCurrentDecisionsInView` 도, 토큰 발급도 하지 않는다. 교체된 새 뷰는 `startView:376` 에서 이미 토큰을 받았으므로 중복 발급을 피하기 위한 return 이다. 테스트 `controller_decide_test.go:41-49` ("view replaced": `require.Empty(t, controller.leaderToken)`).
   - 파이프라인 영향: 이 경로는 "결정 1개가 지연 전달됐다" 를 전제한다. W 개가 지연 전달되면 W 번 이 경로를 타며, 모두 **decisionsInView 에 집계되지 않는다** → 노드마다 집계가 달라져 `getLeaderID` (`util.go:91`) 가 갈라질 위험(leader-rotation.md 참고).

3. **뷰 중단됨 but 교체 안 됨 (`d.view.Stopped()`)** — `controller.go:543-552`
   ```go
   if d.view.Stopped() {
       // ... an aborted view cannot propose, so do not rotate and do not acquire the leader token.
       // Whichever view starts next acquires the leader token in startView if this node leads it.
       c.MaybePruneRevokedRequests()
       return
   }
   ```
   집계는 하되(`541`) 토큰 없음. 테스트 `controller_decide_test.go:29-39`. 토큰 회수는 `abortView:427` 가 이미 수행. 누수 없음.

4. **로테이션** — `controller.go:559-564` 이어서 `566-568`
   ```go
   if c.checkIfRotate(md.GetBlackList()) {
       c.changeView(c.getCurrentViewNumber(), md.GetLatestSequence()+1, c.getCurrentDecisionsInView())
       c.RequestPool.RestartTimers()
   }
   c.MaybePruneRevokedRequests()
   if iAm, _ := c.iAmTheLeader(); iAm {
       c.acquireLeaderToken()
   }
   ```
   `changeView → abortView(relinquish 1개) → startView(새 리더면 acquire 1개)` 뒤에 **다시** `567` 에서 acquire 한다. 용량 1 이므로 두 번째 acquire 는 `default` 로 버려져 무해하다. **용량 W 에서는 이 경로가 토큰을 1개 초과 발급한다**(startView 가 W 개를 채운 뒤 +1 → 비블로킹 send 가 실패하지 않는 한 W+1 은 불가능하지만, startView 가 W-restored 개만 채우는 설계면 +1 이 실제로 들어간다). 반드시 `rotated` 분기 후 return 하거나 startView 에서만 발급하도록 바꿔야 한다.

5. **정상** — `controller.go:566-568`: 리더면 1개 발급. 이것이 "결정 1건 = 허가 1건 반환" 의 유일한 정상 경로.

**중복 발급 가능성(현재)**: 구조적으로 불가능 — 용량 1 + 비블로킹 send. 설계가 이 "실패해도 조용히 무시" 에 의존한다(`765` 주석 "seems we're already a leader").
**누수 가능성(현재)**: `propose:464-466` 에서 `c.stopped() || c.Batcher.Closed()` 이면 토큰을 소비한 채 반환하고 재발급하지 않는다. 의도된 동작(뷰 교체 중이며 다음 `startView` 가 재발급). 단, `Sync()` (`437-442`) 가 `Batcher.Close()` 를 호출한 뒤 `changeView` 가 `396-400` ("view is already running") 로 early-return 하면 `Batcher.Reset()` (`411-413`) 이 실행되지 않아 Batcher 가 Closed 로 남는다 → 다음 토큰은 `464` 에서 소비만 되고 사라진다. 이 조합이 실제로 리더에서 발생하는지는 미확인(열린 문제 1).

### [x] `startView()` 363행, `changeView()` 386행에서 토큰 초기화 방식 — 뷰가 바뀔 때 in-flight 제안이 여럿이면 토큰 몇 개를 회수해야 하는지

`startView` (`controller.go:363-384`):
```go
view, initPhase := c.ProposerBuilder.NewProposer(c.leaderID(), proposalSequence, c.currViewNumber, c.currDecisionsInView, c.quorum)
...
if leader {
    if initPhase == COMMITTED || initPhase == ABORT {
        c.acquireLeaderToken()
    } else {
        c.Logger.Debugf("Not acquiring leader token when starting view with phase %s", initPhase.String())
    }
    role = Leader
}
```
- `initPhase` 는 `ProposalMaker.NewProposer` (`internal/bft/util.go:286-342`) 가 `view.Phase` 를 그대로 돌려준 것. `pm.restoreOnceFromWAL.Do(func(){ pm.State.Restore(view) })` (`util.go:318-323`) 가 **프로세스 수명 중 단 1회**만 WAL 을 복원하므로 PROPOSED/PREPARED 는 재시작 직후 첫 뷰에서만 나온다(`state.go:115` 기본 COMMITTED, `171` PROPOSED, `228` PREPARED).
- 의미: "WAL 에 복원된 in-flight 제안 1개가 있으면 그 제안이 토큰 1개를 점유 중" → 토큰 0개 발급. 즉 `발급 토큰 = 1 - 복원된 in-flight 수` 를 이미 암묵적으로 계산하고 있다. W 로 일반화하면 `발급 = W - restoredInFlight`. 단 `PersistedState.Restore` 는 제안 1개만 복원(`state.go:155`, `215` 에서 `v.inFlightProposal =` 단수 대입)하므로 `restoredInFlight ∈ {0,1}` 이고, WAL 포맷을 바꾸지 않으면 W 개 복원은 불가.

`changeView` (`controller.go:386-414`):
```go
if !c.abortView(latestView) { return }
c.setCurrentViewNumber(newViewNumber)
c.setCurrentDecisionsInView(newDecisionsInView)
c.startView(newProposalSequence)
if iAm, _ := c.iAmTheLeader(); iAm { c.Batcher.Reset() }
```
`abortView` (`416-434`) 가 `relinquishLeaderToken()` **1회** 호출 후 `c.currView.Abort()` (`view.go:1016-1019`: `stop()` + `viewEnded.Wait()`, 뷰 고루틴 종료까지 블록). 즉 현재 회수 규칙은 "뷰당 토큰 최대 1개 회수".

**W 개 in-flight 일 때 회수해야 하는 토큰 수**: 리더가 보유 가능한 토큰은 `0..W` 개(소비되어 in-flight 가 된 것은 채널에 없다). 뷰가 바뀌면 in-flight 제안은 뷰와 함께 버려지므로(뷰체인지 경로는 `InFlight` 로 별도 복구) **채널에 남은 토큰 전부**를 drain 해야 하고, 그 개수는 런타임에 `0..W` 중 임의 값이다. 따라서 `relinquishLeaderToken` 은 "비어질 때까지 루프" 로 바꿔야 한다. 그 다음 `startView` 가 새 뷰 기준으로 `W - restoredInFlight` 를 발급. 예외: `changeView:396-400` early-return(뷰가 이미 같은 설정으로 실행 중) 경로는 토큰을 건드리지 않는다 — 그대로 유지하면 된다.

주의: `run():480-483` 의 defer 는 `c.currView.Abort()` 만 하고 토큰은 `Stop()` 이 drain 한다 → `Stop`/`StopWithPoolPause` 의 인라인 drain 도 루프로.

### [x] `propose()` 463행: `Batcher.NextBatch()` 가 블로킹인데 `run()` 루프 안에서 호출됨 → 깊이 W 로 바꾸면 run 루프가 멈추는지 (별도 고루틴 필요 여부)

```go
// controller.go:463-475
func (c *Controller) propose() {
	if c.stopped() || c.Batcher.Closed() { return }
	nextBatch := c.Batcher.NextBatch()
	if len(nextBatch) == 0 { c.acquireLeaderToken(); return }
	metadata := c.currView.GetMetadata()
	proposal := c.Assembler.AssembleProposal(metadata, nextBatch)
	c.currView.Propose(proposal)
}
```
- `BatchBuilder.NextBatch` (`internal/bft/batcher.go:40-63`): `pool.NextRequests(..., check=true)` 가 full 이 아니면 `time.After(b.batchTimeout)` (기본 `RequestBatchMaxInterval` 50ms, `config.go:95`) 동안 `closeChan`/`submittedChan` 을 기다리며 **블록**. 이 동안 `run` 루프는 `decisionChan`, `viewChange`, `abortViewChan`, `syncChan` 을 전혀 처리하지 못한다.
- 지금 안전한 이유: 토큰이 있다 ⇔ in-flight 가 0 이므로, NextBatch 가 블록 중일 때 도착할 수 있는 결정(decisionChan)이 없다. 뷰체인지 쪽은 `AbortView:448`, `ViewChanged:456-459`, `Sync:438-440` 이 **채널에 보내기 전에 `Batcher.Close()` 를 먼저 호출**해서 `closeChan` 으로 NextBatch 를 깨운다 — 이것이 run 루프가 NextBatch 에 갇히지 않게 하는 유일한 탈출구다.
- **W ≥ 2 에서는 깨진다**: 토큰 1개를 소비해 NextBatch 에서 50ms 블록하는 동안 in-flight W-1 개의 결정이 뷰 고루틴에서 `Decide` 로 올라온다. `decisionChan` 용량 1 (`controller.go:803`) 이라 뷰 고루틴은 `Decide:900-913` 의 send 에서 블록 → 뷰가 메시지를 처리하지 못해 다음 제안의 prepare/commit 수집이 멈춘다 → 파이프라인 이득이 배치 타임아웃만큼 깎인다(최악 W-1 개 결정이 매번 50ms 대기).
- 추가로 `c.currView.GetMetadata()` (`view.go:909-937`) 는 `v.ProposalSequence`, `v.DecisionsInView` 를 뷰 고루틴과 락 없이 읽는다. 현재는 `v.decide` → `startNextSeq()` (`view.go:859`, `864-906`) 가 끝난 뒤 `Decider.Decide` 가 `delivered` 를 기다리고(`controller.go:915-919`), 그 뒤에야 `decide:567` 토큰 → `propose` 가 실행되므로 채널을 통한 happens-before 가 성립한다. W ≥ 2 면 제안 k 가 결정되기 전에 k+1 의 메타데이터를 만들어야 하므로 **이 happens-before 가 사라지고**, 뷰의 `ProposalSequence` 는 아직 k 이다 → 컨트롤러가 자체 "다음 제안 seq/decisionsInView" 커서를 가져야 하며, `View.GetMetadata` 를 인자 있는 형태(`GetMetadataFor(seq, dec)`)로 바꿔야 한다.

**결론**: 별도 고루틴(proposer loop)이 필요하다. 또는 run 루프 안에 두되 NextBatch 를 비블로킹(`NextRequests(check=false)` 즉시 반환 + 타이머 채널을 select 에 합류)으로 바꿔야 한다. 전자가 단순하다: proposer 고루틴이 `leaderToken` 을 소비하고 `Batcher.NextBatch()` 에서 블록해도 run 루프는 영향 없음. 단 `propose` 가 run 고루틴 밖으로 나가면 `currView` 접근은 `currViewLock` 으로 보호해야 하고(지금 `472`, `474` 는 락 없이 `c.currView` 직접 접근 — run 고루틴 전용이라 허용되던 것), `abortView` 와의 경쟁(토큰 drain 직후 proposer 가 이미 토큰을 뽑아 Propose 를 호출하는 창)을 뷰 쪽 `Stopped()` 검사 또는 세대(generation) 번호로 막아야 한다.

### [x] `sync()` 587행 / `maybePruneInFlight()` 695행: 동기화 후 in-flight 제안을 어떻게 버리는지 — 단수 가정이 박힌 곳

`sync` (`controller.go:587-693`) 끝부분:
```go
if latestDecisionMetadata != nil {
    c.maybePruneInFlight(latestDecisionMetadata)   // :683-685
}
```
`maybePruneInFlight` (`695-718`):
```go
inFlight := c.InFlight.InFlightProposal()
if inFlight == nil { return }
...
if syncResultViewMD.GetLatestSequence() < inFlightMD.GetLatestSequence() {
    // In-flight sequence is higher than latest committed sequence then in-flight might still be relevant
    return
}
c.InFlight.clear()
```
단수 가정이 박힌 곳:
- `InFlightData` (`internal/bft/util.go:184-192`): `v *inFlightProposalData` **포인터 1개** (`proposal *types.Proposal; prepared bool`).
- `StoreProposal` (`util.go:232-239`): 무조건 덮어쓰기 `ifp.v = &inFlightProposalData{proposal: &p}` — 두 번째 제안을 저장하면 첫 제안은 사라진다.
- `StorePrepares(view, seq)` (`util.go:242-253`): 인자 `view, seq` 를 **사용하지 않고** 현재 저장된 하나에 `prepared: true` 를 찍는다. 제안 k+1 저장 후 k 의 commit 을 저장하면 k+1 이 prepared 로 잘못 표시된다. `nil` 이면 `panic("stored prepares but proposal is not initialized")`.
- `InFlightProposal()` / `InFlightProposalAndPrepared()` (`195-229`): 단수 반환.
- `clear()` (`255-260`): 전부 비움 — 비교는 "synced seq < inflight seq 면 유지, 아니면 전부 삭제" 1건 판단.
- 채우는 쪽: `PersistedState.Save` (`internal/bft/state.go:41-47`) 가 ProposedRecord 저장 시 `storeProposal`(`59-68`), Commit 저장 시 `storePrepared`(`70-73`). 뷰는 pre-prepare 저장을 `State.Save(savedMsg, true)` (`view.go:416`, truncateTo=true: "everything written before it may be truncated") 로 하므로 **WAL 도 제안 k+1 의 pre-prepare 를 쓰는 순간 k 의 pre-prepare/commit 을 잘라낼 수 있다** — WAL 절단 규칙도 단수 가정.
- 소비하는 쪽: `ViewChanger.prepareViewDataMsg` (`internal/bft/viewchanger.go:467-490`) 가 `InFlightProposalAndPrepared()` 1건을 `ViewData.InFlightProposal/InFlightPrepared` 에 담고, `getInFlight` (`492-532`) 는 in-flight seq 가 lastDecision seq 와 같으면 nil, `inFlight+1 == lastDecision` 인 뷰체인지-중-커밋 특수 케이스만 처리 — "in-flight 는 lastDecision 바로 다음 1건" 전제.

W 개 in-flight 를 지원하려면 `InFlightData` 를 seq 키 정렬 맵(또는 슬라이스)으로 바꾸고, `maybePruneInFlight` 는 `seq ≤ synced seq` 인 항목만 제거(나머지 유지), `StorePrepares` 는 인자 seq 에 해당하는 항목만 표시, WAL 절단은 "가장 오래된 미결 pre-prepare" 기준으로 바뀌어야 한다. ViewData 프로토콜 변경(`InFlightProposal` 반복 필드)은 본 섹션 범위 밖이지만 의존성으로 기록.

### [x] `Decide()` 887행 → `decisionChan` → `decide()`: 결정 전달이 순서 보장되는지 (채널 용량, 호출 주체)

- 호출 주체: 뷰 고루틴. `View.prepared()` (`view.go:329-352`) → `v.decide(...)` (`856-862`) → `v.Decider.Decide(*proposal, signatures, requests)` **동기 호출**. `Decider` 는 컨트롤러(`pkg/consensus/consensus.go:333` `Decider: c.controller`). 뷰체인지 중 전달(`viewchanger.go:1237`, `1398`)은 `v.Application.Deliver` (= `MutuallyExclusiveDeliver`) 를 직접 호출하며 `decisionChan` 을 거치지 않는다.
- 채널: `c.decisionChan = make(chan decision, 1)` (`controller.go:803`).
- `Decide` (`887-920`): `currView` 스냅샷을 `d.view` 에 담고(`888-897`), 
  ```go
  select {
  case c.decisionChan <- d:
  case <-c.stopChan: return
  case <-view.AbortChan():
      go c.enqueueDecision(d)   // :911, 백그라운드 고루틴
      return
  }
  select {
  case <-delivered:              // :916 전달 완료까지 뷰 고루틴 블록
  case <-c.stopChan:
  case <-view.AbortChan():
  }
  ```
- **순서 보장 (현재)**: 생산자가 뷰 고루틴 하나이고, 그 고루틴이 `delivered` 가 닫힐 때까지(`decide:524`) 블록하므로 결정은 한 번에 1건, 자연히 FIFO. 뷰 고루틴이 `Decide` 안에 묶여 있는 동안 뷰는 `incMsgs` 를 처리하지 못한다 — 즉 **뷰 자체가 "결정 전달 완료 전까지 다음 제안 처리 금지" 로 직렬화**되어 있다. 이것이 토큰과 함께 단일 in-flight 를 강제하는 두 번째 지점이다.
- 예외: `view.AbortChan()` 경로의 `go c.enqueueDecision(d)` (`911`, `928-933`) 는 별도 고루틴이므로 두 건 이상이 이 경로로 들어가면 Go 스케줄러에 따라 **순서가 뒤바뀔 수 있다**. 현재는 뷰당 미전달 결정이 최대 1건이라 문제가 없다.
- 순서가 깨질 때의 결과: `MutuallyExclusiveDeliver.Deliver` (`977-1010`) 는 `latest >= pending.LatestSequence` 이면 전달하지 않고 Sync 결과로 대체한다(`989-1000`). k+1 이 먼저 전달되어 Checkpoint 가 k+1 이 되면 k 는 "이미 커밋됨" 으로 **조용히 건너뛰어진다**(Synchronizer 가 k 를 다른 노드에서 받아오면 복구되지만 로컬 전달 순서 k+1→k 는 애플리케이션 입장에서 금지). 따라서 W ≥ 2 에서는 (a) 뷰 고루틴 1개가 seq 순으로만 `Decide` 를 호출하고 (b) abort 경로의 백그라운드 enqueue 를 단일 FIFO 큐(순서 유지 고루틴 1개 또는 슬라이스+뮤텍스)로 바꿔야 한다. `decisionChan` 용량을 W 로 늘리면 뷰 고루틴이 전달 완료를 기다리지 않고 다음 제안을 진행할 수 있으나, `delivered` 대기(`916`)를 제거/완화할지는 전달 지연 vs 뷰 진행 분리 설계와 함께 결정해야 한다.
- 결정 집계의 뷰 식별은 `d.view != currView` 포인터 비교(`533`)이므로 같은 뷰에서 나온 여러 결정은 모두 집계된다 — 이 부분은 W 와 무관하게 유지 가능.

### 추가로 확인된 단일 in-flight 전제 (요청 범위 밖이나 A 결론에 직접 영향)
- `RequestPool.NextRequests` (`internal/bft/requestpool.go:295-330`) 는 `rp.fifo.Front()` 부터 **읽기만** 하고 제거하지 않는다. 제거는 `decide → removeDeliveredFromPool` (`controller.go:523`, `935-941`) 에서 전달 후에만 일어난다. 따라서 제안 k 가 전달되기 전에 `NextBatch` 를 다시 부르면 **같은 요청이 다시 배치에 담긴다**. W ≥ 2 는 풀에 "제안됨/예약됨" 상태(또는 커서)가 필수.
- 뷰 레벨: `v.nextPrePrepare = make(chan *protos.Message, 1)` (`view.go:138`), `processMsg` 는 `ProposalSequence` 또는 `+1` 만 수용(`view.go:227-233`), `processPrePrepare` 는 슬롯이 차 있으면 드롭(`322-326`), `doPhase` 는 제안 1건의 PROPOSED→PREPARED→COMMITTED 를 순차 실행(`285-302`). 즉 뷰 상태기계는 단일 제안용이며 "next" 버퍼는 메시지 선착 허용용. 컨트롤러 토큰을 W 로 늘려도 뷰가 받지 못한다 — 뷰 쪽 변경이 선행 조건.
- `verifyProposal` (`view.go:577-585`) 는 `md.LatestSequence == v.ProposalSequence`, `md.DecisionsInView == v.DecisionsInView` 를 **정확 일치**로 검사. 파이프라인 제안 k+1 은 팔로워 뷰가 k 를 처리 중일 때 도착하므로 이 검사가 거부한다.

## 결론 / 설계 결정

### 세마포어(용량 W)로 바꿀 때 건드려야 하는 함수 목록

| # | 함수 / 위치 | 변경 |
|---|---|---|
| 1 | `Start` `controller.go:802` | `make(chan struct{}, c.PipelineDepthMax)`; `PipelineDepthMax` 필드 추가(기본 1). `decisionChan` 용량도 W 로(순서 유지 전제). |
| 2 | `acquireLeaderToken` `:761` | `acquireLeaderTokens(n int)` — n 번 비블로킹 send. "이미 가득" 을 정상으로 간주하는 현 의미는 **버린다**(중복 발급 감지 불가해짐). 디버그 모드에서 `len(leaderToken)+inflight > W` 패닉. |
| 3 | `relinquishLeaderToken` `:769` | `drainLeaderTokens()` — 빌 때까지 루프. 호출 4곳(`abortView:427`, `Stop:851`, `StopWithPoolPause:868`, 신규) 모두 교체. |
| 4 | `startView` `:363-384` | `leader && phase∈{COMMITTED,ABORT}` → `acquireLeaderTokens(W - restoredInFlight)`; `restoredInFlight` 는 `NewProposer` 가 반환하도록 시그니처 확장(현재는 phase 로 0/1 만 암시). |
| 5 | `abortView` `:416-434` | drain 루프 + `Abort()`; 뷰 세대 번호 증가(proposer 고루틴 경쟁 방지). |
| 6 | `changeView` `:386-414` | 로직 유지. `Batcher.Reset()` 뒤 proposer 고루틴 재가동 신호. |
| 7 | `decide` `:516-569` | 정상 경로 +1 유지; **로테이션 분기(`559-564`)를 탔으면 `567` 발급 생략**(startView 가 이미 W 발급). 뷰 교체/중단 경로는 토큰 변경 없음 유지. |
| 8 | `propose` `:463-475` | run 루프에서 분리 → `proposerLoop()` 고루틴: `for { select { case <-leaderToken: ...; case <-stopChan: return } }`. 메타데이터는 컨트롤러 커서 `(nextSeq, nextDecisions)` 로 `currView.GetMetadataFor(seq, dec)` 호출. 빈 배치 시 토큰 반환 + 백오프. |
| 9 | `run` `:477-514` | `case <-c.leaderToken` 제거(8 로 이동). |
| 10 | `Decide` `:887-920` / `enqueueDecision` `:928-933` | abort 경로의 `go enqueueDecision` 을 순서 보존 큐로. `delivered` 대기 정책 결정. |
| 11 | `maybePruneInFlight` `:695-718` + `InFlightData` `util.go:184-260` | seq 키 다중 항목; `seq ≤ synced` 만 제거; `StorePrepares` 가 인자 seq 사용. |
| 12 | `Stop`/`StopWithPoolPause` `:844-875` | drain 루프. |
| 13 | `removeDeliveredFromPool` `:935-941` + `RequestPool.NextRequests` `requestpool.go:295` | 풀에 예약 상태 도입(제안 시 예약, 전달 시 제거, 뷰 중단 시 예약 해제). |
| 14 | 테스트 `controller_decide_test.go:26,38,48` | `require.Len(..., 1)` 등은 W 기준으로 재작성. |
| 의존 | `view.go` (`138`, `227-233`, `285-302`, `577-585`, `909-937`), `state.go`/WAL 절단, `viewchanger.go` ViewData | 본 섹션 밖. 선행 조건으로 기록. |

### 토큰 회수 규칙 초안

불변식 **I1**: 노드가 현재 뷰의 리더이고 뷰가 실행 중이면 `len(leaderToken) + inflight(view) + restoredInFlight == W`. 팔로워 또는 뷰 중단 상태면 `len(leaderToken) == 0`.

규칙:
- **R1 (발급: 뷰 시작)** `startView` 에서만 "채우기" 를 한다: 리더이고 phase∈{COMMITTED, ABORT} 이면 `W - restoredInFlight` 개. PROPOSED/PREPARED(재시작 복원)면 `W - 1` 개가 아니라 **복원된 개수를 뺀 만큼** — 현재 WAL 은 1개만 복원하므로 당장은 `W-1`.
- **R2 (반환: 정상 결정)** `decide` 정상 경로에서 결정 1건당 토큰 1개. 조건: `d.view == currView && !d.view.Stopped() && !rotated && iAmTheLeader`.
- **R3 (회수: 뷰 중단/교체)** `abortView` 는 `Abort()` 호출 **전에** 채널을 비울 때까지 drain. in-flight 였던 제안은 토큰으로 되돌리지 않는다(뷰와 함께 소멸; 복구는 `InFlight`/뷰체인지 경로).
- **R4 (로테이션)** `checkIfRotate` 참이면 `changeView` 가 R3+R1 을 수행하므로 R2 의 +1 을 **생략**한다.
- **R5 (빈 배치)** proposer 가 토큰을 뽑았으나 배치가 비면 토큰을 되돌린다(현 `469` 와 동일). 단 W 개 토큰이 모두 빈 배치로 즉시 되돌아오면 busy-loop 이므로 `Batcher.NextBatch` 의 타임아웃이 백오프 역할을 하도록 유지(어댑티브 배처가 이 대기를 조절).
- **R6 (정지)** `Stop`/`StopWithPoolPause`/`close` 후 drain 루프; proposer 고루틴은 `stopChan` 으로 종료.
- **R7 (세대 검사)** proposer 가 토큰을 뽑은 뒤 `Propose` 직전에 뷰 세대/`Stopped()` 를 확인, 불일치면 토큰을 **버린다**(되돌리지 않음 — 새 뷰의 R1 이 다시 채움).

의사코드:
```go
func (c *Controller) proposerLoop(gen uint64) {
	for {
		select {
		case <-c.stopChan: return
		case <-c.leaderToken:
		}
		if c.stopped() || c.Batcher.Closed() || c.viewGen.Load() != gen { return } // R7: 토큰 폐기
		batch := c.Batcher.NextBatch()           // 블록해도 run 루프 무관
		if len(batch) == 0 { c.acquireLeaderTokens(1); continue }  // R5
		seq, dec := c.nextProposalCursor()       // view.ProposalSequence + inflight
		md := c.currentView().GetMetadataFor(seq, dec)
		c.currentView().Propose(c.Assembler.AssembleProposal(md, batch))
	}
}

func (c *Controller) abortView(view uint64) bool {
	...
	c.viewGen.Add(1)
	c.drainLeaderTokens()                       // R3: 0..W 개 전부
	c.currView.Abort()
	return true
}

func (c *Controller) decide(d decision) {
	... (교체/중단 경로는 기존과 동일, 토큰 변경 없음)
	rotated := false
	if c.checkIfRotate(bl) { c.changeView(...); c.RequestPool.RestartTimers(); rotated = true } // R4
	c.MaybePruneRevokedRequests()
	if !rotated { if iAm, _ := c.iAmTheLeader(); iAm { c.acquireLeaderTokens(1) } }           // R2
}
```

## 열린 문제
1. **Batcher Closed 잔류**: `Sync()` (`437-442`) 가 리더에서 `Batcher.Close()` 한 뒤 `run:498-511` 의 `changeView` 가 `396-400` 로 early-return 하면 `Batcher.Reset()` 이 호출되지 않는다. 뷰가 stop 되지 않은 상태의 리더가 `Sync()` 를 호출하는 경로가 실제로 존재하는지(뷰의 `v.Sync.Sync()` 호출 `view.go:214`, `393` 은 직후 `v.stop()` 하므로 `stopped==true` 라 해당 없음) 확인하지 못했다. W 에서는 토큰 W 개가 모두 `propose:464` 에서 소멸하므로 영향이 커진다 — 실험으로 재현 필요.
2. **`delivered` 대기 정책**: `Decide:915-919` 가 전달 완료까지 뷰 고루틴을 막는 것이 뷰 상태기계 직렬화의 핵심이다. 파이프라인에서 이를 제거하면 뷰가 k+1 처리 중 k 전달이 실패/재구성(reconfig)될 때의 처리(`518-521` `c.close()`)와 충돌한다. 어디까지 비동기화할지는 뷰 섹션 담당과 합의 필요.
3. **복원 in-flight 개수**: `PersistedState.Restore` 와 WAL 절단(`view.go:416` truncateTo=true)이 1개 전제이므로 R1 의 `restoredInFlight` 는 당장 `{0,1}`. W 개 복원을 지원할지(WAL 포맷 변경) 아니면 재시작 시 파이프라인을 1로 축소(단순)할지 결정 필요.
4. **abort 경로 결정 순서**: `go c.enqueueDecision(d)` (`911`) 가 두 건 이상 동시에 발생할 수 있는 조건을 코드만으로는 단정하지 못했다(현재는 구조상 1건). W 에서 순서 뒤바뀜이 `MutuallyExclusiveDeliver:989-1000` 에 의해 조용한 건너뜀으로 이어지므로 테스트로 고정해야 한다.
5. `PipelineDepthMax` 와 `AdaptiveBatch` 플래그를 `pkg/types/config.go` `Configuration` 에 두고 `Validate` (`115`~) 에 `PipelineDepthMax >= 1` 검사 추가 — 다른 섹션(config) 과 중복 가능, 조정 필요.
