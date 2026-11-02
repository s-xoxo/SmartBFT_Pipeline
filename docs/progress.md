# 진행 기록

SmartBFT 포크 main 에 들어간 변경과 그 근거 PR. 논문 구현 장에서 "이 변경은 커밋 X" 로 지칭할 때 이 표를 쓴다.

| 날짜 | 순서 | 브랜치 | PR | merge commit | 내용 | 플래그 |
|---|---|---|---|---|---|---|
| 2026-11-02 | — | baseline | — | 32a628d6 (태그) | upstream hyperledger-labs/SmartBFT main, 비교군 | — |
| 2026-11-02 | — | main | — | ea1d499d | PR 템플릿 추가 (문서) | — |
| 2026-11-02 | 1 | feat/metrics-round-latency | [#2](https://github.com/s-xoxo/SmartBFT/pull/2) | fa0ae1ad | seq 별 라운드 지연, WAL append 지연, in-flight 수, 풀 바이트, 배치 충만 사유 메트릭. 프로토콜 무변경 | 없음 |
| 2026-11-02 | 2 | feat/pipeline-config | [#1](https://github.com/s-xoxo/SmartBFT/pull/1) | d9d36397 | `PipelineDepthMax`·`AdaptiveBatch` 필드, `Validate()` 규칙(depth>1 ⇒ rotation off, W×count ≤ pool), 하네스 `Restart()` 전체 설정 유지, `newNodeWithConfig`, reconfig 변환기 | 둘 다 (기본값 원본) |

PR 본문 사본: `docs/pr/`. 게이트 결과는 각 PR 본문과 코멘트에 기록.

병합 후 main d9d36397 전체 게이트 (2026-11-02): `go test ./test -count=1` → `ok 145.490s`.

## fabric 포크

| 날짜 | 내용 |
|---|---|
| 2026-11-02 | baseline 태그 = upstream main 98dc3b24. 변경 없음 |

## 다음

- 3 `feat/adaptive-batch`, 4 `feat/pipeline-window` — 상세 설계 문서 작성 완료 (`docs/notes/design-adaptive-batch.md`, `docs/notes/design-pipeline-window.md`, 2026-11-02). 리뷰어 결정 반영 완료 (`docs/notes/decisions-2026-11-02.md`). feat 3 구현 착수 (2026-11-02), feat 4 는 feat 3 병합 후.
