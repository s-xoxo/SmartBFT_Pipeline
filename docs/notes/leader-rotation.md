# 로테이션과 파이프라인 충돌
- 날짜: 2026-11-02
- 대상 커밋: SmartBFT 32a628d6 (baseline)
- probe 브랜치: 없음 — 읽기만
- 비고: `main` HEAD(ea1d499d) 와 baseline 사이에 관련 파일 diff 없음(leader-token-flow.md 비고 참조).

## 체크리스트

### [x] `pkg/types/config.go`: `LeaderRotation` (기본 true), `DecisionsPerLeader` (기본 3)

```go
// pkg/types/config.go:75-78
// LeaderRotation is a flag indicating whether leader rotation is active.
LeaderRotation bool
// DecisionsPerLeader is the number of decisions reached by a leader before there is a leader rotation.
DecisionsPerLeader uint64
```
기본값 `config.go:109-110`: `LeaderRotation: true, DecisionsPerLeader: 3`.

`Validate` (`config.go:176-181`) 가 두 값을 **강결합**한다:
```go
if c.LeaderRotation && c.DecisionsPerLeader == 0 { return errors.New("DecisionsPerLeader should be greater than zero when leader rotation is active") }
if !c.LeaderRotation && c.DecisionsPerLeader != 0 { return errors.New("DecisionsPerLeader should be zero when leader rotation is off") }
```
즉 `LeaderRotation=false` 는 곧 `DecisionsPerLeader=0` 이고, `DecisionsPerLeader` 는 뷰 코드에서 **로테이션 on/off 스위치**로도 쓰인다:
- `View.Propose` (`internal/bft/view.go:965-967`): `if v.DecisionsPerLeader > 0 { _, prevSigs = v.RetrieveCheckpoint() }` — 0 이면 `PrevCommitSignatures` 를 싣지 않음.
- `bindCommitSignaturesToProposalMetadata` (`view.go:992-995`): 0 이면 `PrevCommitSignatureDigest` 미기록.
- `updateBlacklistMetadata` (`view.go:1039-1040`): 0 이면 블랙리스트 비움.
- `verifyProposal` (`view.go:604`): `... && v.DecisionsPerLeader > 0` 일 때만 digest 일치 검사.
- `verifyBlacklist` (`view.go:655-660`): 0 이면 블랙리스트가 비어 있어야 함.

리더 선출 `getLeaderID` (`internal/bft/util.go:72-100`):
```go
if !leaderRotation { return nodes[view%n] }
for i := range nodes {
    index := (view + (decisionsInView / decisionsPerLeader)) + uint64(i)
    node := nodes[index%n]
    if _, exists := blackListed[node]; !exists { return node }
}
```
로테이션 on 이면 **같은 뷰 번호 안에서** `decisionsInView / decisionsPerLeader` 가 바뀔 때마다 리더가 바뀐다. 뷰 번호는 그대로이며 뷰체인지 프로토콜(ViewChange/ViewData/NewView 메시지)은 **돌지 않는다**.

### [x] `controller.go` `decide()` → `checkIfRotate()` → `changeView(...)`: 결정 3개마다 뷰 재시작. 파이프라인 깊이 W > 3 일 때 윈도우 중간에서 뷰가 바뀌면 어떻게 되는지 (in-flight 제안은 뷰체인지 경로로 복구?)

경로 (`internal/bft/controller.go:541-564`):
```go
c.incrementCurrentDecisionsInView()
...
if c.checkIfRotate(md.GetBlackList()) {
    c.changeView(c.getCurrentViewNumber(), md.GetLatestSequence()+1, c.getCurrentDecisionsInView())
    c.RequestPool.RestartTimers()
}
```
`checkIfRotate` (`571-585`): `currLeader := getLeaderID(..., decisionsInView-1, ...)`, `nextLeader := getLeaderID(..., decisionsInView, ...)`, 둘이 다르면 true — 즉 `decisionsInView % DecisionsPerLeader == 0` 이 되는 결정에서 참(기본 3, 6, 9, …번째 결정).

`changeView(sameView, latestSeq+1, decisions)` (`386-414`) → `abortView` (`416-434`): `relinquishLeaderToken()` 후 `c.currView.Abort()` (`view.go:1016-1019`, 뷰 고루틴 종료 대기) → `startView(latestSeq+1)` 로 **같은 뷰 번호**의 새 `View` 객체를 새 리더로 생성 (`363-384`). 각 노드가 자기 `decide` 안에서 **로컬·결정적**으로 수행하며 메시지 교환이 없다.

