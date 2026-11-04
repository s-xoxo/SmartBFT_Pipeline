# feat 6 `feat/viewchange-multi-inflight` 설계 — 뷰 체인지의 다중 in-flight 복구 (D1 포함)

- 날짜: 2026-11-04
- 대상 커밋 (SmartBFT 포크): main `aab360af` (PR #6 feat 5 포함). 라인 번호는 이 커밋 기준
- 선행: [viewchange-trace.md](viewchange-trace.md) (D 절: 흐름도, 복구 규칙 의사코드, 안전성 논증·Lemma 1–5, D1 코드 확인, sync 순서), [decisions-2026-11-02.md](decisions-2026-11-02.md), [decisions-2026-11-04.md](decisions-2026-11-04.md) 4~6라운드, [design-state-wal-window.md](design-state-wal-window.md) (feat 5)
- 플래그: `PipelineDepthMax` (W). **W=1 에서 ViewData 바이트·메시지 집합·복구 결과 동일** (§9)
- 리뷰어 요청 항목(6라운드): ① D1 `clearAbove(newProposalSequence)` + sync 순서 테스트 2 케이스, ② seq 별 A1/A2/B 순회·접두사 커밋 반복·B 중단 후 접미사 파기(D1)·D2, ③ proto `repeated InFlightEntry in_flight = 6`(4/5 유지), ④ "새 리더가 2 이상 뒤처진 경우" — sync 후 재시작 경로와 `ViewChangeTimeout` 상호작용 테스트, ⑤ feat 5 접합부: 재시작 복원 슬롯이 뷰 체인지 중 `InFlightData` 에 반영되는가(코드 위치)

## 1. 목표와 범위

뷰 v 에서 W 개까지 in-flight 였던 제안을 뷰 v+1 로 넘길 때, (a) 어떤 정직 노드가 전달했을 가능성이 있는 seq 는 모두 같은 값으로 보존(접두사), (b) 그 이후 접미사는 모든 노드에서 파기(D1), (c) 보존 접두사를 새 뷰 시작 전에 순서대로 커밋한다. 현재(feat 4·5) 는 `InFlightData` 의 **최저 seq 1건**만 ViewData 에 싣고 `CheckInFlight` 가 `maxLastDecision+1` 하나만 판정하므로 더 높은 in-flight 는 모든 노드에서 폐기되어 재제안된다 — 안전하지만(Deliver 기준, Lemma 2) 라이브니스 손실이고 D1 이 `InFlightData` 에서 지켜지지 않는다(viewchange-trace "D1 코드 확인").

범위 밖: 뷰 체인지 메시지 수 자체의 변화(ViewData/NewView 개수는 그대로), `ViewChangeTimeout` 값 변경(측정 후), 뒤처진 리더의 최적화(ViewData 에 여러 decision 싣기 — 2차 후보).

## 2. 현재 코드 (`aab360af`)

| 지점 | 현재 | 바꿀 것 |
|---|---|---|
| `messages.proto:65-71 ViewData` | `in_flight_proposal = 4`, `in_flight_prepared = 5` 단수 | `InFlightEntry{Proposal proposal=1; bool prepared=2}`, `repeated InFlightEntry in_flight = 6` 추가. 4/5 유지 |
| `viewchanger.go:467 prepareViewDataMsg` | `InFlight.InFlightProposalAndPrepared()`(최저 1건) → `getInFlight` 필터 | 전 항목을 seq 순으로 꺼내 §4 송신 규칙으로 필터 → W=1 이면 4/5 만, W>1 이면 4/5 = 첫 항목 + 6 = 전체 |
| `viewchanger.go:492 getInFlight` | `seq == lastDecision.seq` 면 nil, `committedDuringViewChange` 특례 | 리스트 버전 `getInFlightEntries`: `seq ≤ ckpt` 제거, 연속성, ≤ W |
| `viewchanger.go:763 ValidateInFlight` | 단수: `seq == last+1` | 리스트: 첫 항목 `last+1`, 연속, `len ≤ W`; 필드 4/5 와 6[0] 일치 검사(W>1) |
| `viewchanger.go:851 CheckInFlight` | `expectedSequence = maxLastDecision+1` 하나에 A1/A2/B | `CheckInFlightPrefix`: `s = base+1, base+2, …` 순회, seq 별 A1/A2/B (§5) |
| `viewchanger.go:1174 processNewViewMsg` | `commitInFlightProposal(p)` 1회 → `ViewChanged(currView, mySequence+1)` | 접두사 길이만큼 반복(§6), 그 뒤 `ViewChanged` |
| `viewchanger.go:1252 commitInFlightProposal` | in-flight View 1개를 PREPARED 로 만들어 커밋; `<-v.Ticker` 2회(1353-1354), `checkIfTimeout`(1382) | 유지. 루프 불변식 `p.seq == checkpoint+1`; `checkpoint ≥ p.seq` 면 skip(true) |
| `controller.go:419 changeView` | `abortView → setView… → Batcher.Reset → startView` | `startView` 직전 `c.InFlight.clearAbove(newProposalSequence)` (D1, §3) |
| `util.go InFlightData` | `lowest/clearUpTo/pruneDelivered` | `clearAbove(seq)`, `entries() []…`(seq 순) 추가 |
| `controller.go:665 sync / 772 maybePruneInFlight` | `clearUpTo(synced)` | 유지 |

## 3. D1 — `clearAbove(newProposalSequence)` 와 sync 순서 (①)

**규칙.** `Controller.changeView(newViewNumber, newProposalSequence, newDecisionsInView)` 에서 `abortView` 뒤, `startView` 직전에
```go
c.InFlight.clearAbove(newProposalSequence) // D1: 새 뷰의 low 위 항목은 폐기된 접미사
```
`clearAbove(seq)` 는 **`s > seq` 만** 삭제한다. `≥` 를 지우면 baseline 의 미복구 in-flight 이월(PBFT prepared 인증서 이월, 다음 뷰 체인지에서 다시 보고)이 깨져 W=1 ViewData 바이트가 달라진다 (viewchange-trace "W=1 대조").

**인자를 쓰는 이유 (sync 순서).** `Controller.run` 의 sync 분기(`controller.go:563-575`)는 `sync()` 안에서 `Checkpoint.Set → newProposalSequence = latest+1 → maybePruneInFlight → clearUpTo(latest)` 를 끝낸 뒤 `changeView(view, newProposalSequence, dec)` 를 부른다. 따라서 `clearAbove` 가 **인자 `newProposalSequence`** 를 쓰면 순서는 `clearUpTo(S) → clearAbove(S+1)`, 남는 집합 ⊆ `{S+1}` — baseline `maybePruneInFlight` 가 "아직 유효할 수 있다" 며 남기는 항목과 정확히 같다. `c.currView.ProposalSequence` 나 `ViewSequences` 는 **옛 뷰의 low** 라 쓰면 안 된다. 호출 4곳의 인자가 모두 새 low 임을 확인함: ViewChanger `mySequence+1`(`viewchanger.go:1228`), sync `latest+1`(`controller.go:567`), sync 무결과 `ViewSequences.ProposalSeq`(`:574`, 중단된 뷰 인스턴스의 low — 그 위 항목은 그 인스턴스의 슬롯이므로 삭제가 맞음), 로테이션 `md.LatestSequence+1`(`:628`).

**테스트 (`controller_window_internal_test.go`) — `TestInFlightClearedAfterSyncAndViewChange`:**
- (a) `InFlightData = {p, p+1, p+2}`, Synchronizer 가 `latest = p+2` 를 돌려줌 → `sync()` 후 `changeView` → `InFlightData` **비어 있음**.
- (b) 같은 상태에서 `latest = p` → 남는 항목 **`{p+1}` 하나**(= 새 low; baseline 이월 의미).
- (c) W=1 대조: `{p}` 하나, `latest = p−1`(sync 가 아무것도 안 가져옴) → `{p}` 유지(no-op), ViewData 바이트 동일(`TestMessageHistogramMatchesBaseline` 가 end-to-end 로 고정).
- (d) 로테이션 경로: in-flight 없음 → no-op.

## 4. ViewData 송신 규칙 (③ proto 포함)

```proto
message InFlightEntry { Proposal proposal = 1; bool prepared = 2; }
message ViewData {
    uint64 next_view = 1;
    Proposal last_decision = 2;
    repeated Signature last_decision_signatures = 3;
    Proposal in_flight_proposal = 4;   // 유지: W=1 바이트 동일, W>1 에서는 in_flight[0] 과 같다
    bool in_flight_prepared = 5;       // 유지
    repeated InFlightEntry in_flight = 6;   // W>1 전용
}
```
`SignedViewData.raw_view_data` 는 바이트 위에 서명되고 리더가 그대로 전달하므로 필드 추가는 서명을 깨지 않는다(viewchange-trace 체크리스트 1).

송신(`prepareViewDataMsg`, 의사코드는 viewchange-trace "새 복구 규칙"):
```
entries := InFlight.entries()  // seq 오름차순
out := []
for e in entries:
    if e.seq <= ckpt: continue                       // 전달됨 (getInFlight 의 == 비교를 ≤ 로 일반화)
    if len(out)==0 && e.seq != ckpt+1: break         // 첫 항목은 ckpt+1
    if len(out)>0 && e.seq != out[-1].seq+1: break   // 연속성
    out.append(e); if len(out)==W: break
if W == 1: vd.in_flight_proposal/prepared = out[0] (또는 nil/false)      // 지금과 바이트 동일
else:      vd.in_flight_proposal/prepared = out[0]; vd.in_flight = out  // 수신측은 6 우선, 4/5 와 6[0] 일치 검사
```
`committedDuringViewChange` 특례(`getInFlight :521-530`)는 `seq ≤ ckpt` 제거로 흡수된다(viewchange-trace 열린 문제 5; `TestCommitWhileHavingInFlight` 의 기대값은 유지되는지 구현 시 확인, 바뀌면 그 테스트의 의미를 다시 정의).

수신 검증(`ValidateInFlight` 리스트 버전): `entries[0].seq == lastDecision+1`, 연속, `len ≤ W`, 각 metadata unmarshal, W>1 이면 `in_flight[0] == in_flight_proposal && prepared 일치`. W=1 노드가 W>1 노드의 ViewData 를 받는 혼합 구성은 지원하지 않음(설정 불일치; `Validate()` 는 노드 로컬이라 막을 수 없으므로 문서화만).

## 5. 복구 규칙 — seq 별 A1/A2/B 순회 (②)

`CheckInFlightPrefix(messages, f, quorum)` → `(ok bool, prefix []*protos.Proposal, err)`:
```
base := maxLastDecisionSequence(messages)            // :948 그대로
for s := base+1; ; s++ {
    candidates := {p : ∃vd, vd.entry(s)==p ∧ vd.entry(s).prepared}
    for p in candidates:
        preprepared := #{vd : vd.entry(s)==p}                                   // A2 ≥ f+1
        noArgument  := #{vd : vd.entry(s)==nil ∨ vd.entry(s)==p}                // A1 ≥ quorum
        if preprepared ≥ f+1 ∧ noArgument ≥ quorum { prefix.append(p); continue outer }   // A: 보존
    noInFlight := #{vd : vd.entry(s)==nil ∨ ¬vd.entry(s).prepared}
    if noInFlight ≥ quorum { return ok=true, prefix }                            // B: 여기서 끊고 뒤는 폐기(D1)
    return ok=false                                                             // 기존 :944 와 동일
}
```
`vd.entry(s)` = 그 ViewData 의 in_flight 중 seq==s 인 항목(없으면 nil; W=1 은 4/5 를 길이 1 리스트로 취급). W=1 이면 `s = base+1` 한 번만 돌고 기존 `CheckInFlight` 와 같은 판정(Lemma 5). 상한: `s ≤ base+W` 를 넘으면 B 로 간주(정직 노드의 in-flight 는 ≤ W).

안전성은 viewchange-trace Lemma 1–4 (seq 별 A/B 배타성, 전달의 접두사성, 보존 충분성, 폐기 안전성) 그대로. **D2(prepare 선행 조건)** 는 feat 4 가 구현했다(`view.go stepWindow` 의 접두사 처리, `TestViewFollowerPreparePrecondition`) — 이 문서에서는 "s 의 prepare 는 유실, s+1 의 commit 은 완성" 상황을 s 의 pre-prepare 유실로 좁혀 주는 보조 가정으로만 참조한다.

## 6. 접두사 커밋 반복과 접미사 파기 (②)

`processNewViewMsg`(`:1174`) 의 `1190-1202` 대체:
```
ok, prefix := CheckInFlightPrefix(authenticated, f, quorum)
if !ok { return }
for _, p := range prefix {
    ck := checkpoint.seq
    if p.seq <= ck { continue }            // 루프 중 sync/deliver 로 이미 커밋
    if p.seq != ck+1 { return }            // 불변식 위반(sync 점프) → 뷰 변경 포기, 재시도 대기
    if !commitInFlightProposal(p) { return }   // 타임아웃·sync·stop → 기존과 동일하게 뷰 안 바꿈
}
mySequence := checkpoint.seq ; NewView 레코드 Save ; ViewChanged(currView, mySequence+1)
```
접미사 파기(D1)는 §3 의 `changeView` 가 `ViewChanged → changeView(newView, mySequence+1)` 경로에서 수행한다 — `processNewViewMsg` 안에 별도 clear 를 두지 않는다(모든 뷰 시작 경로를 한 곳에서 덮기 위해). `commitInFlightProposal` 의 `:1277 Panicf`(체크포인트 불일치)는 루프 중 외부 sync 로 체크포인트가 점프한 경우를 위해 "`checkpoint ≥ p.seq` 면 skip(true)" 로 완화.

**타임아웃 예산** (viewchange-trace 열린 문제 3): 반복마다 `<-v.Ticker` 2회 + quorum 대기, `checkIfTimeout` 은 `startViewChangeTime` 기준. 1차: `ViewChangeTimeout` 을 바꾸지 않고 **접두사 1개 커밋마다 소요 시간을 로그·메트릭(`viewchange_prefix_commit_seconds`)으로 측정**해 W=8 에서 기본 20 s 안에 드는지 확인. 넘으면 "접두사 하나 커밋마다 타이머 재시작" 보다 `ViewChangeTimeout` 을 W 비례로 올리는 쪽을 택한다(전자는 바이잔틴 리더의 시간 끌기 공격면).

**in-flight View 의 sync 트리거** (열린 문제 2): 접두사 k+1..k+m 을 순차 커밋하는 동안 앞선 노드의 `k+m+1` Commit 이 `discoverIfSyncNeeded` 로 들어가 `syncInFlight` 를 부를 수 있다. feat 4 의 View 는 `[low, low+W]` 를 수용하므로 in-flight View 에 `PipelineDepth = W` 를 주면 `k+m+1 ≤ k+1+W` 인 Commit 은 슬롯에 보관되고 sync 로 빠지지 않는다 → in-flight View 생성(`:1288-1320`)에 `PipelineDepth` 를 전달. 구현 시 `TestMultiInFlightThenViewChange` 로 확인.

## 7. 뒤처진 새 리더 (④)

문제(viewchange-trace 열린 문제 1): `checkLastDecision`(`:616`) 은 `lastDecision.seq > mySequence+1` 인 ViewData 를 거절하고, `validateNewViewMsg`(`:1051`) 는 같은 조건에서 `Synchronizer.Sync()` 를 부른다. 윈도우에서는 정직 노드 간 체크포인트 차가 최대 W 라 2 이상 뒤처진 새 리더가 생길 수 있다.

**1차안(확정, 2026-11-02): 변경 없음 — sync 후 뷰 체인지 재시작.** 뒤처진 리더는 quorum 을 못 채워 `ViewChangeTimeout` 뒤 다음 뷰로 넘어가거나(리더 교체), 그 사이 하트비트/`discoverIfSyncNeeded` 로 sync 되어 따라잡은 뒤 ViewData 를 다시 받는다. PBFT 표준 처리이며 안전성 영향 없음, 라이브니스 지연만.

**테스트 `TestLaggingLeaderSyncsThenViewChanges`** (`test/pipeline_window_test.go`, W=3): 노드 2(다음 리더)가 commit 을 잃어 체크포인트가 2 이상 뒤처진 상태에서 뷰 체인지 유발(노드 1 하트비트 중단). `ViewChangeTimeout` 을 수 초로 단축. 확인: (i) 노드 2 로그 "last decision seq %d is greater than this node's current sequence" → `Synchronizer returned with sequence` → 이후 뷰 체인지 완료("Processed a new view"/`ViewChanged`); (ii) **연쇄 뷰 체인지 없음**: 최종 뷰 번호 == 시작 뷰 + 1 (또는 +2 까지 허용하되 로그로 기록), 모든 노드 동일 레코드. `ViewChangeTimeout` 과 sync 시간이 겹치면 +2 가 날 수 있으므로 1차는 "≤ +2, 안전성 유지" 를 단언하고 실제 횟수를 결과에 기록(측정 항목).

## 8. feat 5 접합부 — 복원 슬롯과 `InFlightData` (⑤)

**질문:** 재시작으로 복원된 슬롯(`RestoredCount`)이 뷰 체인지 중이면 `InFlightData` 에 반영되는가. 복원 슬롯이 ViewData 에 실려야 하는데 `Restore` 가 `InFlightData` 도 채우는지, View 슬롯만 채우는지.

**답: `InFlightData` 도 채운다(전자).** 코드 위치(`aab360af`):
- `state.go:247-251` (`Restore`): 복원 항목마다 `ps.storeProposal(r.proposed)`; commit 레코드가 있으면 `ps.storePrepared(r.commit)`. `storeProposal` 은 `InFlightProposal.StoreProposalAt(seq, …)`(`:58-69`), `storePrepared` 는 `StorePrepares(view, seq)`(`:70-73`) — 이 `InFlightProposal` 은 `consensus.go:140-152` 에서 `c.inFlight` 로 `PersistedState`·`Controller`·`ViewChanger` 가 **공유**한다. 즉 View 슬롯(`v.restored` → `setup()`)과 `InFlightData` 가 같은 집합으로 채워진다(`TestRestoreFoldsWindow` 가 `InFlightProposal()/IsInFlightPrepared()` 로 최저 항목을 확인).
- **시점:** `Restore` 는 `ProposalMaker.NewProposer`(`util.go:405-410`, `restoreOnceFromWAL`) 에서 `Controller.Start → startView` 때 1회 실행된다(`controller.go:926`). `ViewChanger.run` 은 `ControllerStartedWG.Wait()`(`viewchanger.go:169`) 로 `controller.go:938 StartedWG.Done()`(startView 뒤) 까지 기다리므로, **어떤 ViewData 도 `Restore` 가 `InFlightData` 를 채운 뒤에 만들어진다.** `consensus.go:554-559` 의 시작 순서(`collector.Start → viewChanger.Start → controller.Start`)와 무관하게 성립.
- **재시작 직후 뷰 체인지(VC 레코드 복원)**: `setViewAndSeq`(`consensus.go:511-535`) 가 `LoadViewChangeIfApplicable` 로 `viewChanger.Restore` 채널을 채우면 `run` 이 `processViewChangeMsg(restore=true)` 로 ViewData 를 보내는데, 이 역시 `ControllerStartedWG.Wait()` 뒤다. 단, 그 경우 WAL 마지막 레코드가 VC 라 feat 5 `Restore` 는 **복원 없음**(baseline 과 동일, design-state-wal-window §5-1) → ViewData 는 in-flight 없음으로 나간다. 이것은 baseline 의 알려진 동작(viewchange-trace 체크리스트; upstream #748 이 이 경로를 바꿨음 — §10)이며 feat 6 범위에서 유지.
- **feat 6 이 바꿀 것:** 없음(접합은 이미 성립). `prepareViewDataMsg` 가 `entries()` 로 전 항목을 읽으면 복원 슬롯 m 개가 그대로 실린다. 테스트 `TestPipelineRestartThenViewChangeCarriesRestored`: 팔로워가 2개 복원 직후 뷰 체인지 → 그 노드의 ViewData `in_flight` 길이 2(prepared 플래그 포함) 를 로그/훅으로 확인.

## 9. W=1 동일성

| 측면 | 근거 |
|---|---|
| ViewData 바이트 | W=1 은 필드 6 을 채우지 않고 4/5 만 — `prepareViewDataMsg` 의 W 분기. `TestMessageHistogramMatchesBaseline` 가 view-data/new-view 정확 일치로 고정 |
| 복구 판정 | `CheckInFlightPrefix` 가 `s=base+1` 1회 → 기존 A/B 와 동일(Lemma 5) |
| 접두사 커밋 | 길이 ≤ 1 → `commitInFlightProposal` 1회 |
| D1 | 단일 항목은 항상 `≤ low` → `clearAbove` no-op |
| 뒤처진 리더 | 변경 없음 |

## 10. upstream 동기화 메모 (feat 6 전에 결정)

upstream main 은 baseline 뒤 4 커밋(#744 abort the view directly, #745/#746 hold the view while view changer/sync writes, #748 restore in-flight kept by a view record). #748 은 `Restore` 가 마지막 레코드가 NV/VC 일 때 그 앞의 PP(+C)를 `InFlightData` 에만 복원한다 — feat 5 의 "NV/VC 가 마지막이면 복원 없음" 과 **충돌**한다(우리는 D1 로 경계 뒤만 복원). sync/upstream 브랜치에서 병합할 때 결정 필요: (권고) #748 의 의도("VC 레코드 뒤에 보존된 in-flight 를 다음 뷰 체인지에 싣기")는 feat 6 의 ViewData 에도 유효하므로 **"VC 가 마지막이면 그 앞 경계 이후의 접두사를 `InFlightData` 에만 복원(View 는 비움)"** 으로 fold 규칙을 확장한다. NV 가 마지막이면 새 뷰가 시작된 것이므로 복원 없음 유지. 이 결정은 feat 6 설계 리뷰에서 함께 받는다.

## 11. 테스트 계획

단위 (`internal/bft`): `TestInFlightClearedAfterSyncAndViewChange`(§3 a–d), `TestInFlightDataClearAbove`, `TestCheckInFlightPrefix`(접두사 0/1/2/3, 중간 B, A 실패 → ok=false, W=1 회귀 = 기존 `TestCheckInFlight*` 케이스 재사용), `TestValidateInFlightList`, `TestPrepareViewDataMultiInFlight`(W=1 바이트 동일 포함), `TestProcessNewViewCommitsPrefix`(2개 순차 커밋, 중간 sync 점프 시 포기).

통합 (`test/`): `TestMultiInFlightThenViewChange`(W=3, 3개 in-flight 중 리더 정지 → 새 뷰가 접두사 커밋 → 동일 레코드), `TestMultiInFlightLeaderCrashThenRecovers`, `TestLaggingLeaderSyncsThenViewChanges`(§7), `TestPipelineRestartThenViewChangeCarriesRestored`(§8), `TestMessageHistogramMatchesBaseline` 무변경.

## 12. 커밋 순서

a. `util, controller: InFlightData.clearAbove and D1 in changeView` (+ §3 테스트) — W=1 no-op.
b. `proto, viewchanger: repeated in_flight in ViewData, list validation, multi-entry sender` — W=1 바이트 동일.
c. `viewchanger: per-sequence prefix check and prefix commit loop` (+ in-flight View depth).
d. `test: multi in-flight view change, lagging leader, restart then view change`.

## 13. 열린 질문 (리뷰어 결정 요청; 괄호는 권고)

1. §6 타임아웃: 1차는 측정만, `ViewChangeTimeout` 불변. (권고: 그대로)
2. §7 연쇄 뷰 체인지 단언 범위: `≤ +2` 허용·기록 vs `== +1` 엄격. (권고: ≤ +2 기록, 측정 항목)
3. §10 #748 과의 조화: VC 마지막이면 `InFlightData` 만 복원. (권고: 채택, sync/upstream 브랜치에서)
4. §4 혼합 W 구성 비지원 문서화만. (권고: 그대로)
