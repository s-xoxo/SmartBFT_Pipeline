# fix/view-start-race — 리더 재전송에 자기 prepare 포함 (뷰 시작 경쟁)

- 브랜치: `fix/view-start-race` (base `main` = `e845b957`, feat 6 PR #7 포함)
- 커밋: `8841c231` view: resend the leader's prepare with each open pre-prepare
- 배경: feat 6 통합 테스트에서 리더를 분리하면 새 리더의 첫 pre-prepare·prepare 가 팔로워의 새 뷰 설치 전에 도착해 옛 View 에서 버려지고(`Controller.ProcessMessages` 는 현재 View 로만 라우팅, 미래 뷰 버퍼 없음), PR #5 재전송은 pre-prepare 만 복구해 남은 두 팔로워로는 prepare 정족수(3)를 못 채워 2 s 뒤 재불만 → 연쇄 뷰 체인지. 논문의 리더 크래시 실험에서 피할 수 없는 경로(리뷰 9라운드 (b)).

## 무엇을

`View.resendPrePrepares`: prepare 정족수 미달 슬롯(`phase < slotPrepared`)마다 `slot.prePrepare` 에 이어 **리더 자신의 prepare(`slot.lastBroadcastSent`, 슬롯이 PROPOSED 일 때)** 를 재방송한다. 팔로워 측 변경 없음: 같은 pre-prepare 는 슬롯당 1개 규칙으로, 중복 prepare 는 `voteSet` 의 송신자별 1표 규칙으로 걸러진다.

## 왜 가장 작은 변경인가

대안 (i) 재전송 주기를 하트비트와 분리한 설정, (ii) 컨트롤러가 `currViewNumber+1` 메시지를 보류, (iii) 새 리더가 첫 제안을 팔로워 설치 확인 뒤로 지연 — 모두 새 상태·설정·메시지가 필요하다. prepare 재방송은 기존 재전송 루프에 메시지 1종을 더하는 것이고, 주기(하트비트 주기)와 대상(정족수 미달 슬롯) 규칙은 PR #5 그대로다.

## W=1 동일성

`resendTick == nil` 이라 호출 없음. 메시지 집합·로그 동일(`TestMessageHistogramMatchesBaseline` 통과).

## 테스트

- 단위: `TestViewLeaderResendsPrePrepares`(두 서브테스트가 재전송마다 pre-prepare → prepare 순서와 총 개수 13/9 를 단언), `TestViewStartsFromRestoredSlots`(복원된 PROPOSED 슬롯도 pre-prepare + prepare 재전송).
- 통합: **`TestMultiInFlightThenLeaderDisconnect`** — 3개 prepared in-flight 상태에서 리더 **분리**(3 노드 생존), 팔로워 불만으로 뷰 체인지, 재전송 500 ms; 새 뷰 첫 seq 완료, 추가 2 seq, "Starting view with number 2" 0건. 3회 연속 통과(수정 전에는 같은 시나리오가 전체 스위트에서 연쇄 뷰 체인지로 실패).

## 게이트

```
go build ./... && go vet ./...                      ok
golangci-lint run ./...                              0 issues
go test -count 1 -race ./internal/... ./pkg/...      GATE_UNIT
go test ./test/ -count=1                             GATE_FULL
go test -race ./test/ -count=1                       GATE_FULLRACE   (branch-policy §4 신규 게이트)
```

## 플래그

`PipelineDepthMax` — 1(기본)이면 변경 없음.

## 참고

설계 §17–18(뷰 시작 경쟁 실측·후속 후보), 결정 9라운드. Fabric 실험의 리더 크래시 복구 시간 측정은 이 수정이 전제.