**W > 1 에서 윈도우 중간 로테이션의 결과 (코드 기반 추론)**:
1. 로테이션을 유발한 결정이 seq k 라 하자. 파이프라인에서는 옛 리더가 이미 k+1 … k+W-1 을 pre-prepare 로 보냈고, 일부 팔로워는 prepare/commit 까지 진행했을 수 있다.
2. 각 노드는 k 를 전달하는 순간 뷰를 `Abort()` 한다. 뷰 객체의 `inFlightProposal`(`view.go:99`, `422`), `prepares/commits` 투표집합, `nextPrePrepare` 버퍼(`138`)가 모두 버려진다. 새 뷰는 seq `k+1` 에서 시작하므로 **새 리더가 k+1 을 다시 제안**한다. 요청은 `removeDeliveredFromPool` (`controller.go:935-941`) 로 전달 후에만 풀에서 빠지므로 옛 k+1 의 요청은 풀에 남아 재제안된다 — 요청 유실은 없고 W-1 개 제안분의 작업이 낭비된다.
3. **뷰체인지 경로로 복구되지 않는다.** `InFlight` 가 뷰체인지에서 쓰이는 지점은 `ViewChanger.prepareViewDataMsg` (`internal/bft/viewchanger.go:467-490`) 하나이고 이는 ViewChange 프로토콜(뷰 번호 증가)에서만 실행된다. 로테이션은 뷰 번호를 바꾸지 않으므로 `InFlightData` 에 저장된 옛 k+1 (`state.go:67` 에서 저장됨)은 참조되지 않고, 다음 `State.Save` 가 덮어쓴다(`util.go:238`).
4. **안전성 위험 (추론, 미실험)**: 로테이션은 쿼럼 합의 없이 각 노드가 k 를 전달한 시점에 일어난다. 노드별로 k 전달 시점이 다르므로, 어떤 노드는 아직 옛 뷰에서 옛 k+1 에 commit 을 보내고 다른 노드는 새 뷰에서 새 k+1 에 commit 을 보낼 수 있다. 두 제안은 **같은 (view, seq)** 를 가진다. 현재 코드는 뷰 고루틴이 `Decide` 에서 블록(`view.go:861`, `controller.go:915-919`)되어 k 전달 완료 전에는 k+1 메시지를 처리하지 못하므로 이 창이 사실상 닫혀 있지만, 파이프라인이 그 블록을 풀면 창이 열린다. 2f+1 노드가 옛 k+1 에 commit 한 뒤 로테이션하고, 2f+1 노드가 새 k+1 에 commit 하면 같은 (view, seq) 에 두 개의 커밋 증명이 생길 수 있다. 이를 코드로 배제하려면 "옛 리더가 로테이션 경계를 넘는 seq 를 제안하지 않는다" 는 규칙이 필요하다(아래 결론).
5. **집계 불일치 (코드로 확인)**: 로테이션 후 `decisionChan` 에 남아 있던 옛 뷰의 결정(옛 k+1 이 이미 결정됐다면)은 `decide:533-540` 의 `d.view != currView` 경로로 **전달은 되지만 `decisionsInView` 에 집계되지 않는다**. 로테이션 전에 k+1 을 전달한 노드는 집계하고, 후에 전달한 노드는 집계하지 않으므로 `getLeaderID` 입력이 노드마다 달라져 리더 불일치 → 하트비트 타임아웃 → 뷰체인지로만 회복된다. 역시 경계 규칙으로 "옛 뷰의 미전달 결정이 로테이션 시점에 존재하지 않음" 을 보장해야 한다.
6. **PrevCommitSignatures 결합 (코드로 확인)**: 로테이션 on 이면 리더는 `Propose` 에서 Checkpoint(= 마지막 **전달된** 제안)의 commit 서명을 `PrevCommitSignatures` 로 싣고(`view.go:965-981`), `GetMetadata` 가 그 digest 를 메타데이터에 묶는다(`924-934`, `996`). 팔로워는 `verifyPrevCommitSignatures` (`611-652`) 에서 **자기 Checkpoint 의 제안**에 대해 서명을 검증하고, `604` 에서 digest 일치를 검사한다. 파이프라인에서 리더가 k 미결 상태로 k+1 을 만들면 prevSigs 는 k-1 의 것이다; k 를 이미 전달한 팔로워의 Checkpoint 는 k 이므로 k-1 서명이 k 제안에 대해 검증 실패 → `processProposal:389-396` 에서 `Complain` + `Sync` + `stop()` → 뷰체인지. **즉 로테이션 on(DecisionsPerLeader>0) 상태에서는 로테이션 시점과 무관하게 W ≥ 2 제안이 거부된다.** 로테이션 off(0) 이면 이 세 검사가 모두 비활성이다.

