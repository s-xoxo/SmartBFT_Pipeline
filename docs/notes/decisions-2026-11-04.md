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
- **1차 벤치 (feat 5·6 착수 전)**: `bench/lib` 로 N=4, W ∈ {1,2,4,8}, 모사 RTT ∈ {0,50,100} ms 를 짧게 돌려 "W 를 키우면 TPS 가 오르는가" 추세만 확인. 오르지 않으면 `Controller.Decide` 블로킹(열린 질문 ③)을 먼저 보며, 그 결과가 feat 6 설계에도 영향. 결과: `results/2026-11-04-first-trend/`.
