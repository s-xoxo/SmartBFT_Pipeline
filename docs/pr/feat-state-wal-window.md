# feat/state-wal-window — WAL 다중 in-flight 저장·복원 (feat 5)

- 브랜치: `feat/state-wal-window` (base `main` = `db03681c`, PR #5 fix 포함) → PR #6, merge `aab360af`
- 설계: `SmartBFT_Pipeline/docs/notes/design-state-wal-window.md` (승인 2026-11-04, §12 리뷰 결정 반영). 결정 기록: `docs/notes/decisions-2026-11-04.md` 5라운드
- 커밋 (각 커밋 W=1 에서 빌드·vet green):

| # | 커밋 | 내용 |
|---|---|---|
| a | `4b03ba2a` state, view: restore every proposal in flight from the WAL, truncating at the lowest open sequence | `Restore` fold 재작성(경계·체크포인트 필터·깊이 상한·뷰 불일치), `View.restored` 핸드오프 + `setup()` 다중 슬롯, truncateTo = `seq == low`, `InFlightSequenceInWAL` 삭제 + `Consensus.Start` 거부 제거, `wal_records_since_boundary` 게이지, 단위 테스트 6개, upstream 테스트 `TestRestartKeepsViewAndInFlightProposal` 의 첫 재시작을 `RestartSync(false)` 로(아래 편차 4번) |
| b | `8f461755` controller: issue W minus the restored proposals as leader tokens | `Proposer.RestoredCount()`, `startView` 발급 `W − m`, `TestControllerTokensAfterRestore` |
| c | `6ca7714b` test: a restart with proposals in flight restores the same slots | `TestPipelineRestartWithInFlightIsRejected` → `TestPipelineRestartRestoresInFlight`, `TestPipelineLeaderRestartWithInFlight`, `TestPipelineRestartAfterDeliveryStartsClean` |

변경 규모: 12 files, +916 / −230. 라이브러리: `state.go` +248/−131, `view.go` +45/−7, `metrics.go` +36, `controller.go` +7/−7, `consensus.go` +1/−9. 테스트: `state_window_test.go` +304(신규), `view_window_internal_test.go` +132/−40, `pipeline_window_test.go` +106/−29, `controller_window_internal_test.go` +27/−2, `state_test.go` +4/−4, `controller_test.go` +2.

## 무엇을

W>1 로 동작하던 노드가 in-flight 제안 m 개(1 ≤ m ≤ W)를 WAL 에 둔 채 죽었다가 재시작하면 **같은 슬롯 집합을 같은 단계(PROPOSED/PREPARED)로 복원**하고, 리더면 토큰을 `W − m` 개 발급한다. feat 4 의 임시 조치(W>1 + in-flight 재시작 → `Consensus.Start` error)를 제거한다.

- **레코드 형식 무변경.** `proposed_record`/`commit`/`new_view`/`view_change` 그대로. proto 변경 없음.
- **truncateTo 규칙** (`view.go processProposal`): `Save(pp, seq == v.ProposalSequence)`. low 슬롯의 PP 만 경계. W=1 은 처리 슬롯이 항상 low 라 항상 true → 바이트 동일.
- **Restore** (`state.go`): 마지막 레코드가 NV/VC 면 복원 없음(원본과 동일) → 마지막 NV/VC 경계 뒤 레코드만 디코드 → seq 별 fold(PP 짝 없는 C 는 error) → **필터**: `seq ≤ 체크포인트` 버림(Info "safely committed"), `seq > 체크포인트 + W` error(메시지에 "PipelineDepthMax 가 종료 시보다 작음, 올리거나 WAL 삭제" 안내), 경계(없으면 최저 슬롯)와 다른 뷰의 레코드는 Warn 후 버림, 연속성·`C+1` 시작 검사 → `restoredSlot` 목록을 `View.restored` 에 넘기고 `InFlightData` 에 `storeProposal/storePrepared`. 로그 "Restored proposal with sequence %d"(기존 유지) + "Restored %d proposals in flight, sequences %d to %d".
- **truncateTo 와 체크포인트 필터는 한 쌍** (코드 주석에도 명시): low PP 저장 뒤 앞 레코드는 잘리지만 low 는 미전달이라 복원 대상이 맞고, low 전달 후 재시작이면 필터가 버린다. 하나만 바꾸면 미전달 레코드 유실 또는 전달 완료 레코드 재복원(토큰 1 오차).
- **View** (`view.go`): `restored []*restoredSlot`, `restoredCount`. `setup()` 이 seq 순으로 슬롯 생성(proposal, requests, `lastBroadcastSent` = commit ?? prepare, `mySig`, phase), `head = 마지막 seq + 1`; 리더면 `slot.prePrepare` 를 레코드에서 복원해 PR #5 의 재전송 규칙(prepare 정족수 미달 슬롯)에 자연히 포함. 뷰 체인지의 단일 in-flight 핸드오프(4개 필드) 경로는 그대로.
- **Controller**: `Proposer.RestoredCount()`; `startView` 가 `W − min(m, W)` 발급 → 불변식 `len(token) + inflight + restored == W`.
- **메트릭**: `api.MetricsState.RecordsSinceBoundary` (`wal_records_since_boundary`): truncateTo 레코드 → 1, 그 외 +1, Restore 시 `len(entries)`.

## 왜

feat 6(뷰 체인지 다중 in-flight)은 "내가 WAL 에 뭘 들고 있는가" 를 `InFlightData` 에서 읽는다. feat 4 까지는 `Restore` 가 마지막 1~2 레코드만 복원해 W>1 재시작을 거부했고, 이대로 feat 6 을 하면 재시작 후 뷰 체인지가 잘못된 in-flight 집합을 보고한다. 스냅샷 레코드(`WindowMark`) 대신 슬롯별 레코드 누적 + fold 복원을 택한 이유: 레코드 형식·쓰기 지점이 그대로라 W=1 바이트 동일이 자명하고, W ≤ 8 이면 보존량이 작다(슬롯당 2 레코드).

## W=1 동일성

| 측면 | 결과 |
|---|---|
| WAL 바이트 | 동일 (truncateTo 가 W=1 에서 항상 true, 레코드 형식 무변경) |
| 메시지 집합 | 동일 — `TestMessageHistogramMatchesBaseline` 통과 |
| `Restore` 결과 | 동일 — `TestRestoreDepthOneMatchesBaseline` (proposed / prepared / committed / NV last / WAL ahead → error) |
| **편차 4번** | `[PP(n)]` 로 끝난 WAL + sync 로 체크포인트가 n 을 넘은 뒤 재시작: baseline 은 stale 제안을 PROPOSED 로 복원해 다음 seq 의 prepare 를 전부 "wrong digest" 로 거절하고 sync 로만 회복(**baseline 에서 재현함**: `results/2026-11-04-baseline-stale-restore/`, 뷰 체인지는 없음). 이 브랜치는 필터로 버린다. 정상 경로 메시지 집합 불변. **upstream 테스트 1개 조정**: `TestRestartKeepsViewAndInFlightProposal`(#739, WAL 증가 회귀 테스트)은 "정족수 전에 죽은 노드가 같은 제안을 in-flight 로 되살린다" 를 의도하지만 `Restart()` 가 sync 를 켜고 재시작해 체크포인트가 그 제안을 넘은 **뒤** 복원을 기대했다 — 즉 편차 4번의 stale 복원을 단언하고 있었다. 첫 재시작을 `RestartSync(false)` 로 바꿔 의도("아직 결정을 모르는 노드의 크래시")를 그대로 검증하고, 나머지 단언(prepare 재브로드캐스트, 같은 뷰, WAL 레코드·파일 수)은 무변경 |

## 안전성

- 복원은 **보수적**이다: 의심스러운 레코드(체크포인트 이하, 다른 뷰)는 버리고 sync 로 따라잡는다. 버려진 제안의 안전성은 뷰 체인지 쪽 논증(prepared 접두사 복구, Lemma 2)이 보장하며 재시작 노드는 "prepared 안 함" 으로 보일 뿐이다.
- 재복원된 슬롯은 뷰가 처음 받은 것과 같은 상태기계를 탄다(prepare/commit 1회 재브로드캐스트, 정족수 수집). 전달 중복은 체크포인트 필터가 막고 `MutuallyExclusiveDeliver` 가 2차 방어.
- D1: NV/VC 경계 뒤만 복원하므로 폐기된 뷰의 슬롯이 `InFlightData` 로 돌아오지 않는다(feat 6 의 `clearAbove` 와 짝).

## 테스트

단위 (`internal/bft`): `TestRestoreFoldsWindow`(9 케이스: 혼합 순서, 필터, NV 경계, NV/VC last, 뷰 불일치), `TestRestoreErrors`(5), `TestRestoreDepthOneMatchesBaseline`(5), `TestSaveCountsRecordsSinceBoundary`, `TestViewTruncatesAtLowestOpenSequence`(depth 3·1), `TestViewStartsFromRestoredSlots`(2 슬롯 복원 → commit/prepare 재브로드캐스트, 미정족수 슬롯만 재전송, 순서대로 결정), `TestControllerTokensAfterRestore`(4 조합). `TestStateRestore` 기존 케이스 유지(에러 문구 4개 갱신).

통합 (`test/`): `TestPipelineRestartRestoresInFlight`(팔로워 m=2 → "Restored 2 proposals in flight, sequences 1 to 2" → 동일 레코드 전달 → 후속 참여), `TestPipelineLeaderRestartWithInFlight`(리더 m=2, W=3 → 토큰 1 → seq 3 만 제안, 4 는 결정 뒤), `TestPipelineRestartAfterDeliveryStartsClean`(전달 직후 재시작 → "safely committed" → 복원 0 → 3개 in-flight). `TestMessageHistogramMatchesBaseline` 무변경 통과.

baseline 재현 (병합 기준 4): `TestStaleProposedRecordRestore` (baseline 워크트리에서 실행, 사본 `results/2026-11-04-baseline-stale-restore/`) — 재현됨, 뷰 체인지 없음.

## 게이트

```
go build ./... && go vet ./...                      ok
golangci-lint run ./...                              0 issues
mockery (drift)                                      변경 없음
go test -count 1 -race ./internal/... ./pkg/...      ok (bft 8.2s, api, types, wal)
go test ./test/ -count=1                             ok 155.026s
go test ./test/ -race -run <신규 3 + 히스토그램>      ok 124.554s (DATA RACE 없음)
```

## 플래그

`PipelineDepthMax` — 1(기본)이면 WAL 바이트·메시지 집합 동일, 복원 결과는 편차 4번 외 동일.

## 참고

- 열린 질문 결정(①~⑤): 설계 문서 §12. 후속 후보: WAL compaction(보존량은 `wal_records_since_boundary` 로 관측), upstream 이슈 게시(초안 `results/2026-11-04-baseline-stale-restore/upstream-issue-draft.md`).