### [x] 뷰 재시작 시 `RequestPool.RestartTimers()` 의 비용

`internal/bft/requestpool.go:470-488`:
```go
func (rp *Pool) RestartTimers() {
	rp.lock.Lock()
	defer rp.lock.Unlock()
	rp.stopped = false
	for reqInfo, element := range rp.existMap {
		item := element.Value.(*requestItem)
		item.timeout.Stop()
		ri := reqInfo
		to := time.AfterFunc(rp.options.ForwardTimeout, func() { rp.onRequestTO(item.request, ri) })
		item.timeout = to
	}
}
```
- 복잡도 O(풀 크기): 항목마다 `timer.Stop` + `time.AfterFunc` 1회. 상한은 `RequestPoolSize` (기본 400, `config.go:97`); 파이프라인 실험에서는 풀을 더 크게 잡을 것이므로 비용은 비례 증가.
- **배타 락**(`rp.lock.Lock()`) 을 전 구간 보유. 같은 락을 `Submit` (`requestpool.go:233`), `NextRequests` (`296`), `RemoveRequest` (`373`), `Size` (`286`) 가 사용하므로 로테이션마다 제출·배칭·전달 후 제거가 멈춘다.
- 부수 효과: 모든 요청의 forward 타이머가 `ForwardTimeout`(기본 2s, `config.go:98`) 으로 **재시작**된다. 기본 `DecisionsPerLeader=3` 에서 결정이 2초 이내 간격으로 계속 나오면 어떤 요청도 forward 타임아웃에 도달하지 못한다(장애 감지 지연; 정확성 문제는 아님). 또 `rp.stopped=false` 로 `onRequestTO`/`onLeaderFwdRequestTO` (`501`, `534`) 가 다시 동작.
- 호출 지점: `controller.go:563` (로테이션), `viewchanger.go:384` (뷰체인지 완료 informNewView 류), `viewchanger.go:1230` (NewView 처리 후 `ViewChanged` 직후), `pkg/consensus/consensus.go:256` (reconfig). 로테이션이 유일한 "정상 운영 중 주기적" 호출자다.
- 로테이션의 다른 비용(참고): `currView.Abort()` 가 뷰 고루틴 종료를 `viewEnded.Wait()` 로 동기 대기(`view.go:1018`), 새 `View` 할당 + 투표집합 재생성(`setupVotes` `view.go:147-185`), 새 고루틴, `LeaderMonitor.ChangeRole` (`controller.go:382`), 리더면 `Batcher.Reset()` (`412`) 로 `closeChan` 재생성. 이 모든 것이 `run` 고루틴에서 `decide` 안에 **동기**로 일어나므로 결정 전달 경로의 꼬리 지연에 직접 더해진다. 파이프라인에서는 그 사이 뷰 고루틴이 올리는 다른 결정이 `decisionChan`(용량 1) 에서 막힌다.

### [x] 실험 설정 결정: `LeaderRotation=false` vs `DecisionsPerLeader = k·W` — 논문에 적을 근거

**선택지 A — `LeaderRotation=false` (강제로 `DecisionsPerLeader=0`)**
근거(모두 코드 인용 가능):
1. 파이프라인 차단 요인 제거: 위 6번 — `Propose:965`, `GetMetadata/bind:992`, `verifyProposal:604`, `verifyBlacklist:655` 가 모두 비활성화되어 k 미결 상태의 k+1 제안이 팔로워 검증을 통과한다. 이 결합을 유지하면서 파이프라인하려면 "제안 k+1 이 k-1 의 commit 서명을 싣고 팔로워가 `Checkpoint - inflightGap` 기준으로 검증" 하도록 프로토콜을 바꿔야 하는데, 이는 블랙리스트/리더 책임 추적 의미론(`verifyBlacklist`) 까지 흔든다.
2. 로테이션에 따른 뷰 재시작(`changeView` + `RestartTimers`) 이 없으므로 `decide` 경로가 결정 1건당 상수 비용이 되고, 측정하는 처리량/지연이 파이프라인 효과만 반영한다.
3. 위 4·5번의 안전성/집계 불일치 창이 존재하지 않는다(뷰 번호가 바뀌는 진짜 뷰체인지만 남고, 그것은 `InFlight` + ViewData 경로로 복구된다).
4. 대조군(W=1) 도 동일 설정으로 돌리면 공정 비교가 된다. 기본값(true/3) 과 다르다는 점은 명시해야 하며, 블랙리스트 기능이 꺼지므로 "리더 장애 시 뷰체인지만으로 회복" 하는 모델이 된다.

