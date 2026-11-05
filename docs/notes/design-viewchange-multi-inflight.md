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

---

## 14. 설계 리뷰 1차 반영 (2026-11-05, 7라운드) — 체크리스트 순

리뷰어 체크리스트(우선순위순): (1) S1/S2/S3 ↔ Lemma ↔ 코드 분기 대응표 + A2 의 f+1 논증, (2) 단일 경계 원칙 자료구조별 표 + 뷰 체인지 중 재시작 테스트, (3) 뒤처진 새 리더 테스트 4종, (4) feat 5 접합: 복원 슬롯 → `InFlightData` 등록 경로와 일치 검증, (5) proto 필드 6 + 혼합 배포 거부. 아래 §14.1–14.5 가 각각 대응한다. §3–§8 의 본문은 그대로 두고 이 절이 우선한다(충돌 시 이 절).

### 14.1 안전성 명제 S1/S2/S3 과 Lemma·코드 분기 대응

명제(리뷰어 수정안):
- **S1 (결정 보존)**: 어떤 정직 노드가 Deliver 한 seq s 의 값 v 는 새 뷰에서도 s = v 로 복구된다.
- **S2 (결정 가능 값 보호)**: 정직 노드 하나라도 s 에 대해 prepared 인증서(2f+1 prepare)를 가진 값 v 가 있으면 새 리더는 s 에 v 이외의 값을 제안할 수 없다. 단 어떤 값도 prepared 가 아니라는 증거(B: `noInFlight ≥ quorum`)가 있으면 s 를 비워도 된다.
- **S3 (순서·비확정)**: 복구된 접두사는 checkpoint+1 부터 연속이며, A/B 어느 쪽 증거도 없는 slot 은 확정하지 않고 실패(재시도)한다.

| 명제 | 담당 Lemma (viewchange-trace) | 코드 분기 (`aab360af` → feat 6) | 논증 요지 |
|---|---|---|---|
| S1 | Lemma 2 (전달의 접두사성) + Lemma 3 (보존 충분성) | `maxLastDecisionSequence`(`viewchanger.go:948`): base = 정족수 ViewData 의 최대 last_decision seq. s ≤ base 인 전달값은 `last_decision` 자체로 운반되고 뒤처진 노드는 `validateNewViewMsg :1040-1100` 의 "ahead by one → ValidateLastDecision → deliver" 또는 sync 로 받는다. s > base 이면서 어떤 정직 노드가 전달했다면 그 노드는 2f+1 commit 을 봤고 그중 ≥ f+1 정직 노드가 WAL 에 PP+C 를 기록해 `prepared=true` 로 보고(`state.go Save → storePrepared`) → A2 성립 → `CheckInFlightPrefix` 가 s 를 그 값으로 보존 → `commitInFlightProposal` 이 같은 값으로 커밋 | 정직 노드가 전달한 값은 (i) base 이하면 last_decision 으로, (ii) base 초과면 A 로 보존된다. 두 정직 노드가 같은 s 에 다른 값을 전달하지 않음(Lemma 2 + Lemma 4) |
| S2 | Lemma 1 (seq 별 A/B 배타성) + Lemma 4 (폐기 안전성) | **A2** `preprepared ≥ f+1` (`CheckInFlight :924`, 접두사 버전 §5), **A1** `noArgument ≥ quorum` (`:927`), **B** `noInFlightCount ≥ quorum` (`:940`) | 아래 "A2 의 f+1 논증". A 가 성립하는 값은 유일(Lemma 초안 4: 같은 (view, seq) 에 정직 노드는 한 번만 prepare). A 와 B 는 배타적(Lemma 1): prepared 를 든 정직 노드가 f+1 명이면 noInFlight ≤ (2f+1) − (f+1) + f = 2f < quorum. 따라서 B 로 비운 s 에는 정직 prepared 인증서가 f+1 개 미만 — "정직 노드 하나라도" 는 아래 보강 참조 |
| S3 | Lemma 5 (W=1 환원) 의 전제 + 의사코드의 `return ok=false` | 송신: `getInFlightEntries` 연속성·`ckpt+1` 시작(§4); 수신: `ValidateInFlight` 리스트(첫 항목 `last+1`, 연속, `≤ W`); 판정: `CheckInFlightPrefix` 가 A 도 B 도 아니면 `ok=false`(`:944` 와 동일) → `processNewViewMsg :1193-1196` "did not pass" → 뷰 바꾸지 않고 재시도/타임아웃; 접두사 루프의 `p.seq != ck+1 → return`(§6) | 접두사는 base+1 부터 A 가 연속으로 성립하는 구간, 첫 B 에서 중단. 증거 없는 slot 은 확정하지 않는다 |

