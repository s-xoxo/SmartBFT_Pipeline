# 설계 리뷰 결정 목록 (2026-11-02)

feat 3 (`feat/adaptive-batch`), feat 4 (`feat/pipeline-window`) 설계 문서에 대한 리뷰어 결정. **PR 본문에 그대로 붙인다.**

## feat 4 — design-pipeline-window.md

| # | 항목 | 결정 |
|---|---|---|
| (b) | baseline 에 주기적 재전송 없음 | 확인·정정 동의. pre-prepare 재브로드캐스트는 W>1 전용(`resendTick` nil at W=1). **W>1 에서도 1차 복구 경로는 sync 이고 재전송은 지연 단축용 보조** — 안전성 논증은 재전송에 의존하지 않음 |
| (c) | W=1 편차 3개 (prev-seq 응답 슬롯 정확, 죽는 뷰로의 제안 억제, 늦은 중복 pre-prepare Warn 1회) | 승인, 조건: **메시지 종류별 히스토그램 동일성 테스트** 추가. `MutateSend` 훅으로 pre-prepare/prepare/commit/heartbeat/view-change 수를 세어 baseline 태그와 W=1 main 이 `TestBasic`·`TestNodeInFlightThenViewChange` 에서 같음을 단언 (golden 파일 방식, 타이밍 의존 메시지는 범위로) |
| ① | 재전송 틱 | 하트비트 주기, 새 설정 필드 없음. **한계 기록**: 6 s 는 라운드(ms)보다 길어 netem 손실 실험 p99 에 드러남. 후속 브랜치 후보: gap-triggered 1회 재전송(리더가 슬롯 k+1 prepare 는 받았는데 k 는 못 받은 노드에게 k 의 pre-prepare 를 노드·슬롯당 1회) |
| ② | pre-prepare Save 의 `truncateTo` | `true` 유지 (브랜치 5 가 바꿈) |
| ③ | `Controller.Decide` 블로킹 | 유지. **한계 기록**: Deliver 완료까지 뷰 고루틴 블록 → 다른 슬롯 메시지가 incMsgs 에 적체. W 를 키워도 TPS 가 안 오르면 첫 용의자. 판별용으로 Deliver 소요(`view_latency_batch_save`, PR #2 에서 의미 명시)를 라운드 지연과 함께 그래프에 |
| ⑩ | 팔로워 수용 구간 W+1 칸 | 권고대로 (W=1 에서 {S, S+1} 환원이 근거) |
| (a) | DecisionsInView 예측식 | **Lemma 추가**: 팔로워 검사식과 리더 예측식이 같은 식. 새 뷰 초기값에서도 성립 — `changeView(view, seq, dec)` 가 세 값을 함께 받아 `startView` 가 같은 쌍으로 View 를 만들며(controller.go:386-414, 363-384), 호출 4곳(:491 새 뷰 dec=0, :503 sync, :510 뷰 재시작, :561 로테이션)이 모두 노드 간 동일한 입력에서 쌍을 유도. 뷰 시작 쌍의 일치는 baseline 이 이미 요구 |
| (d) | 한계 2개 (W>1 재시작 미지원, >1 in-flight 뷰 체인지 단일 복구 폴백) | 승인. **추가**: W>1 + in-flight 상태의 재시작을 `Restore`/`Start` 에서 명시적으로 거부 (**error, panic 아님**) |
| 기타 | ④~⑨, ⑪, ⑫ | 권고대로 |

## feat 3 — design-adaptive-batch.md

| # | 항목 | 결정 |
|---|---|---|
| 수정 | 지연 가드 기준선 "전 구간 최소" | **수정 요청**: RTT 가 영구 증가(netem 50→100 ms)하면 가드가 계속 발동해 C 가 천장에 갇힘. **최근 100 결정 슬라이딩 윈도우 최소** 또는 EWMA 쪽으로 천천히 감쇠하는 최소로 변경 (권고: 슬라이딩 윈도우 최소, 윈도우 크기는 TunerConfig) |
| 주의 | LAN 에서 가드가 정상 배치 성장을 혼잡으로 오인 가능 | 설계 의도와 일치하나 LAN TPS 가 baseline 보다 낮으면 이 가드 때문. **γ 를 실험 변수로**(기본 2.0), `guard_trips{reason}` 를 그래프에 함께 표시 |
| 발견 | 팔로워 측 배치 크기 상한 없음 | **논문에 필수 기재**: 팔로워도 Fabric verifier 도 배치 크기를 검사하지 않으므로 비잔틴 리더는 baseline 에서도 임의 크기 배치 가능 → 적응형 제어는 공격 표면을 추가하지 않음. "W_max 초과 감지 → 뷰 체인지" 실험은 배치 크기가 아니라 **파이프라인 깊이(수용 구간 밖 seq 폐기)에만** 적용. 평가 설계에 반영 |
| ③ | `C_init = C_cap/4` | 권고대로. 램프 1.25^6 ≈ 3.8배, k=3 이면 약 18 결정에 C_cap 도달. **벤치 warm-up 최소 30 결정** |
| ④ | 튠 파라미터는 `TunerConfig` 에만 | 권고대로, 조건: **벤치·하네스에서 주입 가능**해야 함 (k, γ, 배율 스윕). `types.Configuration` 비노출과 양립 |
| ⑩ | T 연장 규칙 유지, φ_hi=1.0 으로 끄기 | 권고대로 |
| 기타 | atomic.Pointer 스냅샷, MaxSizeBytes 고정, 풀 getter 3개, dead band ×3 > step ×1.25, 10·T_0 유휴 완화, 나이 가드(0.5×Forward → drain) | 권고대로 |

## 브랜치 목록 변경

`feat/adaptive-depth` 를 6 뒤·구 7 앞에 추가 (논문 주제 "배치 크기 **및 파이프라인 깊이** 동적 조절"). 신호: in-flight 슬롯 점유율·라운드 지연, 상한 `PipelineDepthMax`, 규칙은 C 의 AIMD 골격 재사용. 설계는 feat 4 결과를 본 뒤.

## 착수 조건

- feat 3: 지연 가드 기준선 수정 → 구현 착수
- feat 4: 메시지 히스토그램 동일성 테스트, W>1 재시작 명시적 거부(error) → 구현 착수. feat 3 병합 후 (controller.go·requestpool.go 겹침)
- 두 PR 본문에 이 결정 목록을 그대로 붙임