**선택지 B — 로테이션 on, `DecisionsPerLeader = k·W`**
- `k·W` 는 "윈도우가 항상 꽉 차고 윈도우 시작이 `decisionsInView` 의 W 배수에 정렬된다" 는 가정에서만 경계가 맞는다. 어댑티브 배처가 빈 배치/부분 윈도우를 만들고(`propose:468-470` 빈 배치 시 토큰 반환), 뷰체인지 후 `startView(…, newDecisionsInView)` 가 임의 값에서 시작하므로(`changeView:407`, `sync:645` `latestDecisionDecisions + 1`) 정렬은 보장되지 않는다. 따라서 `k·W` 는 **튠 파라미터일 뿐 안전 장치가 아니며** 경계 규칙을 코드로 강제해야 한다(아래 초안).
- 6번 PrevCommitSignatures 문제는 `DecisionsPerLeader` 값과 무관하게 남는다 → B 를 택하려면 프로토콜 수정이 선행.
- 장점: 기본 설정과의 연속성, 블랙리스트 유지, "로테이션 오버헤드 vs 파이프라인" 절삭 실험 가능.

**결정(권고)**: 주 실험은 **A (`LeaderRotation=false`)**, W=1 대조군도 동일. B 는 경계 규칙 + prevSig 완화를 구현한 뒤 `DecisionsPerLeader ∈ {W, 2W, 4W}` 절삭 실험으로 분리하고, 구현하지 못하면 "향후 과제" 로 둔다. 논문에는 1·2·3 을 코드 라인과 함께 근거로 적는다.

## 결론 / 설계 결정

### 규칙 초안: 로테이션은 윈도우 경계에서만 일어난다

정의
- `d` = 현재 `decisionsInView` (전달된 결정 수), `DPL` = `DecisionsPerLeader`.
- epoch(d) = `d / DPL` — `getLeaderID` (`util.go:91`) 가 쓰는 항. 같은 epoch 안에서는 리더가 같다.
- 제안 seq `s` 의 결정 번호 = `d + inflightBefore(s) + 1`; 그 제안의 `DecisionsInView` 메타데이터 값은 `d + inflightBefore(s)`.

**규칙 B1 (리더 측 경계 상한)**: 리더는 `epoch(DecisionsInView(s)) == epoch(d)` 인 seq 만 제안한다. 즉 허용 in-flight 수
```
allowed(d, inflight) = min(W, DPL - (d mod DPL)) - inflight
```
이 값이 0 이면 토큰이 있어도 제안하지 않는다(토큰을 채널에 남긴다).

**규칙 B2 (토큰 발급과 결합)**: leader-token-flow.md 의 R1 을 `startView` 에서 `min(W, DPL - (d mod DPL)) - restoredInFlight` 로 바꾸고, R2 의 결정당 +1 은 `epoch` 가 바뀌지 않을 때만 수행한다(epoch 가 바뀌면 `checkIfRotate` 가 참이 되어 `changeView` 가 R3(drain)+R1 을 수행하므로 R4 그대로).
- 로테이션 off(DPL=0) 이면 `allowed = W - inflight` 로 환원.

**규칙 B3 (팔로워 측 검사)**: `verifyProposal` (`view.go:582-585`) 의 `md.DecisionsInView == v.DecisionsInView` 정확 일치를 파이프라인용으로 완화할 때, 반드시 `epoch(md.DecisionsInView) == epoch(v.DecisionsInView)` 를 추가 검사한다. 이로써 악의적/버그 리더가 경계를 넘는 제안을 보내도 팔로워가 거부하고, 4번의 같은-(view,seq) 이중 제안 창이 닫힌다(옛 리더는 경계 너머 seq 에 대한 유효 제안을 만들 수 없음).