**A2 의 f+1 논증 (S2 "정직 노드 하나라도").** 정직 노드 h 가 s 에 대해 값 v 의 prepared 인증서(2f+1 prepare)를 가졌다고 하자. prepare 를 보낸 2f+1 노드 중 ≥ f+1 은 정직하고, 정직 노드는 prepare 를 보내기 전에 PP(s, v) 를 WAL 에 기록해(`view.go processProposal :630 Save(pp)`) `InFlightData` 에 v 를 올린다(`state.go storeProposal`). 새 리더가 모은 정족수(2f+1) ViewData 중 그 f+1 정직 노드와 겹치는 것은 ≥ (f+1) + (2f+1) − (3f+1) = 1 개 … 즉 **최소 1개의 정직 ViewData 가 s 에 v 를 (prepared 플래그 유무와 무관하게) 보고한다.** 그러나 A2 는 `preprepared ≥ f+1` 을 요구하므로 "정직 노드 하나" 만으로는 A 가 성립하지 않을 수 있다. 그때 성립하는 것은 B 인가? B 는 `noInFlight ≥ 2f+1` 인데 noInFlight 에는 "s 에 v 를 들었지만 prepared 가 아닌" 정직 노드도 들어간다. 따라서 **S2 의 "정직 노드 하나라도 prepared 인증서를 가짐" 은 A 를 보장하지 않고, B 도 배제하지 않는다** — 이것은 baseline `CheckInFlight` 의 성질 그대로이며, PBFT 와 같은 근거로 안전하다: *정직 노드가 prepared 인증서를 가졌다는 것만으로는 어떤 정직 노드도 s 를 전달하지 않았을 수 있고(commit 정족수 미달), 그 경우 s 를 다른 값으로 결정해도 전달 기준 안전성(S1)은 깨지지 않는다.* S2 가 실제로 보호해야 하는 것은 "**어떤 정직 노드가 commit 정족수(2f+1 commit)를 모아 전달했을 수 있는 값**" 이고, 그 경우 ≥ f+1 정직 노드가 prepared 로 보고하므로 A2 가 성립한다(위 S1 행). → **S2 의 문구를 "정직 노드 f+1 개 이상이 s 에 대해 prepared 인 값 v 가 있으면(= 어떤 정직 노드가 commit 정족수를 모았을 수 있는 최소 조건) 새 리더는 v 이외의 값을 제안할 수 없다; 그보다 약한 증거(prepared 노드 ≤ f)인 값은 B 로 비울 수 있고 그래도 S1 은 유지된다" 로 정정할 것을 제안한다.** 이 정정이 받아들여지지 않으면 A2 를 `≥ 1` 로 낮춰야 하는데, 그러면 바이잔틴 노드 하나가 임의의 값을 prepared 로 보고해 새 리더의 제안을 막을 수 있어(라이브니스 공격) baseline 의 설계와 어긋난다. 리뷰어 결정 요청(§15 질문 5).

**각 Lemma 가 성립하는지는 구현 뒤 `TestCheckInFlightPrefix` 의 케이스 표(A 유일성, A/B 배타, B 중단, 증거 없음 → ok=false, f+1 바이잔틴 prepared 보고 → A 불성립·B 성립)로 기계적으로 확인한다**; 이 절은 요구 사항이며 성립 판단은 그 테스트와 문서를 함께 보고 한다.

### 14.2 단일 경계 원칙 — 자료구조별 표와 뷰 체인지 중 재시작

**원칙.** 뷰 체인지 뒤 아래 자료구조 모두가 같은 seq 경계 `newProposalSequence`(= 접두사 커밋 뒤 `checkpoint+1`, `viewchanger.go:1228 ViewChanged(currView, mySequence+1)`) 를 따른다: 그 아래는 전달됨, 그 위는 존재하지 않음(폐기), 경계 자체는 "다음에 제안될 seq" 로 비어 있거나 baseline 이월 항목 1개.

