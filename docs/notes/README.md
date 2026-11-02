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
