# fix/pipeline-resend-noise — W>1 pre-prepare 재전송 범위 축소와 중복 로그 레벨

- 브랜치: `fix/pipeline-resend-noise` (base `main` = `323db6b2`, feat 4 PR #4 병합 직후)
- 커밋: `59f4204c` view: narrow the pre-prepare resend and quiet its duplicates at depth above one
- 변경 규모: 2 files, +74 / −4 (`view.go` +12/−4, `view_window_internal_test.go` +62)
- 배경: 1차 벤치(`SmartBFT_Pipeline/results/2026-11-04-first-trend/`) 에서 W>1, RTT 50/100 ms 셀마다 `Got pre-prepare for sequence N but we're in sequence N+k` Warn 6–12건. 손실·뷰 체인지 없음. 리뷰어(2026-11-04 4라운드) 지시로 지금 처리.

## 무엇을

둘 다 **깊이 W > 1 에서만** 동작이 달라진다.

1. **재전송 대상 축소** (`View.resendPrePrepares`): 제외 조건 `slot.phase >= slotCommitted` → `slot.phase >= slotPrepared`. 리더가 prepare 정족수를 모은 슬롯은 더 재전송하지 않는다.
2. **prev 링의 pre-prepare 로그** (`View.handlePrevSeqMessage`): 이미 전달한 seq 의 pre-prepare 가 오면 W>1 은 Debug, W=1 은 기존 Warn 유지.

## 왜

- 재전송은 "pre-prepare 를 놓친 팔로워가 간극을 메워 그 위를 prepare 할 수 있게" 하는 것이 목적이다(design-pipeline-window.md §5). 리더에 prepare 정족수가 모였다면 정족수 노드 전부가 그 pre-prepare 를 갖고 있고, D2 접두사 규칙으로 그 아래 seq 도 전부 갖고 있다. 정족수 밖 ≤ f 노드는 prev 링 응답과 sync 로 따라오는 baseline 경로를 그대로 쓴다. 재전송 노출 창이 한 라운드(제안 → 커밋)에서 한 RTT(제안 → prepare 정족수)로 줄어, 재전송이 전달 완료 노드에 닿는 경우가 거의 없어진다.
- 그래도 닿는 경우는 결함이 아니라 재전송의 예상된 지연 도착이므로 Warn 이 아니다. 실험 로그가 Warn 으로 덮이면 진짜 경고(검증 실패, 뷰 체인지)를 놓친다.

## 안전성·W=1 동일성

- 메시지 집합: W=1 은 `resendTick == nil` 이라 `resendPrePrepares` 가 호출되지 않는다(§5.3). 로그 분기도 `depth() > 1` 조건. **W=1 메시지 흐름·로그 모두 바이트 동일.** `TestMessageHistogramMatchesBaseline` 그대로 통과.
- W>1 안전성: 재전송은 라이브니스 보조 수단이며 안전성 논증(Lemma 1–5)은 재전송에 의존하지 않는다. 축소로 잃는 것은 "정족수 밖 노드가 재전송으로 pre-prepare 를 얻는 경우" 이고, 그 노드는 baseline 과 같이 prev 링(prepare/commit 응답)과 `discoverIfSyncNeeded` → sync 로 따라온다.

## 테스트

- `TestViewLeaderResendsPrePrepares/"depth 2 does not resend prepared sequences"`: 두 슬롯 제안 → 첫 슬롯에 prepare 정족수 → 틱 2회 → 둘째 슬롯만 재전송, 브로드캐스트 총 7건.
- `TestViewPrevSeqPrePrepareLogLevel`: `low−1` 의 pre-prepare 에 대해 depth 1 → Warn, depth 2·4 → Debug, 응답 없음.
- 기존 유지: `TestTwoPrePreparesInARow`(W=1 Warn 2종), `TestViewSlotRouting`(prev 링 pre-prepare 로그 2건, 레벨 무관), `TestViewDuplicatePrePrepareLogLevel`.

## 게이트

```
go build ./... && go vet ./...                      ok
golangci-lint run ./...                              0 issues
go test -count 1 -race ./internal/... ./pkg/...      ok (bft 8.4s, api, types, wal)
go test ./test/ -count=1                             ok 154.800s
```

## 플래그

`PipelineDepthMax` — 1(기본)이면 변경 없음.

## 참고

- 결정 기록: `SmartBFT_Pipeline/docs/notes/decisions-2026-11-04.md` 4라운드
- 설계 문서: `SmartBFT_Pipeline/docs/notes/design-pipeline-window.md` §5.4
- 후속 후보(미착수): gap-triggered 재전송(팔로워가 간극을 리더에 알림) — netem 손실 실험 결과 뒤 판단.