| 자료구조 | 소유 | 경계를 따르게 하는 코드 | 상태 |
|---|---|---|---|
| `InFlightData.entries` | `Consensus.inFlight`(공유) | `Controller.changeView` 의 `startView` 직전 `clearAbove(newProposalSequence)`(§3, 신규); 그 아래는 `decide → pruneDelivered`(`controller.go:589`) 와 `sync → clearUpTo`(`:797`) 가 지움 | feat 6 추가 |
| `View.slots`(투표·검증 서명·pre-prepare·proposal) | 옛 View 인스턴스 | `abortView → currView.Abort()`(`controller.go:455-466`, `view.go:1317`) 로 뷰 고루틴 종료 → 인스턴스 폐기; 새 View 는 `setup()` 에서 빈 `slots` 로 시작(`view.go:246`) | 자동 충족 |
| `View.incMsgs` 큐(미처리 메시지) | 옛 View 인스턴스 | 채널은 인스턴스와 함께 버려짐; `Controller.ProcessMessages`(`controller.go:329`) 는 항상 `c.currView` 로 라우팅하므로 새 뷰는 자기 큐만 본다. 옛 뷰 번호의 메시지는 새 View 의 `processMsg :325` "expected view" Warn 으로 폐기 | 자동 충족 |
| `verifiedSigs` 채널 | 옛 View 인스턴스 | 송신 고루틴이 `abortChan` 에서 포기 | 자동 충족 |
| 복구 캐시 `inFlightAttempt`/`inFlightView` | ViewChanger | `commitInFlightProposal :1360-1364` 가 커밋·타임아웃 후 `inFlightView = nil`; `decideInFlight :1396 attempt.viewRef.stop()`. 접두사 루프에서는 반복마다 새 attempt 를 만들고 이전 것을 nil 로 → 루프 종료 시 nil | 유지(루프화만) |
| 요청 풀 예약 | RequestPool | `startView :393 UnreserveAll()` — 폐기 슬롯의 요청은 예약 해제되어 새 뷰가 재제안 | 자동 충족 (feat 4) |
| WAL 재생 결과 | PersistedState | `Restore`(feat 5): 마지막 NewView/ViewChange 레코드 **뒤**만 fold → 경계 레코드가 있으면 옛 뷰 슬롯은 복원되지 않음. NV 레코드는 `processNewViewMsg :1207-1216` 에서 접두사 커밋 뒤 `LatestSequence = mySequence` 로 기록되므로 경계 = `newProposalSequence − 1` 과 일치 | **조건부** — 아래 |
| `ViewSequences`(하트비트·sync 기준 low) | atomic | `NewProposer :400`/`advanceLow` 가 새 뷰의 low 로 발행 | 자동 충족 |

**뷰 체인지 중 재시작 (WAL 경계가 옛 뷰에 있는 경우).** 리뷰어 지적대로 "NewView 경계 뒤만 읽으니 자동" 은 NV 레코드가 기록된 뒤에만 성립한다. 경우별:

