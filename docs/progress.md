# 진행 기록

SmartBFT 포크 main 에 들어간 변경과 그 근거 PR. 논문 구현 장에서 "이 변경은 커밋 X" 로 지칭할 때 이 표를 쓴다.

| 날짜 | 순서 | 브랜치 | PR | merge commit | 내용 | 플래그 |
|---|---|---|---|---|---|---|
| 2026-11-02 | — | baseline | — | 32a628d6 (태그) | upstream hyperledger-labs/SmartBFT main, 비교군 | — |
| 2026-11-02 | — | main | — | ea1d499d | PR 템플릿 추가 (문서) | — |
| 2026-11-02 | 1 | feat/metrics-round-latency | [#2](https://github.com/s-xoxo/SmartBFT/pull/2) | fa0ae1ad | seq 별 라운드 지연, WAL append 지연, in-flight 수, 풀 바이트, 배치 충만 사유 메트릭. 프로토콜 무변경 | 없음 |
| 2026-11-02 | 2 | feat/pipeline-config | [#1](https://github.com/s-xoxo/SmartBFT/pull/1) | d9d36397 | `PipelineDepthMax`·`AdaptiveBatch` 필드, `Validate()` 규칙(depth>1 ⇒ rotation off, W×count ≤ pool), 하네스 `Restart()` 전체 설정 유지, `newNodeWithConfig`, reconfig 변환기 | 둘 다 (기본값 원본) |
| 2026-11-02 | 3 | feat/adaptive-batch | [#3](https://github.com/s-xoxo/SmartBFT/pull/3) | 5ae45b7c | `BatchParams` atomic + `SetBatchParams`, 풀 getter 3개, `BatchTuner`(C: FULL+backlog AIMD, T: 충만율, 지연 가드=슬라이딩 윈도우 최소, 나이 가드, 유휴 완화), `Consensus.BatchTunerConfig` 주입, 메트릭 4개. 전체 스위트 129s | `AdaptiveBatch` |
| 2026-11-03 | 4 | feat/pipeline-window | [#4](https://github.com/s-xoxo/SmartBFT/pull/4) | 323db6b2 | View 를 seq 별 슬롯 윈도우로 재구성(수용 `[low, low+W]`, 처리 `[low, low+W)`, D2 접두사 규칙, in-order Deliver, `advanceLow`), 리더 토큰 cap-W 세마포어 + `proposerLoop`(R1–R7), 요청 풀 예약, `InFlightData` seq 키, W>1 전용 pre-prepare 재전송·중복 로그 분기, W>1+WAL in-flight 재시작 error 거부. W=1 메시지 히스토그램 동일성 테스트(golden = baseline 태그). 전체 스위트 150s | `PipelineDepthMax` |
| 2026-11-04 | — | fix/pipeline-resend-noise | [#5](https://github.com/s-xoxo/SmartBFT/pull/5) | db03681c | W>1 전용: pre-prepare 재전송 대상을 "prepare 정족수 미달 슬롯" 으로 축소, prev 링의 중복 pre-prepare 로그 Debug(W=1 Warn 유지). 1차 벤치의 Warn 소음 제거. 전체 스위트 155s | `PipelineDepthMax` (>1 에서만) |

PR 본문 사본: `docs/pr/`. 게이트 결과는 각 PR 본문과 코멘트에 기록.

병합 후 main d9d36397 전체 게이트 (2026-11-02): `go test ./test -count=1` → `ok 145.490s`.
병합 후 main 323db6b2 전체 게이트 (2026-11-03): `go test ./test -count=1` → `ok 151.084s`.
병합 후 main db03681c 전체 게이트 (2026-11-04): `go test ./test -count=1` → `ok 151.331s`.

## fabric 포크

| 날짜 | 내용 |
|---|---|
| 2026-11-02 | baseline 태그 = upstream main 98dc3b24. 변경 없음 |

## 다음

- 3 `feat/adaptive-batch`, 4 `feat/pipeline-window` — 상세 설계 문서 작성 완료 (`docs/notes/design-adaptive-batch.md`, `docs/notes/design-pipeline-window.md`, 2026-11-02). 리뷰어 결정 반영 완료 (`docs/notes/decisions-2026-11-02.md`). feat 3 병합 완료 (2026-11-02). feat 4 병합 완료 (2026-11-03, #4).
- 다음: 5 `feat/state-wal-window` (WAL 다중 in-flight 복원, W>1 재시작 거부 해제), 6 `feat/viewchange-multi-inflight` (ViewData 다중 in-flight, D1). 7 `feat/adaptive-depth` 는 feat 4 벤치 결과 뒤에 설계.
- 2026-11-04 병합 후 리뷰 반영 (`docs/notes/decisions-2026-11-04.md`): R8 기록, D1 코드 확인(InFlightData 는 뷰 체인지 경로에서 미삭제 → feat 6 에 `clearAbove(low)`), feat 5 결정(슬롯별 WAL 레코드 + 경계 이후 전부 복원, truncate = low 슬롯 커밋 시, 복원 토큰 `W − restored`, 병합 기준 = 다중 in-flight 재시작 복원 테스트). 순서 feat 5 → 6 고정.
- 2026-11-04 1차 벤치 (`bench/`, `results/2026-11-04-first-trend/`): RTT 50/100 ms 에서 TPS 가 W 에 선형(W=8, K=1024 에서 8×). Deliver 소요 ≈ 0.1 % → Decide 블로킹은 병목 아님. RTT 0 셀은 CPU 경합으로 무효.
- 2026-11-04 4라운드 (`docs/notes/decisions-2026-11-04.md` 4라운드): D1 승인 — `clearAbove` 는 `changeView` 인자 `newProposalSequence`(sync 뒤 새 low) 를 쓰고 feat 6 에 `TestInFlightClearedAfterSyncAndViewChange` 추가; 논문 구현 장 단락 초안("암묵적 단일 in-flight 불변식"); 재전송 Warn 은 `fix/pipeline-resend-noise` PR #5 로 병합(브랜치 정책에 `fix/` 추가); feat 5 `Restore` 에 체크포인트 필터(`seq ≤ Checkpoint` 버림). **feat 5 설계 문서 `docs/notes/design-state-wal-window.md` 작성 완료 → 리뷰 대기** (열린 질문 5개). feat 6 은 D1·sync 순서 포함 설계 리뷰 1회 더.
- feat 4 가 남긴 한계(브랜치 5/6/8 범위): W>1 in-flight 재시작은 error, 뷰 체인지는 최저 in-flight 1건만 복구, config barrier 미구현. 재전송 틱 = 하트비트 주기(6 s)로 netem 손실 실험에서 p99 에 드러날 수 있음 — gap-triggered 재전송은 후속 후보.
