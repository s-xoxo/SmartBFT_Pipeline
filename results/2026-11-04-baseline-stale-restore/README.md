# baseline 재현: PP 만 남은 WAL 과 앞선 체크포인트로 재시작하면 stale 제안이 복원됨

- 날짜: 2026-11-04
- 대상: SmartBFT `baseline` 태그 (upstream main `32a628d6`), 원본 순차 프로토콜(파이프라인 없음)
- 목적: feat 5 설계 문서 §8 표의 "W=1 편차 4번" 행이 실제로 도달 가능한지, 도달하면 무엇이 일어나는지(뷰 체인지 여부) 확인 — 리뷰어 2026-11-04 결정 ②
- 재현 테스트: `stale_restore_repro_test.go.txt` (baseline 워크트리 `test/` 에 두고 실행; 라이브러리 코드 무변경, 테스트는 하네스 타임아웃에만 실패하고 관찰은 로그로 기록)
- 실행: `go test ./test/ -run TestStaleProposedRecordRestore -count=1 -v` — 3회 모두 동일 결과 (`run3-syncOnRestart-both.log`, 요약 `summary.txt`)

## 시나리오 (N=4, 노드 1 리더, 뷰 0)

1. 노드 4 가 Prepare/Commit 을 잃고 PrePrepare·HeartBeat 는 받음.
2. 요청 1 → 노드 1–3 전달. 노드 4 는 PP(1) 처리("Processed proposal with seq 1"), WAL = `[PP(1)]`, PROPOSED 에서 정지.
3. 요청 2 → 노드 1–3 전달. 노드 4 는 리더 하트비트(seq 3)로 "Heartbeat sequence is bigger than expected" → `Synchronizer.Sync` → 체크포인트 2, "Starting view with number 0, sequence 3". `sync()` 는 `fetchState` 가 같은 뷰를 돌려주므로 **NewView 레코드를 쓰지 않음** → WAL 은 여전히 `[PP(1)]`.
4. 노드 4 재시작 (`SyncOnStart` false/true 둘 다). 재시작 직전 상태 확인: WAL `[proposed_record view=0 seq=1]`, 체크포인트 seq 2, NewView 레코드 없음 (precondition=true, 6/6).
5. 요청 3, 4, 5.

## 결과

| 관찰 | 값 |
|---|---|
| "Restored proposal with sequence 1" (stale 복원) | 1 |
| 다른 노드가 받은 stale prepare(seq 1) ("got message from 4 with sequence 1 but our sequence is 3") | 3 (노드 1·2·3 각 1) |
| "Got wrong digest at processPrepares for prepare with seq 3 … we are in seq 3" | 3 |
| 노드 4 가 합의로 seq 3 전달 | **없음** (seq 4 도 없음) |
| 복구 경로 | 하트비트(seq 5) → sync 1회 → seq 3·4 를 Sync deliver 로 수령, seq 5 부터 정상 참여 |
| 뷰 체인지 | **없음** (4 노드 모두 0건; 영향 노드가 1개라 f+1 미달) |

`SyncOnStart=true` 로 재시작해도 같다: `Sync` 가 체크포인트와 같은 seq 2 를 돌려줘 아무것도 바뀌지 않고, 이어서 `startView(3)` → `NewProposer` → `Restore` → `recoverProposed` 가 Phase=PROPOSED, inFlightProposal=seq 1 을 세우고 `NewProposer` 는 `ProposalSequence` 만 3 으로 올린다(`util.go:325-328`).

원인: `state.go recoverProposed` 가 체크포인트와 비교하지 않는다(`recoverPrepared` 는 비교함). 결과: 해당 노드가 다음 seq 하나(여기서는 3, 이어서 4)를 합의로 결정하지 못하고 sync 로만 따라온다. 안전성 위반은 없다(라이브니스·효율 결함). 영향 노드가 f+1 개면 뷰 체인지로 이어질 수 있으나 본 재현에서는 발생하지 않았다.

## 포크에서의 처리

feat 5 (`feat/state-wal-window`) 의 `Restore` 체크포인트 필터(`seq ≤ checkpoint` 버림)가 W 와 무관하게 이 경로를 막는다. 설계 문서 §8 "W=1 편차 4번", 테스트 `TestRestoreFoldsWindow/"a stale pre-prepare below the checkpoint is dropped"`.

## upstream 보고

**게시됨 (2026-11-04): [hyperledger/SmartBFT#752](https://github.com/hyperledger/SmartBFT/issues/752)** — 제목 "Restored in-flight proposal below the checkpoint causes wrong-digest rejections after a restart". 본문 `upstream-issue-body.md`(재현 테스트 첨부, 수정안은 "one possible fix" 로 마지막에, 연구 언급 없음, 범위: 안전성 아님·라이브니스 지연·f+1 동시 재시작 시 불필요한 뷰 체인지 가능). 게시 전 확인: upstream main `f55a7d12`(#744–#748 포함) 에서도 같은 테스트로 재현됨(`SyncOnStart` 양쪽, precondition·증상 동일) — #748 은 NewView/ViewChange 레코드 뒤의 in-flight 복원에 체크포인트 비교를 넣었지만 `recoverProposed` 는 그대로다. #739 확인: 해당 테스트는 WAL 파일 증가 회귀 테스트(issue #736)로 #739 에서 추가·강화된 것이고, "sync 후에도 복원" 을 의도한 설명·커밋 메시지는 없음 → 조정 정당. 초안 이력: `upstream-issue-draft.md`.