| 재시작 시점 | WAL 꼬리 | feat 5 `Restore` 결과 | 판정 |
|---|---|---|---|
| (a) 뷰 v 진행 중, 뷰 체인지 시작 전 | `[PP(p) v, PP(p+1) v, C(p) v]` | p(PREPARED), p+1(PROPOSED) 복원 → View 슬롯·`InFlightData` | 정상 (in-flight 그대로) |
| (b) **ViewChange 전송 후·NewView 수신 전** — VC 레코드 있음 | `[…, VC(next_view = v)]` (※ `processViewChangeMsg :440-446` 은 `v.currView` 를 갱신하기 **전**에 저장하므로 레코드의 `next_view` 는 떠나는 뷰 v) | 마지막이 VC → **복원 없음**(baseline 동일, §5-1). `InFlightData` 비어 있음 → 재시작 뒤 `viewChanger.Restore` 채널로 재전송되는 ViewData 는 in-flight 없음 | 접미사를 보고하지 않음(안전, 요구 충족). 단 **접두사(prepared p)도 잃어** A2 를 채우는 정직 노드가 하나 줄어든다 — 라이브니스 비용. upstream #748 이 이 경우를 "VC 앞 PP(+C) 를 `InFlightData` 에만 복원" 으로 고침 → §10/§15-3 |
| (c) 뷰 체인지 시작 전에 재시작했고 재시작 직후 뷰 체인지 참여 (VC 레코드 없음) | (a) 와 같음 | p, p+1 모두 `InFlightData` 에 → ViewData 에 둘 다 보고 | **올바른 입력**: ViewData 의 in-flight 는 A/B 판정의 입력이고, 접미사는 판정 결과(B) 이후에 생기는 개념. D1 은 판정 뒤 `changeView` 의 `clearAbove` 가 적용 |
| (d) NewView 수신·NV 레코드 기록 후, 새 뷰 첫 PP 처리 전 | `[PP(p) v, PP(p+1) v, C(p) v, NV(v+1, latest = p)]` | 마지막이 NV → 복원 없음; `LoadNewViewIfApplicable` 이 (v+1, p) 로 시작 | 옛 접미사 p+1 보고 안 함 (충족) |
| (e) 같은 뷰 번호로 뷰 인스턴스 재시작(sync 무결과 경로 `controller.go:574`) 뒤 재시작 | NV 없음, 옛 인스턴스의 `PP(p+1) v` 남음 | 뷰 번호가 같아 "다른 뷰 레코드 Warn" 규칙에 **걸리지 않음** → p+1(옛 인스턴스) 복원 | **주의**: 새 인스턴스가 (v, p+1) 에 다른 배치를 제안하면 복원 노드는 옛 PP 를 들고 있어 충돌. baseline 도 W=1 에서 같은 구조(sync 무결과 → 같은 뷰·같은 seq 재제안 → "dropping message")를 가짐. W>1 에서 노출이 늘어나므로 **feat 6 테스트 항목으로 기록**, 수정은 별도(§15-6) |

**테스트 `TestRestartDuringViewChangeReportsNoSuffix`** (`test/`, W=3): 뷰 0 에서 p..p+2 in-flight(commit 보류) → 리더 정지로 뷰 체인지 → 노드 3 이 ViewChange 를 보낸 직후(로그 "sent view data msg" 전후 두 변형) 재시작 → (i) 노드 3 의 `Restore` 로그가 "Restored proposal" 을 내지 않음(경우 b) 또는 복원했더라도 뷰 체인지 완료 후 `InFlightData` 가 `> newProposalSequence` 를 갖지 않음(경우 c, 컨트롤러 훅/메트릭으로 확인) → (ii) 모든 노드 동일 레코드. 경우 (e) 는 `TestSameViewRestartKeepsOldPrePrepare` 로 현상을 기록만(실패 조건 없음, 로그 카운트).

### 14.3 뒤처진 새 리더 — 테스트 4종 (병합 기준)

| # | 테스트 | 시나리오 (W=3, N=4) | 단언 |
|---|---|---|---|
| L1 | `TestLaggingLeaderSyncsThenViewChanges` | 다음 리더(노드 2)가 commit 을 잃어 체크포인트 2 이상 뒤처짐 → 뷰 체인지 | 노드 2 로그 "last decision seq … greater than this node's current sequence" → `Synchronizer returned with sequence` → 뷰 체인지 완료; 전 노드 동일 레코드 |
| L2 | `TestLaggingLeaderViewChangeTimeoutOverlap` | L1 + `ViewChangeTimeout` 수 초로 단축, sync 지연(`DelaySync`) 을 타임아웃 근처로 | 최종 뷰 ≤ 시작+2, 안전성(동일 레코드) 유지, 실제 뷰 증가 횟수를 로그로 기록(측정 항목) |
| L3 | `TestRepeatedLeaderChangesKeepDecisions` (**S1 실험 검증**) | 3개 in-flight 상태에서 리더 정지 → 뷰 1 → 접두사 커밋 중/직후 다시 리더 정지 → 뷰 2 → 뷰 3 까지 반복 | 뷰마다 Deliver 된 (seq, 값) 이 모든 노드에서 동일하고 한 번 전달된 seq 의 값이 뒤 뷰에서 바뀌지 않음(각 노드 `Delivered` 기록 비교); 접두사 커밋 로그 횟수 기록 |
| L4 | `TestLaggingLeaderWithRestoredInFlight` | 노드 2 가 2개 복원(feat 5) 직후 뒤처진 채 새 리더가 됨 | 복원 슬롯이 자기 ViewData 에 실리고(§14.4), sync 뒤 뷰 체인지가 완료되며 복원 값이 전달값과 일치 |

