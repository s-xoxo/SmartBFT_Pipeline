# feat 5 `feat/state-wal-window` 설계 — WAL 다중 in-flight 저장·복원

- 날짜: 2026-11-04
- 대상 커밋 (SmartBFT 포크): main `db03681c` (PR #4 feat 4 + PR #5 fix 포함). 라인 번호는 이 커밋 기준
- 선행 결정: [decisions-2026-11-04.md](decisions-2026-11-04.md) "feat 5 착수 전 결정" + 4라운드 "체크포인트 필터"; 사실 조사: [state-wal.md](state-wal.md) (G 절, baseline)
- 플래그: `PipelineDepthMax` (W = `EffectivePipelineDepth()`). **W=1 에서 WAL 바이트열·복원 결과가 baseline 과 동일** (§8)
- 후속: feat 6 `feat/viewchange-multi-inflight` 이 이 브랜치의 `InFlightData` 복원 결과를 ViewData 에 싣는다

## 1. 목표와 범위

W>1 로 동작하던 노드가 in-flight 제안 m 개(1 ≤ m ≤ W)를 WAL 에 둔 채 죽었다가 재시작하면, **같은 슬롯 집합을 같은 단계(PROPOSED/PREPARED)로 복원**하고 리더라면 토큰을 `W − m` 개 발급해 불변식 `len(token) + inflight + restored == W` 를 지킨다. feat 4 가 둔 임시 조치 — W>1 이고 WAL 에 in-flight 가 있으면 `Consensus.Start` 가 error 로 거부(`consensus.go:122-129`, `state.go InFlightSequenceInWAL`) — 를 제거한다.

범위 밖: `pkg/wal` 변경(그룹 커밋, 파일 정책), 뷰 체인지 메시지(feat 6), `WindowMark` 류 스냅샷 레코드(채택하지 않음).

## 2. baseline·feat 4 현황 (요약)

| 항목 | 현재 (`db03681c`) | 근거 |
|---|---|---|
| 레코드 종류 | `proposed_record`(PP + 내 Prepare), `commit`(내 Commit), `new_view`, `view_change` — 4종, proto 변경 없음 | `messages.proto:113-120` |
| truncateTo | PP 레코드 **항상 true** (`view.go:630 Save(savedMsg, true)`), 나머지 false | feat 4 가 그대로 유지 |
| `ReadAll` | 마지막 truncateTo 레코드부터 끝까지 | `writeaheadlog.go:528-570` |
| `Restore` | **마지막 1~2 레코드만** 본다: 마지막이 PP → `recoverProposed`, C → `recoverPrepared`(직전이 짝 PP 라고 가정), NV/VC → 복원 없음 | `state.go:160-187` |
| 체크포인트 비교 | `recoverPrepared` 만: `v.ProposalSequence > seq` → "safely committed" 무시, `<` → error. `recoverProposed` 는 비교 없음 | `state.go:224-233` |
| View 수용 | 단일 필드 `inFlightProposal/inFlightRequests/lastBroadcastSent/myProposalSig` + `Phase`; `setup()` 이 `slots[low]` 하나로 변환, `head = low+1` | `view.go:114-117, 252-265` |
| 토큰 | `startView`: `restored = 1 if initPhase ∈ {PROPOSED, PREPARED}`; 발급 `W − restored` | `controller.go:398-411` |
| `InFlightData` 복원 | `Restore` 가 `ps.storeProposal/storePrepared` 로 채움(seq 키, feat 4) | `state.go:195, 237-238` |
| W>1 거부 | `InFlightSequenceInWAL` → `Start` error | `consensus.go:122-129` |

W>1 에서 깨지는 지점 두 가지: (i) PP(n+1) 의 truncateTo=true 가 미결정 PP(n)/C(n) 을 복구 집합에서 지운다, (ii) `Restore` 가 `[PP(n), PP(n+1), C(n), C(n+1)]` 같은 혼합 순서를 읽지 못한다.

## 3. 레코드 형식 — 변경 없음

슬롯별로 지금과 같은 `proposed_record`/`commit` 레코드를 그대로 쌓는다. seq 는 각각 `pre_prepare.seq`, `commit.seq` 에 이미 있고 view 도 들어 있다. 새 proto 메시지·필드 없음 → W=1 WAL 바이트 동일의 첫 근거, 업스트림 바이너리와의 WAL 호환도 유지(state-wal.md 열린 문제 6 해소).

## 4. truncateTo 규칙

**`proposed_record(seq)` 의 truncateTo = (seq == low)**, 즉 Save 시점에 그보다 작은 미결정 seq 가 없을 때만 true. `commit` 은 항상 false(원본 유지). 구현 위치: `view.go processProposal` 의 `Save(savedMsg, true)` → `Save(savedMsg, slot.seq == v.ProposalSequence)`.

- 리뷰어 표현 "low 슬롯 커밋 시" 의 해석: 경계가 되는 레코드는 **low 슬롯의 PP**(그 아래가 모두 커밋·전달된 뒤 처리된 PP)다. commit 레코드는 경계가 될 수 없다(ReadAll 이 경계 **앞**을 버리므로 PP 가 C 와 함께 남아야 함). 다른 해석이면 지적 요청.
- W=1: feat 4 의 처리 창은 `[low, low+W) = {low}` 이므로 `processProposal` 이 보는 슬롯은 항상 `seq == low` → **항상 true → 바이트 동일**.
- W>1 지속 부하: low 가 늘 in-flight 라 true 가 드물다(파이프라인이 비는 순간마다 한 번). 그 사이 WAL 은 파일 단위로 쌓이고(`pkg/wal` 삭제는 파일 전환 시, state-wal.md G3) 복구 시 `ReadAll` 이 경계 이후 전부를 메모리에 올린다. 예상 보존량 ≈ (경계 이후 결정 수 + W) × (PP + C 크기). 논문용으로는 `wal_count_of_files` 와 복구 시간을 측정 항목에 넣는다 (§11 열린 질문 1).

## 5. Restore 알고리즘 (`state.go Restore` 대체)

입력: `entries` (= ReadAll 결과, 마지막 경계 이후 전부), 체크포인트 seq `C` (= `v.RetrieveCheckpoint()` 메타데이터의 `LatestSequence`; Deliver 직후 갱신되는 값. baseline `recoverPrepared` 가 쓰던 `v.ProposalSequence − 1` 과 같은 값이며 NewView 복원 경로에서도 작거나 같아 보수적), 깊이 `W = v.depth()`.

1. **마지막 레코드가 `new_view`/`view_change` 이면 복원 없음** (baseline 과 동일; `LoadNewViewIfApplicable`/`LoadViewChangeIfApplicable` 은 그대로 마지막 레코드를 본다 — 레코드 순서가 바뀌지 않으므로 의미 불변).
2. **경계**: 마지막 `new_view` 또는 `view_change` 레코드의 위치 b. **b 이후 레코드만** fold 한다(D1: 뷰 체인지 전 뷰의 슬롯은 복원하지 않음 — viewchange-trace.md "D1 코드 확인" 4항). b 가 없으면 전부.
3. **fold**: `map[seq]{pp *ProposedRecord, commit *Message}`. 같은 seq 의 PP 가 두 번 나오면 뒤의 것(같은 뷰에서는 불가능; 방어적), C 는 짝 PP 가 있어야 하며 없으면 error(baseline "last message is a commit, but expected … matching pre-prepare" 의 일반화).
4. **체크포인트 필터 (4라운드 결정)**: `seq ≤ C` 항목 **버림**(이미 전달됨; 복원하면 `MutuallyExclusiveDeliver` 가 막아도 토큰 수가 1 틀어짐). `seq > C + W` 항목 → **error** (baseline `recoverPrepared` 의 "WAL ahead of last committed" error 일반화; W 를 줄여 재시작한 경우 포함 — §11 질문 3).
5. **연속성**: 남은 seq 는 `C+1 .. C+m` 로 연속이어야 한다. 간극이 있으면 error(정직한 WAL 에서는 불가능: PP 는 D2 접두사 규칙으로 seq 순으로 기록되고 경계는 low 의 PP 뿐).
6. **슬롯 복원**: 각 seq 에 대해 `restoredSlot{seq, proposal, prepare(ProposedRecord.Prepare), commit(있으면), phase = PREPARED if commit else PROPOSED, prePrepare = Message{PrePrepare: pp}}`. `View` 의 단일 필드 4개를 `restored []restoredSlot` 로 바꾸고 `setup()` 이 seq 순으로 `slots[seq]` 를 만든다: `proposal`, `requests = Verifier.RequestsFromProposal`, `lastBroadcastSent = commit ?? prepare 메시지`, `mySig`(commit 의 서명), `prePrepare`(리더의 W>1 재전송용; W=1 은 재전송 없음), `phase`, `recordPrePrepareBegin(seq)`. `head = C+1+m`, `v.Phase = phase(lowest)`(메트릭·`startView` 호환).
7. **`InFlightData`**: 각 복원 항목에 `storeProposal(pp)`, commit 있으면 `storePrepared(commit)` — 지금과 같은 함수, 항목 수만 m.
8. **로그**: `Restored %d proposals in flight, sequences %d..%d` (통합 테스트가 기다리는 문자열).

의사 코드:

```go
func (ps *PersistedState) Restore(v *View) error {
    v.Phase = COMMITTED
    if len(ps.Entries) == 0 { return nil }
    recs := unmarshalAll(ps.Entries)                      // error → 기존 메시지 유지
    if last := recs[len(recs)-1]; last.GetNewView() != nil || last.GetViewChange() != nil { return nil }
    recs = recs[lastBoundary(recs)+1:]                    // new_view/view_change 뒤만
    slots, err := foldSlots(recs)                         // map[seq]*restoredSlot, C 없는 PP 짝 검사
    ckpt := checkpointSeq(v.RetrieveCheckpoint())
    restored := filterAndOrder(slots, ckpt, v.depth())    // ≤ ckpt 버림, > ckpt+W error, 연속성 error
    for _, r := range restored { ps.storeProposal(r.pp); if r.commit != nil { ps.storePrepared(r.commit) } }
    v.restored = restored
    if len(restored) > 0 { v.Phase = restored[0].phase; v.Number = restored[0].view; v.ProposalSequence = restored[0].seq; v.DecisionsInView = restored[0].decisionsInView }
    return nil
}
```

`NewProposer`(`util.go:412-420`) 의 "컨트롤러 seq/view 가 더 크면 덮어쓰기" 는 그대로 둔다; 필터 덕분에 복원 항목이 있으면 `restored[0].seq == C+1 == proposalSequence` 라 덮어쓸 일이 없다.

## 6. 토큰과 `startView`

`startView` 의 `restored` 를 phase 기반 0/1 에서 **복원 개수 m** 으로 바꾼다. 전달 경로: `ProposerBuilder.NewProposer` 반환값 `(Proposer, Phase)` 는 유지하고 `Proposer` 에 메서드를 추가하지 않기 위해, `View` 에 `RestoredCount() uint64` 를 두고 `startView` 가 `view.(interface{ RestoredCount() uint64 })` 로 조회(mock 재생성 회피; 테스트의 `Proposer` 목은 0). W=1: m ∈ {0,1} 이고 phase 와 1:1 대응 → 발급 수 동일. 불변식 `len(token) + inflight + restored == W` 는 `TestControllerTokensAfterRestore` 로 고정.

## 7. `Consensus.Start` 와 거부 제거

`consensus.go:120-129` 블록과 `state.go InFlightSequenceInWAL` 삭제. feat 4 PR 본문의 "W>1 + WAL in-flight 재시작 error 거부" 항목은 이 브랜치에서 해소된 것으로 progress.md 에 적는다.

## 8. W=1 동일성 논증

| 측면 | baseline / feat 4 | 이 설계 (W=1) | 동일? |
|---|---|---|---|
| WAL 바이트 | PP true, C false, NV/VC false | §4: `seq == low` 가 W=1 에서 항상 참 | **동일** |
| `ReadAll` 결과 | `[PP]`, `[PP, C]`, `[…, NV]`, `[…, VC]` 중 하나 | 같다(바이트 동일이므로) | 동일 |
| `[PP(n)]`, `n > C` | PROPOSED, seq n | fold → {n}, 필터 통과, PROPOSED | 동일 |
| `[PP(n), C(n)]`, `n == C+1` | PREPARED | PREPARED | 동일 |
| `[PP(n), C(n)]`, `n ≤ C` | "safely committed", 복원 없음 | 필터로 버림 | 동일 |
| `[PP(n), C(n)]`, `n > C+1` | error | `> C+W` error | 동일 |
| `[PP(n)]`, `n ≤ C` | **baseline 은 복원함**(비교 없음) → `NewProposer` 가 seq 만 C+1 로 올려 **stale 제안이 slot[C+1] 에 PROPOSED 로 들어감** | 필터로 버림 | **다름 — 아래** |
| 마지막 NV/VC | 복원 없음 | 복원 없음 | 동일 |

마지막 행은 "PP 만 기록한 뒤 sync 로 체크포인트가 그 seq 를 넘어선 채 재시작" 한 경우다(`sync()` 는 fetchState 가 새 뷰를 줄 때만 NV 레코드를 쓴다 `controller.go:740-756`). baseline 은 이때 옛 제안의 prepare 를 다시 뿌리고 `C+1` 의 prepare 를 옛 digest 와 비교하다 뷰 체인지로 빠진다(`view.go:672` "Got wrong digest"). 체크포인트 필터는 이 경로를 고치며, WAL 바이트와 메시지 히스토그램(정상 경로)은 바꾸지 않는다. **권고: 필터를 W 와 무관하게 적용**(§11 질문 2). 엄격한 바이트 동일을 원하면 `depth() == 1` 에서는 PP-only 항목을 필터에서 제외하는 분기 한 줄로 baseline 동작을 재현할 수 있다.

## 9. 테스트 계획

단위 (`internal/bft/state_test.go`, 기존 `TestStateRestore` 유지):
- `TestRestoreFoldsWindow`: `[PP n, PP n+1, C n]` → {n PREPARED, n+1 PROPOSED}, head n+2, `InFlightData` 2 항목(n prepared); `[PP n, PP n+1, C n, C n+1]` → 둘 다 PREPARED; `[PP n, C n, PP n+1]`, C=n → {n+1} 만(체크포인트 필터), 로그 1건.
- `TestRestoreBoundary`: `[PP n, C n, NV, PP n+1]` → {n+1} 만; `[PP n, PP n+1, NV]` → 복원 없음(마지막 NV); `[PP n, VC]` → 없음.
- `TestRestoreErrors`: C 만 있는 seq, `seq > C+W`, 간극.
- `TestRestoreDepthOneMatchesBaseline`: §8 표의 행들을 W=1 로 돌려 기존 함수 결과와 필드 단위 비교(필터 행은 기대값을 명시).
- `TestSaveTruncateFlagFollowsLow` (`view_window_internal_test.go`): W=2 에서 PP(low) true, PP(low+1) false, low 전달 뒤 PP(low+2) false(low+1 이 미결정), W=1 은 항상 true.
- `TestControllerTokensAfterRestore` (`controller_window_internal_test.go`): m=2, W=3 → 토큰 1.

통합 (`test/pipeline_window_test.go`):
- **`TestPipelineRestartRestoresInFlight`** (병합 기준; `TestPipelineRestartWithInFlightIsRejected` 교체): W=3, 노드 4 가 pre-prepare 만 받아 PP 2개 이상을 WAL 에 두고 결정 없음 → `Restart()` → 로그 "Restored 2 proposals in flight" → 메시지 손실 해제 → 노드 4 가 같은 seq 들을 다른 노드와 같은 내용으로 전달.
- `TestPipelineLeaderRestartWithInFlight`: 리더가 2개 in-flight 상태에서 재시작 → 복원 슬롯 pre-prepare 재전송(W>1 틱) → 정상 커밋, 토큰 불변식(이후 제안 수 ≤ W).
- `TestMessageHistogramMatchesBaseline` 무변경 통과(W=1 바이트 동일).

## 10. 커밋 순서 (각 커밋 W=1 green)

a. `state: fold the WAL into per-sequence slots and filter by checkpoint` — Restore 재작성 + 단위 테스트, View 는 `restored` 슬라이스 수용(`setup` 변경).
b. `view: truncate the WAL at the pre-prepare of the lowest open sequence` — truncateTo 규칙 + 테스트.
c. `controller: issue W minus the restored proposals as leader tokens; drop the depth>1 restart refusal` — `RestoredCount`, `consensus.go` 거부 제거, `InFlightSequenceInWAL` 삭제.
d. `test: restart with proposals in flight restores the same slots` — 통합 테스트 교체·추가, PR 본문.

## 11. 열린 질문 (리뷰어 결정 요청; 괄호는 권고)

1. **WAL 보존량**: 지속 부하에서 truncateTo 가 거의 참이 되지 않아 WAL 이 파일 단위로 자란다. 논문 범위에서는 측정 항목(`wal_count_of_files`, 복구 시간)으로 두고 compaction 은 후속으로 미룬다. (권고: 그대로 — 스냅샷 레코드 불채택 결정과 일관)
2. **체크포인트 필터의 W=1 적용**: §8 마지막 행. (권고: W 무관 적용 — baseline 의 잠재 라이브니스 결함 수정이며 WAL 바이트·히스토그램 불변)
3. **`seq > C+W` 처리**: error(권고, feat 4 거부와 같은 메시지 양식) vs. 초과분 버림. W 를 줄여 재시작하는 운영 시나리오는 논문 실험에 없음.
4. **리더 복원 슬롯의 pre-prepare 재전송**: 복원한 `prePrepare` 를 재전송 대상에 넣는다(권고). 넣지 않으면 리더 재시작 후 팔로워가 놓친 PP 를 영구히 못 받고 뷰 체인지로만 회복.
5. **`v.Number` 와 레코드 뷰 불일치**: 복원 항목의 뷰가 서로 다르면(경계 없이 뷰가 바뀐 비정상 WAL) error 로 할지. (권고: error — 정직한 WAL 에서는 NV 레코드가 경계)
