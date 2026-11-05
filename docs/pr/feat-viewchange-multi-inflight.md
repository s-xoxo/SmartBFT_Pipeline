# feat/viewchange-multi-inflight — 뷰 체인지의 다중 in-flight 복구와 D1 (feat 6)

- 브랜치: `feat/viewchange-multi-inflight` (base `main` = `aab360af`, feat 5 PR #6 포함)
- 설계: `SmartBFT_Pipeline/docs/notes/design-viewchange-multi-inflight.md` (§1–13 초안, §14–15 리뷰 1차, §16 결정 2건, §17 구현 메모). 결정 기록: `docs/notes/decisions-2026-11-04.md` 7·8라운드
- 커밋 (각 커밋 W=1 에서 빌드·vet·lint green):

| # | 커밋 | 내용 |
|---|---|---|
| a | `8f0da591` util, controller, state: discard the in-flight suffix at a view start (D1) and keep the in-flight data exact | `InFlightData.clearAbove/inFlightEntries`, `StorePrepares` view 검사, `changeView` 의 `clearAbove(newProposalSequence)`, `Restore` 의 VC-마지막 복원(InFlightData 만, upstream #748) + 등록 검증(metadata·commit view/seq/digest/signer), 단위 테스트 4개 |
| b | `9cdbcae9` proto, viewchanger: carry every proposal in flight through a view change and recover their prefix | `InFlightEntry`, `repeated in_flight = 6`(4/5 유지), `prepareViewDataMsg` 다중 송신, `ValidateInFlightEntries`(혼합 배포 거부 포함), `CheckInFlightPrefix`(seq 별 A1/A2/B), `processNewViewMsg` 접두사 커밋 반복, in-flight View 에 `PipelineDepth`, 단위 테스트 3개 |
| c | `19a6f812` test: view changes with several proposals in flight | 통합 테스트 5개 (아래), 직렬 실행, 하네스 `viewChangeCluster`: 수신 측 commit 유실 + 테스트가 공급하는 하트비트 클록(리더 침묵으로 뷰 체인지 유발, 4 노드 생존) |
| d | `85464bfb` view: read head under the lock in the pre-prepare resend | PR #5 의 `resendPrePrepares` 가 `head` 를 잠금 없이 읽던 경쟁(-race 가 뷰 체인지 테스트에서 검출) 수정 |
| e | `8de81083` test: the leader restart test loses commits at the leader instead of holding the followers' | feat 5 의 `TestPipelineLeaderRestartWithInFlight` 가 송신 측 commit 보류로 비결정적이던 것을 리더 수신 측 유실 + 로그 순서 단언으로 교체 |
| f | `42aa6314` batcher: read the close channel under its lock in Closed | PR #4 의 `proposerLoop` 가 `Batcher.Closed()` 를 잠금 없이 읽어 `changeView` 의 `Reset` 과 경쟁(-race 검출) 수정 |

변경 규모: 16 files, +1476 / −191 (커밋 f 포함). 라이브러리: `viewchanger.go` +232/−90, `state.go` +93/−2, `util.go` +51/−1, `controller.go` +5, `view.go` +4/−1, `consensus.go` +1, `messages.proto` +10 (+ 생성 코드 +144/−73). 테스트: `viewchanger_window_test.go` +364(신규), `pipeline_viewchange_test.go` +297(신규), `state_window_test.go` +107, `pipeline_window_test.go` +64/−19, `controller_window_internal_test.go` +54, `util_test.go` +33, `state_test.go` +3/−2.

## 무엇을

뷰 v 에서 W 개까지 in-flight 였던 제안을 뷰 v+1 로 넘긴다: (a) 어떤 정직 노드가 커밋했을 가능성이 있는 seq 는 모두 같은 값으로 보존(접두사), (b) 그 이후 접미사는 모든 노드에서 파기(D1), (c) 접두사를 새 뷰 시작 전에 순서대로 커밋한다.

- **D1** (`controller.go changeView`): `startView` 직전 `c.InFlight.clearAbove(newProposalSequence)` — 인자(새 low)를 쓰므로 sync 경로에서는 `clearUpTo(S) → clearAbove(S+1)` 순서가 되어 남는 집합 ⊆ `{S+1}`(baseline 이월 항목). `> low` 만 삭제해 W=1 은 no-op.
- **ViewData** (`messages.proto`): `InFlightEntry{proposal, prepared}`, `repeated in_flight = 6`. 4/5 유지; W>1 이면 6[0] 을 반복. W=1 은 6 을 채우지 않아 **바이트 동일**.
- **송신** (`prepareViewDataMsg → getInFlightEntries`): 체크포인트 초과·`ckpt+1` 시작·연속·≤ W. **수신** (`ValidateInFlightEntries`, `validateViewDataMsg`·`validateNewViewMsg` 전 분기): 연속성·길이·4/5 와 6[0] 일치, **혼합 배포 거부**("필드 6 있음 & 내 W=1", "4/5 만 & 내 W>1") + Warn.
- **판정** (`CheckInFlightPrefix`): `s = base+1, base+2, …, base+W` 순회, seq 별로 기존 조건 그대로 — A2 `preprepared ≥ f+1`(prepared 보고가 1개 이상인 제안을 받은 노드 수, prepared 여부 무관 — baseline 과 동일), A1 `noArgument ≥ quorum`, B `noInFlight ≥ quorum`. A 면 보존·계속, B 면 중단(그 위는 접미사), 둘 다 아니면 `ok=false`(재시도). `CheckInFlight` 는 depth 1 래퍼로 유지.
- **커밋 반복** (`processNewViewMsg`): 접두사를 순서대로 `commitInFlightProposal`; 체크포인트가 이미 지난 제안은 skip, 간극이면 포기(재시도). `commitInFlightProposal` 은 체크포인트가 제안을 넘어섰으면 Panic 대신 skip. in-flight View 에 `PipelineDepth` 를 넘겨 위 seq 의 commit 이 sync 트리거가 되지 않게 함.
- **복원 접합** (`state.go`): WAL 마지막이 ViewChange 레코드면 떠나는 뷰(레코드 `next_view`)의 PP/C 를 **`InFlightData` 에만** 복원(View 는 복원 안 함) → 재시작 후 다시 보내는 ViewData 가 prepared 증거를 싣는다(S2, upstream #748). 복원 등록 검증 5항: metadata(view, seq) 일치, commit 의 view/seq/digest/signer 일치, `StorePrepares` 의 view 검사. 불일치는 Warn + commit 폐기(PROPOSED 강등) 또는 그 seq 이후 폐기; 뷰 불일치로 생긴 간극은 error 대신 보수적으로 버림.

## 안전성 (S1/S2/S3 ↔ 코드)

| 명제 | 보장 | 코드 |
|---|---|---|
| S1 결정 보존 | 전달된 값은 base 이하면 `last_decision` 으로, 초과면 f+1 정직 prepared 보고 → A | `maxLastDecisionSequence`, `CheckInFlightPrefix` A 분기, `validateNewViewMsg` "ahead by one → deliver" |
| S2 커밋 가능 값 보호 | 정직 f+1 prepared ⇒ noInFlight ≤ 2f < quorum ⇒ **B 불성립**(Lemma 1) | `CheckInFlightPrefix` B 분기 |
| A2 ≥ f+1 | 라이브니스: 바이잔틴 f 개가 거짓 in-flight 로 재제안을 강제하지 못함 | A2 분기 |
| S3 순서·비확정 | 연속·`ckpt+1` 시작; A 도 B 도 아니면 `ok=false` → 재시도(다른 2f+1 집합에 의존) | `ValidateInFlightEntries`, `ok=false`, 접두사 루프 간극 → return |
| D1 | 접두사 이후 상태 파기: `InFlightData` 는 `clearAbove`, View 슬롯·큐는 인스턴스 폐기, WAL 은 NV/VC 경계 | `changeView`, `Restore` |

W=1: ViewData 바이트 동일(필드 6 비움), `CheckInFlightPrefix` 는 1회 순회로 기존 판정과 동일, `clearAbove` no-op, VC-마지막 복원은 upstream #748 과 같은 동작(편차 5번으로 기록). `TestMessageHistogramMatchesBaseline` 통과.

## 테스트

단위 (`internal/bft`): `TestInFlightDataClearAboveAndEntries`, `TestInFlightClearedAfterSyncAndViewChange`(sync 전부/일부/W=1/접두사 결정), `TestRestoreAfterViewChangeRecordKeepsPrefixInFlight`(5 케이스), `TestRestoreRejectsMismatchedCommit`(6), `TestCheckInFlightPrefix`(10: 전체 창, B 중간, 단독 prepared 보고, pre-prepared 집계, A·B 모두 아님, 순차 ViewData, depth 1), `TestValidateInFlightEntries`(12, 혼합 배포 양방향), `TestPrepareViewDataMultiInFlight`(3, W=1 동일).

통합 (`test/`, W=3): `TestMultiInFlightThenViewChange`(접두사 3 커밋, 옛 리더 재합류), `TestRepeatedLeaderChangesKeepDecisions`(**S1 실험 검증**, 연속 2회 리더 교체), `TestLaggingLeaderSyncsThenViewChanges`(**L1/L2**: 2 뒤처진 새 리더 → 뷰 1 타임아웃(5 s) → 뷰 2 → sync 후 합류, 뷰 3 없음), `TestPipelineRestartThenViewChangeCarriesRestored`(복원 2개가 ViewData 에), `TestRestartDuringViewChangeReportsPrefix`(**#748 케이스**: VC 레코드 뒤 재시작 → 접두사 3개 InFlightData 복원 → 재전송 ViewData 로 뷰 체인지 완료). 혼합 배포 거부는 단위 `TestValidateInFlightEntries` 로.

구현 중 발견: (1) 송신 측 commit 보류(`holdCommits`)는 W>1 에서 같은 링크의 다음 prepare 를 막아 경합을 일으킴 → 뷰 체인지 테스트는 수신 측 commit 유실로, feat 5 의 리더 재시작 테스트는 리더 수신 측 유실로 전환(커밋 e); 전체 스위트에서 그 테스트가 멈춘 뒤 보류된 송신 고루틴이 네트워크 종료까지 막는 것도 확인. (0) `resendPrePrepares` 의 잠금 없는 `head` 읽기 경쟁(커밋 d). (2) 뷰 시작 경쟁: 새 리더의 첫 pre-prepare·prepare 가 팔로워의 새 뷰 설치 전에 도착하면 옛 View 에서 버려진다(baseline 구조). PR #5 재전송은 pre-prepare 만 복구하므로 리더가 분리된 3-노드 상황에서는 그 팔로워의 prepare 가 없어 정족수를 못 채우고 재불만 → 연쇄 뷰 체인지. 테스트는 리더를 분리하지 않고 **하트비트 클록을 침묵**시켜(4 노드 생존) 유발한다. 설계 §17 에 기록, 후속 후보(미래 뷰 메시지 보류 등). (2) A2 의 `preprepared` 는 baseline 과 같이 "그 제안을 받은 노드 수"(prepared 플래그 무관)이며 후보는 prepared 보고 1개 이상인 제안 — 설계 §14.1 의 서술을 코드에 맞춰 §17 에 정리. (3) 뒤처진 새 리더는 ViewData 거부로 뷰를 완성하지 못하고 `ViewChangeTimeout` 뒤 다음 리더로 넘어가며, 그 리더의 하트비트로 sync 되어 합류한다(설계 §7 1차안 그대로, L1 로 확인).

## 게이트

```
go build ./... && go vet ./...                      ok
golangci-lint run ./...                              0 issues
mockery (drift)                                      변경 없음
go test -count 1 -race ./internal/... ./pkg/...      ok (bft 10.3s, api, types, wal)
go test ./test/ -count=1                             ok 222.289s (뷰 체인지 테스트 5개 직렬 포함)
go test ./test/ -run <신규 5> -count=2               ok 64.4s (초기 버전), 개별 -count=3 통과
go test ./test/ -race -run <신규 5 + 리더 재시작 + 히스토그램>  ok 191.767s (DATA RACE 없음; 그 전 2건은 커밋 d·f 로 수정)
```

## 플래그

`PipelineDepthMax` — 1(기본)이면 ViewData 바이트·판정·D1 모두 동일. VC-마지막 복원(upstream #748 동작)만 W 무관 적용.

## 참고

- 병합 기준(8라운드): §14 체크리스트 5 + L3 + #748 테스트 + 혼합 배포 거부 테스트 — 모두 포함.
- 후속: §14.2(e) 같은 뷰 번호 인스턴스 재시작 충돌(기록만), ViewChangeTimeout 측정(§6).