(리뷰어의 "네 가지 확인 항목" 을 위 4종으로 해석했다 — 다르면 지적 요청.)

### 14.4 feat 5 접합 — 복원 슬롯의 `InFlightData` 등록 경로와 일치 검증

등록 경로(`aab360af`): `Restore`(`state.go:243-251`) → 복원 항목마다 `ps.storeProposal(r.proposed)` → `InFlightData.StoreProposalAt(pp.seq, proposal)`(`state.go:58-69`, `util.go:267`) ; commit 레코드가 있으면 `ps.storePrepared(r.commit)` → `InFlightData.StorePrepares(commit.view, commit.seq)`(`state.go:70-73`, `util.go:282`). 같은 `InFlightData` 를 ViewChanger 가 `prepareViewDataMsg :469` 에서 읽는다. 시점 보장은 §8(`ControllerStartedWG`).

**등록 시 검증 (feat 6 요구 사항, 신규):** `foldSlots`/`filterInFlight`(feat 5) 에 다음을 추가하고 불일치는 **Warn 후 해당 seq 를 PROPOSED 로 강등(commit 증거 무시)**, proposal 자체가 깨졌으면 그 seq 이후를 버린다:
1. `commit.view == pp.view`, `commit.seq == pp.seq` (현재 seq 만 키로 맞춤; view 는 미검증).
2. `commit.digest == proposal.Digest()` — 내 commit 이 그 제안에 대한 것인지(prepared 증거의 대상 일치).
3. `commit.signature.signer == SelfID` — 내가 쓴 commit 레코드인지.
4. `pp.view == 복원 집합의 view`(feat 5 의 뷰 불일치 규칙 그대로), `pp.proposal.metadata.{view_id, latest_sequence} == (pp.view, pp.seq)`.
5. `InFlightData.StorePrepares(view, seq)` 가 `view` 인자를 실제로 검사: 항목의 제안 metadata view 와 다르면 Warn 후 무시(`util.go:282-297` 현재는 view 를 쓰지 않음).

테스트: `TestRestoreRejectsMismatchedCommit`(digest 불일치 → PROPOSED 강등 + Warn, view 불일치 → 무시), `TestPipelineRestartThenViewChangeCarriesRestored`(§8).

### 14.5 proto 필드 6 과 혼합 배포 거부 (확정)

혼합 배포(W=1 노드와 W>1 노드 공존)는 **지원하지 않는다**. `Config.Validate` 는 상대 노드 설정을 모르므로 막을 수 없고, `validateViewDataMsg`(`viewchanger.go:534-562`, `ValidateInFlight` 호출 직전) 에서 거부 + Warn:
```
myW := depth
if myW == 1 && len(vd.in_flight) > 0            → reject: "view data carries a pipelined in-flight list but this node runs without a pipeline"
if myW > 1 && vd.in_flight_proposal != nil && len(vd.in_flight) == 0 → reject: "view data carries a single in-flight proposal without the pipelined list"
```
(W>1 노드의 in-flight 없음은 4/5/6 모두 비어 있어 두 조건에 걸리지 않는다.) 같은 검사를 `validateNewViewMsg` 의 ViewData 순회에도 둔다. 논문: "동일 설정 배포 가정" 한 줄. 테스트: `TestViewDataMixedDepthRejected`.

## 15. 열린 질문 (갱신)

