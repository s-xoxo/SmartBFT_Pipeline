# 설계 노트 · probe 결론

`probe/<topic>` 브랜치에서 알아낸 내용을 `<topic>.md` 로 정리해 main에 직접 커밋합니다. 정리 후 probe 브랜치는 삭제합니다.

파일명은 probe 브랜치 이름과 맞춥니다. 예:

- `leader-token-flow.md` ← `probe/leader-token-flow`
- `viewchange-trace.md` ← `probe/viewchange-trace`
- `prev-commit-dependency.md` ← feat 5번의 설계 결정 근거

## 노트 형식

```markdown
# <topic>

- 날짜:
- 대상 커밋 (SmartBFT 포크):
- probe 브랜치:

## 질문
## 확인한 것 (코드 위치, 로그)
## 결론 / 설계 결정
## 열린 문제
```

## 작성된 노트 (2026-11-02)

| 절 | 노트 | 한 줄 |
|---|---|---|
| 종합 | [00-summary.md](00-summary.md) | 의존성 12개, 해소 방안, feat 순서 확정, 포기 항목 |
| A | [leader-token-flow.md](leader-token-flow.md) | 토큰 cap 1, 세마포어 전환 함수 14개, 회수 규칙 R1~R7 |
| B | [view-slots.md](view-slots.md) | 수용 범위 [S-1,S+1], W-2 개 영구 손실 실측, `seqState` 스케치, 불변식 I1~I11 |
| C | [prev-commit-dependency.md](prev-commit-dependency.md) | 로테이션 OFF 면 서명 체인 완전 우회, 선택지 (c) 채택 |
| D | [viewchange-trace.md](viewchange-trace.md) | `ViewChanger.InFlight` 단일 슬롯, 접두사 복구 규칙, NewView 흐름도 |
| E | [leader-rotation.md](leader-rotation.md) | 로테이션은 로컬·결정적, in-flight 미복구, 주 실험은 OFF |
| F | [batcher.md](batcher.md) | `NextRequests` 비파괴적 → 재제안 위험, `SetBatchParams` 시그니처, 신호 표 |
| G | [state-wal.md](state-wal.md) | Restore 1~2 레코드 가정, PP truncate 위험, `WindowMark` 포맷 초안 |
| H | [test-harness.md](test-harness.md) | 하네스 훅 목록, `Restart()` 플래그 초기화 함정, 플래그 ON 테스트 12개 계획, 실행 시간 |
| I | [fabric-verifier.md](fabric-verifier.md) | `LastCommittedBlockHash` 비교, `*After` 메서드 제안, 배리어 이유 4개, 플래그 배관 경로 |

## 설계 문서 (구현 전 확정용, 2026-11-02)

| 브랜치 | 문서 | 상태 |
|---|---|---|
| 3 `feat/adaptive-batch` | [design-adaptive-batch.md](design-adaptive-batch.md) | 병합 완료 (PR #3, 2026-11-02) |
| 4 `feat/pipeline-window` | [design-pipeline-window.md](design-pipeline-window.md) | 병합 완료 (PR #4, 2026-11-03), §15 병합 후 리뷰 |

결정 기록: [decisions-2026-11-02.md](decisions-2026-11-02.md) — PR 본문에 그대로 붙임.

병합 후 결정 (2026-11-04, feat 4 PR #4 리뷰): [decisions-2026-11-04.md](decisions-2026-11-04.md) — R8, 히스토그램 논문 서술, feat 5 WAL 형식·복원 토큰·병합 기준, D1 코드 확인 결과, 1차 벤치.