**규칙 B4 (로테이션 시점 불변식)**: B1+B3 가 성립하면 epoch 마지막 결정 k 를 전달하는 순간 옛 뷰에는 미결/미전달 제안이 없다 → `decide:533-540` 의 비집계 경로는 진짜 뷰체인지에서만 발생하고(현재 의미 유지), `changeView` 의 drain 은 토큰만 회수하면 된다.

**규칙 B5 (RestartTimers)**: B4 로 로테이션 빈도가 `DPL` 결정마다 1회로 고정되므로 `RestartTimers` 비용은 결정당 `O(pool)/DPL`. `DPL = k·W` 로 두면 윈도우 `k` 개마다 1회가 되어 파이프라인 윈도우 비용과 같은 단위로 보고할 수 있다 — 이것이 `k·W` 를 쓰는 유일한 정당화(안전성과 무관, 측정 편의).

의사코드 (리더 측, proposerLoop 안):
```go
func (c *Controller) allowedInFlight() int {
	w := c.PipelineDepthMax
	if c.LeaderRotation {                         // DPL > 0
		d := c.getCurrentDecisionsInView()
		left := int(c.DecisionsPerLeader - d%c.DecisionsPerLeader)
		if left < w { w = left }
	}
	return w - c.inFlightCount()
}
// 토큰을 뽑은 뒤:
if c.allowedInFlight() <= 0 { c.acquireLeaderTokens(1); wait(nextDecisionOrAbort); continue }
```
팔로워 측 (`verifyProposal` 완화 부분):
```go
if v.DecisionsPerLeader > 0 &&
   md.GetDecisionsInView()/v.DecisionsPerLeader != v.DecisionsInView/v.DecisionsPerLeader {
	return nil, errors.New("proposal crosses leader rotation boundary")
}
```

### 실험 매트릭스에 적을 설정
| 구성 | LeaderRotation | DecisionsPerLeader | 비고 |
|---|---|---|---|
| baseline | false | 0 | W=1, 원본 코드 경로(`PipelineDepthMax=1`) |
| pipe-W | false | 0 | W ∈ {2,4,8}, AdaptiveBatch on/off |
| rot-ablation (선택) | true | k·W (k∈{1,2,4}) | B1~B5 + prevSig 완화 구현 후에만 |

## 열린 문제
1. **PrevCommitSignatures 완화 설계 미정**: 로테이션 on 에서 k 미결 상태의 k+1 제안이 어떤 서명을 싣고 팔로워가 무엇에 대해 검증할지(`view.go:593-606`, `611-652`). 이 변경 없이는 B 선택지가 성립하지 않는다. 블랙리스트(`verifyBlacklist:654~`) 의미론과 함께 뷰 섹션 담당과 합의 필요.
2. **4번 안전성 시나리오는 실행으로 확인하지 않음**: 코드 구조상 창이 열린다는 추론이며, 파이프라인 구현 후 "로테이션 경계에서 같은 (view, seq) 에 두 커밋 증명" 을 재현하는 테스트를 작성해 B1/B3 가 막는지 검증해야 한다.
3. **`changeView` early-return 과 경계 규칙의 상호작용**: `changeView:396-400` 은 `decisionsInView` 가 같으면 뷰를 재시작하지 않는다. B2 가 `startView` 에서 토큰을 재계산하는 데 의존하므로, early-return 경로에서는 토큰 재계산이 일어나지 않는다 — 이 경로가 로테이션 직후 발생할 수 있는지 확인 필요.
4. **`decisionsInView` 가 sync 로 점프할 때**(`sync:645` `newDecisionsInView = latestDecisionDecisions + 1`, `679` `0`) epoch 가 바뀌어도 `checkIfRotate` 를 거치지 않고 `changeView(view, seq, dec)` 로 들어간다. 이 경우 리더/토큰은 `startView` 가 재계산하므로 B2 로 커버되지만, in-flight 가 있던 상태였다면 `maybePruneInFlight` (`controller.go:695-718`) 의 다중 항목 버전이 필요하다(leader-token-flow.md 참조).
5. `RequestPool.RestartTimers` 의 실제 지연은 측정하지 않았다(풀 400~수천 항목에서 µs~ms 추정). 로테이션 절삭 실험에서 `decide` 지연 히스토그램(`MetricsView.LatencyBatchSave` 와 별도)로 측정할 것.