1. §6 타임아웃: 1차 측정만. (권고: 그대로)
2. §14.3 L2 연쇄 뷰 체인지 단언 ≤ +2 기록. (권고: 그대로)
3. §10/§14.2(b) upstream #748 조화: VC 가 마지막이면 그 앞 경계 이후 접두사를 `InFlightData` 에만 복원(View 비움), view 검사는 `VC.next_view == 레코드 view`(VC 레코드의 `next_view` 가 떠나는 뷰임에 주의). feat 6 에 포함할지 sync/upstream 에서 할지. (권고: **feat 6 에 포함** — (b) 의 라이브니스 손실이 접두사 복구의 A2 정족수에 직접 영향)
4. §14.5 혼합 배포 거부. (확정)
5. **§14.1 S2 문구 정정**: "정직 노드 하나라도 prepared" → "정직 노드 f+1 개 이상 prepared(= commit 정족수 가능 최소 조건)". A2 를 1 로 낮추면 바이잔틴 1개가 라이브니스를 막음. (권고: 정정 채택, 원문 S2 는 S1 로 흡수)
6. §14.2(e) 같은 뷰 번호 인스턴스 재시작 뒤 옛 PP 충돌: baseline 에도 있는 구조, W>1 에서 노출 증가. feat 6 에서는 테스트로 현상만 기록하고 수정은 후속(`fix/`)으로. (권고: 그대로)

## 16. 설계 리뷰 2차 결정 (2026-11-05, 8라운드) — 구현 착수

**결정 1 — S2 정정 승인, 문구는 "커밋 가능성" 기준.**

> **S2 (커밋 가능 값 보호):** 어떤 정직 노드가 seq s 에 대해 값 v 를 커밋했을 가능성이 있으면 — 즉 v 의 prepared 인증서를 가진 정직 노드가 f+1 개 이상이면 — 새 리더는 s 에 v 이외의 값을 제안할 수 없다. 2f+1 개 ViewData 중 그런 정직 노드가 적어도 하나 포함되므로 v 는 보고되고, 또한 B(`noInFlight ≥ 2f+1`)는 성립할 수 없으므로 s 가 비워지지 않는다.

§14.1 표 정정: **S2 는 A2 가 아니라 B 의 불성립으로 보장된다** — 정직 f+1 개가 prepared 면 noInFlight 를 보고할 수 있는 노드는 최대 (3f+1) − (f+1) = 2f 개라 정족수 2f+1 에 못 미친다(핵심 lemma, Lemma 1 의 B 쪽 절반). **A2(`preprepared ≥ f+1`) 의 역할은 L(라이브니스)**: 바이잔틴 f 개가 거짓 in-flight 를 보고해도 새 리더가 그 값으로 재제안을 강제당하지 않는다. **A2 도 B 도 아닌 경우**(prepared 보고 < f+1 이고 noInFlight < 2f+1)는 S3 대로 실패·재시도이며, 이때 라이브니스는 뷰 체인지 재시도가 **다른 2f+1 집합**을 모으는 데 의존한다(같은 집합이 반복되면 진행 불가 — `ViewChangeTimeout` 뒤 다음 리더로 넘어가 집합이 바뀜).

| 명제 | 보장하는 조건·분기 | 역할 |
|---|---|---|
| S1 | Lemma 2+3; `last_decision`/`maxLastDecisionSequence` + A 보존 | 안전성 |
| S2 | **B 불성립** (정직 f+1 prepared ⇒ noInFlight ≤ 2f) — Lemma 1 | 안전성 |
| A2 ≥ f+1 | 거짓 in-flight 로 재제안 강제 불가 | **L (라이브니스)** |
| S3 | 연속성·`ckpt+1` 시작; A·B 모두 아님 → `ok=false` 재시도 | 비확정 |

**결정 2 — #748 방식 포함 승인 (안전성 사안).** VC 레코드가 마지막이면 그 앞의 접두사를 **`InFlightData` 에만** 복원(View 슬롯 복원 없음). §14.2(b) 의 "접두사 prepared 증거 손실" 은 그 노드가 f+1 중 하나였으면 재시작 후 prepared 를 보고하지 않아 B 불성립 논증의 정족수 계산이 틀어지는 **S2 위반 가능성**이므로 feat 6 범위. **주의(발견 1):** VC 레코드의 `next_view` 는 떠나는 뷰 번호(`processViewChangeMsg` 가 `currView` 갱신 전 저장)이므로, "VC 앞의 접두사" 필터는 **VC 레코드의 `next_view` 와 같은 view 의 PP/C 레코드**로 잡는다(`+1` 어긋남 금지). 테스트: `TestRestoreAfterViewChangeRecordKeepsPrefixInFlight` — 뷰 v 에서 prepared → VC 기록 → 재시작 → ViewData 가 뷰 v 의 prepared 를 보고.

