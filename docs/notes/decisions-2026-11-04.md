# 병합 후 리뷰 결정 목록 (2026-11-04)

feat 4 (`feat/pipeline-window`, PR #4, merge `323db6b2`) 병합 보고에 대한 리뷰어 판정과 feat 5·6 착수 전 결정. 게이트 결과와 diff 핵심은 승인 기준 충족.

## 편차 2개 판정

| # | 편차 | 판정 | 기록 |
|---|---|---|---|
| 1 | `changeView` 에서 `Batcher.Reset` 을 `startView` 앞으로 | **승인**, 단 "설계 편차" 가 아니라 **proposer 고루틴 분리가 드러낸 순서 의존성**. 닫힌 배처에 토큰 → 새 리더 침묵은 라이브니스 버그 | [leader-token-flow.md](leader-token-flow.md) 회수 규칙 **R8 (배처 열기 → 뷰 시작 → 토큰 발급)** 로 추가. feat 6 이 뷰 체인지 경로를 다시 건드릴 때 불변식으로 유지. W=1 메시지 집합 불변은 히스토그램 테스트가 이미 보장 → 추가 확인 불필요 |
| 2 | 히스토그램: 구조적 종류 정확 일치, 변동 종류 범위 | **승인** | 논문 본문: "pre-prepare/prepare/commit/view-data/new-view 메시지 수가 원본과 정확히 일치". 범위 비교 종류는 각주 |

## feat 5 (`feat/state-wal-window`) 착수 전 결정

**순서 고정: feat 5 → feat 6.** 뷰 체인지 다중 복구(feat 6)는 "내가 WAL 에 뭘 들고 있는가" 를 `InFlightData` 에서 읽는데, 현재 `PersistedState.Restore` 가 마지막 1~2 레코드만 복원하므로 feat 6 을 먼저 하면 재시작 후 뷰 체인지가 잘못된 in-flight 집합을 보고한다.

| 결정 사항 | 선택 | 근거 |
|---|---|---|
| WAL 레코드 형식 | **슬롯별 레코드를 그대로 쌓고 `Restore` 가 "마지막 경계 이후 전부" 를 읽는다** (대안: 윈도우 스냅샷 1개 — 복잡, 작음) | W ≤ 8 이면 레코드 수가 적다. `truncateTo` 를 **"low 슬롯 커밋 시"** 로 바꾸면 자연스럽게 정리된다. feat 4 가 유지한 `Save(pp, true)` 가 여기서 바뀐다. 경계 = 마지막 결정 레코드와 `NewView` 레코드(D1, [viewchange-trace.md](viewchange-trace.md) "D1 코드 확인") |
| 복원 후 토큰 수 | feat 4 불변식 **`len(token) + inflight + restored == W`** 그대로 | `startView` 의 `W − restored` 발급이 이미 이를 가정. `Restore` 가 복원한 개수를 정확히 돌려주면 끝 |
| 병합 기준 | feat 4 의 "W>1 in-flight 재시작 거부" (`InFlightSequenceInWAL` + `Consensus.Start` error) **제거**, 그 자리에 **in-flight 2개 이상 상태에서 Restart → 같은 슬롯 집합 복원 → 정상 커밋** 통합 테스트 | `TestPipelineRestartWithInFlightIsRejected` 를 복원 성공 테스트로 교체 |

## feat 6 (`feat/viewchange-multi-inflight`) 전 확인 — D1 과 미전달 commit 서명

질문: in-order 게이트가 보관하는 미전달 commit 서명이 D1 파기 대상에 포함되는가. `InFlightData`(ViewChanger 소유) 에 복사본이 남으면 D1 위반.

**결과 (코드 확인, main `323db6b2`):**
- 게이트 버퍼(`View.slots[seq].signatures`, 투표, `verifiedSigs`) 는 View 안에 있고 `View.Abort()` 로 자동 소멸 → **충족**.
- `InFlightData.entries` 는 모든 `State.Save` 가 seq 키로 채우며, 삭제는 `decide → pruneDelivered` 와 `sync → clearUpTo` 두 곳뿐. **뷰 체인지 경로(`abortView`/`changeView`/`startView`/`processNewViewMsg`) 에 clear 가 없다** → W>1 에서 폐기 접미사 항목(제안 + prepared 플래그)이 남는다 → **D1 위반**. 2026-11-02 노트의 "abortView 의 `InFlightData.clear` 를 일반화" 는 전제가 틀렸다(baseline abortView 는 InFlightData 를 건드리지 않음).
- 노출: 뷰 v 의 `p..p+2` 중 `p` 만 복구된 뒤 v+1 이 `p+1` 을 제안하기 전에 다시 뷰 체인지가 나면 옛 `p+1` 이 in-flight(prepared) 로 보고된다. 전달 기준 안전성은 Lemma 2 로 유지되지만 feat 6 의 다중 ViewData 에서는 폐기 항목이 복구 후보에 섞인다.
- WAL 쪽은 `NewView` 레코드가 경계라 재시작 경로는 충족; feat 5 의 "경계 이후 전부 읽기" 가 이 경계를 지켜야 한다.

**feat 6 요구 사항 (확정):** `InFlightData.clearAbove(seq)` (`s > seq` 삭제) 를 `Controller.changeView` 의 `startView` 직전에 `clearAbove(newSeq)` 로 호출. **`> low` 만** 지운다 — W=1 의 단일 항목은 항상 `≤ low`(복구된 항목 `low−1`, 미복구 항목 `low`) 이므로 no-op 이고 ViewData 바이트 동일; `≥ low` 를 지우면 baseline 의 미복구 in-flight 이월(PBFT prepared 인증서 이월)이 깨진다. 히스토그램 테스트 + `InFlightData` 단위 테스트로 고정. 상세 표: [viewchange-trace.md](viewchange-trace.md).

## 남은 사항

- **원격 브랜치**: 병합된 `feat/` 브랜치는 보존, merge commit 이 기준 — [branch-policy.md](../branch-policy.md) §4 에 추가.
- **1차 벤치 (feat 5·6 착수 전)**: `bench/lib` 로 N=4, W ∈ {1,2,4,8}, 모사 RTT ∈ {0,50,100} ms 를 짧게 돌려 "W 를 키우면 TPS 가 오르는가" 추세만 확인. 오르지 않으면 `Controller.Decide` 블로킹(열린 질문 ③)을 먼저 보며, 그 결과가 feat 6 설계에도 영향. 결과: [results/2026-11-04-first-trend/](../../results/2026-11-04-first-trend/README.md).

  **1차 벤치 결과 (2026-11-04, N=4, K=256 closed-loop, 배치 50, 15 s/셀, 뷰 체인지 0):**

  | RTT | W=1 | W=2 | W=4 | W=8 |
  |---|---|---|---|---|
  | 50 ms | 613 TPS / 407 ms | 1,213 / 242 | 2,425 / 89 | 3,077 / 81 |
  | 100 ms | 317 / 783 | 633 / 467 | 1,253 / 160 | 1,573 / 156 |
  | 100 ms, K=1024 | 317 | 633 | 1,253 | **2,506 (= 8×)** |

  - **W 를 키우면 TPS 가 오른다**: RTT 50/100 에서 W 당 거의 선형(2배씩). W=4→8 의 꺾임은 K=256 이 배치 8개를 못 채운 부하 상한이며 K=1024 에서 사라짐. 다음 실험은 K ≥ 2·W·batch, W=16 까지.
  - **열린 질문 ③ (Decide 블로킹)**: 결정당 Deliver ≈ 13 µs(RTT 0) / ≈ 100 µs(RTT 50/100) 로 결정 주기의 0.1 % 수준 → **병목 아님**, feat 6 설계에 제약 없음. 실제 앱(fsync·블록 검증)을 모사한 Deliver 지연 셀은 다음 실험에 추가.
  - **RTT 0 은 무효**: 단일 머신 CPU 경합으로 팔로워가 창 밖 commit 을 보고 `Sync()` 를 반복(W=1 에서도 83회). 노드별 프로세스/GOMAXPROCS 분리 후 재측정.
  - **관찰 (후속 후보)**: W>1 에서 리더의 6 s 재전송이 이미 전달한 팔로워에 `Got pre-prepare for sequence N but we're in sequence N+k` Warn 을 남김(셀당 6–12). 손실·뷰 체인지 없음. prev 링의 중복 pre-prepare 로그 레벨 분기(W>1) 또는 gap-triggered 재전송과 함께 처리.

## 4라운드 (2026-11-04 후반) — D1 조건, 재전송 Warn, feat 5 체크포인트 필터

리뷰어 판정: D1 결정 **승인(조건 1)**, 재전송 Warn **지금 처리**, feat 5 **Restore 체크포인트 필터 추가**. feat 5 설계 문서 제출 후 feat 6 은 D1·sync 순서까지 포함해 설계 리뷰 1회 더.

### D1 조건: sync 경로의 `clearUpTo` 와 `clearAbove` 의 순서

질문: sync 뒤 `changeView` 가 불리면 `clearUpTo(syncedSeq) → clearAbove(low)` 로 두 번 지우는데, sync 로 low 가 점프했을 때 `clearAbove` 의 low 가 **새 low** 임이 보장되는가.

**코드 확인 (main `323db6b2`, `controller.go`):**
- `run()` 의 sync 분기: `view, seq, dec := c.sync()` → `c.changeView(view, seq, dec)`. `sync()` 안에서 `Checkpoint.Set(latest)` → `newProposalSequence = latestDecisionSeq + 1` → `maybePruneInFlight → clearUpTo(latestDecisionSeq)` 순으로 끝나고, `changeView` 는 그 `newProposalSequence` 를 **인자로** 받는다.
- 따라서 feat 6 의 호출은 `c.InFlight.clearAbove(newProposalSequence)` — `changeView` 의 인자이며, `c.currView.ProposalSequence` 나 `ViewSequences`(둘 다 **옛 뷰의 low**) 를 읽으면 안 된다. 인자를 쓰면 순서는 `clearUpTo(S) → clearAbove(S+1)` 이고 남는 집합 ⊆ `{S+1}` — baseline `maybePruneInFlight` 가 "아직 유효할 수 있다" 며 남기는 in-flight(`seq == synced+1`) 와 정확히 같은 항목이다.
- `changeView` 호출 4곳의 인자 모두 **새 low** 다: ① ViewChanger 경로 `newView.proposalSeq` = `commitInFlightProposal` 뒤의 `mySequence+1` (`viewchanger.go:1228`), ② sync 경로 `latestDecisionSeq+1`(또는 sync 가 결정을 안 가져왔으면 `latestSeq()+1`), ③ sync 무결과 재시작 경로 `ViewSequences.ProposalSeq` = 중단된 뷰의 low(그 위 항목은 중단된 뷰 인스턴스의 슬롯이므로 삭제가 맞음), ④ 로테이션 `md.LatestSequence+1`.

**feat 6 테스트 항목 (추가):** `TestInFlightClearedAfterSyncAndViewChange` — in-flight `p..p+2` 상태에서 (a) sync 가 `p+2` 까지 가져오면 `changeView` 뒤 `InFlightData` **비어 있음**; (b) sync 가 `p` 까지만 가져오면 남는 항목은 `{p+1}` 하나(= 새 low, baseline 의 이월 의미). 컨트롤러 단위 테스트(`controller_window_internal_test.go`) 로 두 경우 모두 고정.

### 논문 구현 장 단락 초안 (리뷰어 제안)

> SmartBFT 의 "단일 in-flight" 가정은 설정값이나 명시적 검사로 코드에 드러나 있지 않다. 뷰 체인지가 다음 뷰로 넘길 제안을 담는 `InFlightData` 는 모든 WAL 기록 시점에 덮어쓰기로 갱신되고, 삭제 지점은 결정 전달 직후(`decide`)와 동기화 직후(`sync`) 두 곳뿐이다. 항목이 하나뿐이면 다음 기록이 이전 항목을 덮어쓰므로 뷰 체인지 경로에서 따로 비울 필요가 없었고, 그 결과 "뷰 체인지 시 폐기된 상태를 버린다" 는 요구(D1)는 코드 어디에도 적혀 있지 않은 **암묵적 불변식**으로만 성립해 왔다. 항목을 seq 별로 다중화하자 이 불변식이 깨졌고, 이는 파이프라이닝 미지원이 하나의 설계 결정이라기보다 상태 저장·뷰 체인지·WAL 복원 전반에 스며든 가정이었음을 보여준다. 본 구현은 뷰 시작 지점(`changeView`)에 `clearAbove(newLow)` 를 두어 이 불변식을 명시적 코드로 바꾸되, `> low` 만 삭제해 깊이 1 에서는 바이트 수준으로 원본과 같은 뷰 체인지 메시지를 유지한다.

(출처: 위 "feat 6 전 확인" 과 [viewchange-trace.md](viewchange-trace.md) D1 코드 확인. 논문 본문에서는 `InFlightData` 등 식별자 대신 서술어로 바꿔 쓴다. 같은 장에 "연구 과정에서 발견해 보고한 원본 결함" 한 줄: WAL 복원이 체크포인트를 무시해 stale 제안을 되살리는 결함 — upstream 이슈 [hyperledger/SmartBFT#752](https://github.com/hyperledger/SmartBFT/issues/752), 재현 `results/2026-11-04-baseline-stale-restore/`.)

### 6초 재전송 Warn — `fix/pipeline-resend-noise` (PR #5)

기능 문제는 아니지만 실험 로그 품질을 위해 지금 처리. 두 변경 모두 **W>1 전용**(W=1 바이트·로그 동일), 별도 `fix/` 브랜치로 (브랜치 정책 §1 에 `fix/` 추가).

| # | 변경 | W=1 보존 근거 |
|---|---|---|
| 1 | `handlePrevSeqMessage` 의 "이미 전달한 seq 의 pre-prepare" 로그를 **W>1 이면 Debug**, W=1 은 Warn 유지 | `TestTwoPrePreparesInARow` 가 W=1 Warn 을 단언하는 것은 `storePrePrepare` 쪽 메시지이고, prev 링 메시지도 W=1 분기를 그대로 둠. 새 테스트 `TestViewPrevSeqPrePrepareLogLevel` (depth 1 Warn / 2·4 Debug) |
| 2 | `resendPrePrepares` 대상을 "미커밋 슬롯(`< slotCommitted`)" 에서 **"리더 기준 prepare 정족수 미달 슬롯(`< slotPrepared`)"** 으로 축소 | W=1 은 `resendTick == nil` 이라 호출 자체가 없음. 근거: prepare 정족수가 모였으면 정족수 노드 전부가 그 pre-prepare 를 갖고 있고(D2 접두사 규칙으로 그 아래도), 그 밖의 ≤ f 노드는 prev 링·sync 로 따라오는 baseline 경로. 새 서브테스트 "depth 2 does not resend prepared sequences" |

설계 문서 반영: [design-pipeline-window.md](design-pipeline-window.md) §5.4. PR 본문: [../pr/fix-pipeline-resend-noise.md](../pr/fix-pipeline-resend-noise.md).

### feat 5 추가 확인 — Restore 의 체크포인트 필터

지적: truncateTo 를 "low 슬롯 커밋 시" 로 바꾸면 복원 시작점이 마지막 truncate 지점이 된다. 노드가 low 를 커밋(전달)하고 다음 truncate 전에 죽으면 경계 뒤에 **이미 커밋된 low 의 레코드**가 남고, 이를 in-flight 로 복원하면 중복 전달 시도(`MutuallyExclusiveDeliver` 가 막음)와 함께 토큰 계산 `W − restored` 가 1 틀어진다.

**결정:** `Restore` 는 fold 결과 중 `seq ≤ 체크포인트 seq`(`Checkpoint.Get()` 의 `LatestSequence`; Deliver 직후 갱신) 인 항목을 **버린다**. baseline 의 `recoverPrepared` 가 `v.ProposalSequence > seq` 를 "safely committed" 으로 버리던 분기의 일반화이며, W=1 에서는 그 분기와 동일하게 동작한다(`recoverProposed` 쪽은 W=1 에서 `seq ≤ 체크포인트` 인 PP 가 경계 뒤에 남을 수 없어 영향 없음). 상세: [design-state-wal-window.md](design-state-wal-window.md) §5.

## 5라운드 — feat 5 설계 승인 (2026-11-04)

승인. truncateTo 는 리뷰어 의도("low 전달 시 truncate 레코드")와 달랐으나 설계안(PP 저장 시 `seq == low` 플래그) 채택, **truncateTo 규칙 + 체크포인트 필터 = 한 쌍** 명시. 열린 질문 결정: ① 보존량 상한 없음 + `wal_records_since_boundary` 메트릭, ② 필터 W=1 적용 + "W=1 편차 4번" + baseline 재현(→ 재현됨, 뷰 체인지 없음, upstream 이슈 [#752](https://github.com/hyperledger/SmartBFT/issues/752) 게시 2026-11-04), ③ `seq > C+W` error + PipelineDepthMax 안내 문구, ④ 복원 슬롯은 PR #5 재전송 규칙 대상에 포함, ⑤ 뷰 불일치 레코드는 버리고 Warn. 상세: [design-state-wal-window.md](design-state-wal-window.md) §12. feat 6 설계 문서에 D1·sync 순서 테스트 2 케이스를 그대로 가져간다.

## 6라운드 — upstream 테스트 조정 승인·이슈 게시 승인·feat 6 설계 항목 (2026-11-04)

- **#739 의도 확인**: PR 본문·커밋 메시지는 WAL 파일 증가(issue #736) 수정이며 테스트는 "in-flight proposal restart test 를 10회 재시작·파일 수 불변으로 강화" 한 것. `git log -S` 로 테스트 이름이 #739 에서 처음 등장. "sync 후에도 복원" 의도 없음 → `RestartSync(false)` 조정 정당. 이슈 본문에 한 문단으로 기재.
- **이슈 게시 조건 3개 확인**: (1) upstream main `f55a7d12` 워크트리에 재현 테스트만 넣어 실행 → 동일 재현(포크 하네스 불필요; 테스트는 upstream `test/` 하네스만 사용). (2) 제목은 현상, 체크포인트 필터는 "One possible fix" 로 마지막, 연구 언급 없음. (3) 범위: 안전성 위반 아님, 라이브니스 지연, 영향 노드 1개면 뷰 체인지 없음, f+1 동시 재시작 시 불필요한 뷰 체인지 가능. → **게시: [hyperledger/SmartBFT#752](https://github.com/hyperledger/SmartBFT/issues/752)**.
- feat 6 설계 문서 항목(재확인분 포함): [design-viewchange-multi-inflight.md](design-viewchange-multi-inflight.md).