**나머지 확인:** L1–L4 해석 맞음, L3 병합 기준. `StorePrepares` view 미검사는 등록 검증 5항으로 충분(upstream 이슈 불필요). 발견 3(같은 뷰 번호 재시작 충돌)은 기록·후속 fix; L1–L4 중 재현되면 `fix/` 로.

**병합 기준(확정):** 지난 체크리스트 5개(§14) + L3 + #748 테스트 + 혼합 배포 거부 테스트.

## 17. 구현 메모 (2026-11-05, `feat/viewchange-multi-inflight`)

- **A2 의 정확한 의미 (코드 기준).** `CheckInFlight`(baseline)와 `CheckInFlightPrefix` 는 seq s 의 **후보**를 "prepared 로 보고된 제안" 으로 잡고, 후보 p 의 `preprepared` 는 **p 를 보고한 노드 수(prepared 플래그 무관)**, `noArgument` 는 "p 를 보고했거나 s 에 아무것도 없다고 한 노드 수" 다. 즉 A2 ≥ f+1 은 "f+1 노드가 그 pre-prepare 를 받았다" 이고, 후보 조건이 "최소 1개 prepared 보고" 다. §14.1 표의 A2 설명("prepared ≥ f+1")은 이 의미로 읽어야 한다. S2 의 B 불성립 논증은 영향 없음(prepared 정직 f+1 ⇒ 그들 모두 present&prepared ⇒ noInFlight ≤ 2f). 단위 테스트 `TestCheckInFlightPrefix` 의 "an unprepared report of the same proposal counts as pre-prepared" 케이스가 이를 고정한다. 연속성 검증 때문에 "s 는 B 인데 s+1 은 모두 prepared" 는 메시지로 만들 수 없고, 실제 B 중간 케이스는 "단독 노드만 s 이상을 보고" 로 나타난다.
- **테스트 하네스 발견.** 송신 측 commit 보류(`holdCommits`, feat 4 테스트에서 사용)는 보류된 commit 이 같은 링크의 후속 prepare 를 막아 W>1 에서 "prepared but not committed" 상태를 비결정적으로 만든다. feat 6 통합 테스트는 **수신 측 commit 유실**(`LoseMessages`, atomic 플래그)로 바꿨다(`viewChangeCluster`).
- **뒤처진 새 리더 실측 (L1/L2).** W=3, 노드 2 가 2 뒤처진 채 다음 리더: "last decision seq 2 is greater than this node's current sequence 0" 으로 ViewData 거부 → 뷰 1 완성 불가 → `ViewChangeTimeout`(5 s) → 뷰 2(노드 3) 가 노드 1·3·4 로 완성 → 노드 2 는 노드 3 하트비트로 sync("Synchronizer returned with sequence 2") 후 뷰 2 합류. 뷰 3 없음. 설계 §7 1차안 그대로; 비용은 타임아웃 1회.
- **재합류 타이밍.** 분리됐던 노드를 다시 연결한 직후 제출한 요청은 그 노드가 아직 옛 뷰에 있어 pre-prepare 를 버리고, 하트비트 10틱 뒤에야 sync 된다(baseline 동작). 테스트는 "Starting view with number N" 을 기다린 뒤 제출한다.
- **편차 5번 (W=1).** VC 레코드가 마지막인 WAL 의 복원이 "없음" 에서 "InFlightData 에만 복원" 으로 바뀜 — upstream #748(main) 과 같은 동작이며 baseline 태그와는 다르다. ViewData 개수 불변, 내용(in-flight 보고)은 재시작 노드에서만 달라진다.
- **뷰 시작 경쟁 (baseline 구조, 테스트에서 실측).** 새 리더는 `ViewChanged` 직후 첫 제안을 보내고, 팔로워는 NewView 처리 → `ViewChanged` → `changeView` → `startView` 로 새 View 를 설치한다. 리더의 첫 pre-prepare(및 prepare)가 팔로워의 설치보다 먼저 도착하면 `Controller.ProcessMessages`(`controller.go:329`)가 그것을 **옛 View 인스턴스**에 넘겨 뷰 불일치로 버린다(미래 뷰 메시지 버퍼 없음). 노드 4개가 살아 있으면 한 노드의 유실은 정족수에 영향이 없지만, 뷰 체인지 테스트처럼 리더가 빠져 3개만 남으면 그 한 건으로 새 뷰의 첫 seq 가 멈추고 2 s 뒤 재불만 → 연쇄 뷰 체인지가 난다(전체 스위트 3차 실행 `TestRepeatedLeaderChangesKeepDecisions`). W>1 에서는 PR #5 의 "prepare 정족수 미달 슬롯 재전송" 이 정확히 이 간극을 메우며, 주기가 하트비트 주기(기본 6 s)라 2 s 불만 타임아웃보다 길다. 재전송은 pre-prepare 만 복구하고 함께 버려진 리더의 prepare 는 복구하지 않으므로, 리더가 분리되어 3 노드만 남으면 그 팔로워의 prepare 없이는 정족수(3)를 못 채운다 — 재전송만으로는 부족함을 실측. 테스트는 **리더를 분리하지 않고 하트비트 클록을 침묵**시켜(`App.heartbeatTime` 주입, 500 ms 틱, `stopHeartbeats`) 뷰 체인지를 유발한다: 옛 리더가 뷰 체인지에 참여해 4 노드가 살아 있으므로 한 노드의 유실이 정족수를 막지 않는다. **후속 후보**: (i) 재전송 주기를 하트비트와 분리한 설정, (ii) 컨트롤러가 `currViewNumber+1` 의 메시지를 짧게 보류, (iii) 새 리더가 `ViewChanged` 뒤 첫 제안을 팔로워 설치 확인(하트비트 응답) 뒤로 미루기. baseline(W=1)에도 같은 경쟁이 있으나 재전송이 없어 재불만으로만 회복된다 — upstream 보고 후보(재현 테스트 필요).

## 18. 리뷰 3차 (2026-11-05, 9라운드) — 발견 3건 판정

- **(a) A2 와 가짜 후보.** 후보 조건("prepared 보고 1개 이상")만으로는 바이잔틴 노드 1개가 존재하지 않는 값 q 를 prepared 로 보고해 후보에 올릴 수 있다. 그러나 A2 는 **q 를 보고한 노드가 f+1 개 이상**을 요구하고, f+1 개 중 적어도 하나는 정직하며 정직 노드는 실제로 받은 pre-prepare 만 보고하므로, A2 가 성립하는 값은 **그 뷰의 리더가 실제로 제안한 값**이다(정직 노드가 같은 (view, seq) 에 두 값을 받을 수 없으므로 유일). 가짜 후보는 preprepared ≤ f 로 A2 에 걸리지 않고, 정직 노드가 nothing 을 보고하면 B 로 비워진다. 이것이 A2 를 L(라이브니스) 로 분류한 근거의 완결: 바이잔틴 f 개는 어떤 값도 강제하지 못한다. `TestCheckInFlightPrefix/"a byzantine node alone reports a prepared proposal"` 가 고정.
- **(b) 뷰 시작 경쟁 → `fix/view-start-race`.** 논문 실험(리더 크래시·분리)에서 피할 수 없는 경로이므로 feat 8 전에 수정. 가장 작은 변경: 리더의 재전송 대상(prepare 정족수 미달 슬롯)에 **자기 prepare** 도 포함(`resendPrePrepares` 가 `slot.prePrepare` 와 `slot.lastBroadcastSent`(prepare) 를 함께 재방송). 팔로워 측은 중복 prepare 투표가 `voteSet` 에서 송신자별로 걸러지므로 변경 없음. W=1 은 `resendTick == nil` 이라 무변경. 회귀 테스트: 리더 **분리**로 뷰 체인지를 유발하는 `TestMultiInFlightThenLeaderDisconnect`(3 노드만 생존, 재전송 500 ms) 가 연쇄 뷰 체인지 없이 완료.
- **(c) 게이트에 통합 스위트 -race 1회 추가** — [branch-policy.md](../branch-policy.md) §4.
- **다음 순서(확정):** fix/view-start-race → 2차 벤치(프로세스 분리, p50/p99, W∈{1,2,4,8}×RTT∈{0,50,100,200}, 리더 크래시 복구 시간 1셀) → feat 7 config-barrier → feat 8 api-prev-proposal → feat 9 (fabric) verifier/assembler → Fabric+netem+Caliper 1차 측정 → adaptive-batch 조정 → feat 10 adaptive-depth. 번호는 branch-policy §3 갱신.
