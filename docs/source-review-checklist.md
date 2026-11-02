# 원본 소스 확인 체크리스트

기준: `hyperledger-labs/SmartBFT` 커밋 `32a628d` (2026-11-01) = 포크의 `baseline` 태그. 행 번호는 이 커밋 기준.
각 절은 `probe/` 브랜치 하나에 대응. 확인 결과는 `docs/notes/<절 이름>.md` 에 기록하고 main에 커밋.

표기: `[ ]` 미확인 · `[x]` 확인 · `[?]` 확인했으나 결론 보류

---

## A. `probe/leader-token-flow` — 제안 직렬화 지점 (`internal/bft/controller.go`)

- [ ] `leaderToken` 채널의 용량과 모든 송수신 지점 나열 (`acquireLeaderToken` 761행, `relinquishLeaderToken` 769행, `run` 477행)
- [ ] `decide()` 516행에서 `acquireLeaderToken()` 이 호출되는 조건 전부 (뷰 교체됨 / 뷰 중단됨 / 로테이션 / 정상) — 각 경로에서 토큰이 누수되거나 중복 발급될 가능성
- [ ] `startView()` 363행, `changeView()` 386행에서 토큰 초기화 방식 — 뷰가 바뀔 때 in-flight 제안이 여럿이면 토큰 몇 개를 회수해야 하는지
- [ ] `propose()` 463행: `Batcher.NextBatch()` 가 블로킹인데 `run()` 루프 안에서 호출됨 → 깊이 W 로 바꾸면 run 루프가 멈추는지 (별도 고루틴 필요 여부)
- [ ] `sync()` 587행 / `maybePruneInFlight()` 695행: 동기화 후 in-flight 제안을 어떻게 버리는지 — 단수 가정이 코드에 박혀 있는 곳
- [ ] `Decide()` 887행 → `decisionChan` → `decide()`: 결정 전달이 순서 보장되는지 (채널 용량, 호출 주체)
- 결론으로 적을 것: 세마포어로 바꿀 때 건드려야 하는 함수 목록, 토큰 회수 규칙 초안

## B. `probe/view-slots` — View 의 2슬롯 구조 (`internal/bft/view.go`)

- [ ] `View` 구조체 72행: current(`prePrepare/prepares/commits`)·next(`nextPrePrepare/nextPrepares/nextCommits`) 외에 시퀀스에 종속된 필드 전부 나열 (`inFlightProposal`, `inFlightRequests`, `currPrepareSent`, `currCommitSent`, `prevPrepareSent`, `prevCommitSent`, `myProposalSig`, `lastVotedProposalByID`, `beginPrePrepare`)
- [ ] `processMsg()` 197행: 메시지 seq 를 `v.ProposalSequence` 와 비교해 current/next/prev/미래 로 분기하는 로직 — 윈도우로 바꿀 때 분기 조건
- [ ] `processPrePrepare()` 304행: `nextPrePrepare` 용량 1, 차 있으면 드롭 → 리더가 W개를 연달아 보내면 몇 개가 버려지는지 실측 (로그 삽입)
- [ ] `startNextSeq()` 864행: swap 로직 — `ViewSequences.Store` 가 "다음 제안 seq" 를 발행하는데 하트비트·sync 가 이 값을 어떻게 쓰는지 (`heartbeatmonitor.go`, `statecollector.go`)
- [ ] `handlePrevSeqMessage()` 723행: 뒤처진 노드를 위해 직전 seq 의 prepare/commit 을 재전송 — 윈도우에서는 "직전" 이 W개가 됨
- [ ] `discoverIfSyncNeeded()` 763행: 받은 메시지 seq 가 내 seq 보다 얼마나 앞서면 sync 를 트리거하는지 — 파이프라인에서는 W 만큼 앞선 게 정상이므로 임계값 조정 필요
- [ ] `voteSet` (`util.go`): 시퀀스별로 하나씩 만들 때 메모리·초기화 비용
- [ ] `decide()` 856행 → `Decider.Decide`: 순서대로만 호출되는지, 아니면 윈도우에서 out-of-order 커밋을 막는 장치를 새로 넣어야 하는지
- 결론으로 적을 것: `map[seq]*seqState` 로 바꿀 때의 상태 구조 초안, 유지해야 하는 불변식 목록

## C. `probe/prev-commit-dependency` — 직전 커밋 서명 의존성 (핵심)

- [ ] `smartbftprotos/messages.proto` `PrePrepare.prev_commit_signatures` (33행), `ViewMetadata.prev_commit_signature_digest` (110행) 의 생성 지점: `Propose()` 963행, `bindCommitSignaturesToProposalMetadata()` 991행, `metadataWithUpdatedBlacklist()` 939행
- [ ] 검증 지점: `verifyProposal()` 558행 → `verifyPrevCommitSignatures()` 611행 → `verifyBlacklist()` 654행. 각각 **무엇이 없으면 실패하는지** 정확히
- [ ] `verifyPrevCommitSignatures` 가 요구하는 `expectedSeq` 가 어떻게 계산되는지 — "직전 seq" 가 하드코딩인지
- [ ] `blacklistingSupported()` 1080행, `updateBlacklistMetadata()` 1038행: 블랙리스트가 꺼진 경우(`f` 조건) 이 검증이 통째로 생략되는지 → 생략된다면 1차 프로토타입에서 우회 경로가 됨
- [ ] `controller.go` `checkIfRotate()` 571행: 블랙리스트가 리더 로테이션 판단에 쓰이는 방식
- [ ] `Signer.SignProposal` / `AuxiliaryData` (`pkg/api/dependencies.go`): 커밋 서명에 실리는 보조 데이터가 Fabric 쪽에서 블록 메타데이터로 들어가는지 (Fabric 포크에서 확인)
- [ ] 선택지 비교표 작성:
  - (a) 윈도우 내에서는 `seq - W` 의 커밋 서명을 싣는다 (지연 증거)
  - (b) 블랙리스트 갱신을 윈도우 경계(파이프라인 drain 시점)에서만 한다
  - (c) 블랙리스트·로테이션 OFF 로 실험, 논문 한계로 명시
  - 각각: 안전성 영향 / 라이브니스 영향 / 수정 범위 / 논문에서 방어 가능성
- 결론으로 적을 것: 채택 선택지와 근거. **이 노트가 설계 장 한 절이 됨**

## D. `probe/viewchange-trace` — 뷰 체인지 (`internal/bft/viewchanger.go`)

- [ ] `ViewData` proto (65행): `in_flight_proposal` 단수, `in_flight_prepared` bool — 이 둘을 `repeated` 로 바꿀 때 서명(`SignedViewData`)·직렬화 호환성
- [ ] `prepareViewDataMsg()` 467행, `getInFlight()` 492행: in-flight 제안을 어디서 꺼내오는지 (`View.inFlightProposal`? `State`?)
- [ ] `checkLastDecision()` 568행: 2f+1 ViewData 중 last_decision 을 고르는 규칙 — 윈도우에서는 "last decision" 이 노드마다 W 범위로 다를 수 있음
- [ ] `extractCurrentSequence()` 701행: 새 리더가 재제안할 seq 를 정하는 규칙 → 다중 in-flight 용 "연속 prepared 접두사" 규칙으로 바꿀 지점
- [ ] `validateNewViewMsg()` 971행 (약 200줄): NewView 검증 — 가장 복잡. 흐름도 그릴 것
- [ ] `processNewViewMsg()` 1174행, `commitInFlightProposal()` 1252행, `decideInFlight()` 1395행: 새 뷰에서 in-flight 제안을 커밋하는 경로 — 하나만 가정한 곳 전부 표시
- [ ] `syncInFlight()` 1424행: in-flight 커밋 실패 시 sync 로 빠지는 조건
- [ ] 테스트: `viewchanger_test.go` 에서 in-flight 관련 케이스 목록 — 다중 in-flight 테스트를 추가할 템플릿으로
- 결론으로 적을 것: 수정 함수 목록, 새 복구 규칙 의사코드, 안전성 논증 초안(prepared 된 seq 는 보존, 아니면 폐기)

## E. `probe/leader-rotation` — 로테이션과 파이프라인 충돌

- [ ] `pkg/types/config.go`: `LeaderRotation` (기본 true), `DecisionsPerLeader` (기본 3)
- [ ] `controller.go` `decide()` → `checkIfRotate()` → `changeView(...)`: 결정 3개마다 뷰 재시작. 파이프라인 깊이 W > 3 일 때 윈도우 중간에서 뷰가 바뀌면 어떻게 되는지 (in-flight 제안은 D 경로로 복구?)
- [ ] 뷰 재시작 시 `RequestPool.RestartTimers()` 의 비용
- [ ] 실험 설정 결정: `LeaderRotation=false` vs `DecisionsPerLeader = k·W` — 논문에 적을 근거
- 결론으로 적을 것: 로테이션은 윈도우 경계에서만 일어나도록 하는 규칙 초안

## F. `probe/batcher` — 배치 (`internal/bft/batcher.go`, `requestpool.go`)

- [ ] `NewBatchBuilder()` 25행: `maxMsgCount`, `maxSizeBytes`, `batchTimeout` 이 어디서 읽히는지 (`consensus.go` 에서 config → builder 한 번)
- [ ] `NextBatch()` 40행: 블로킹 구조, 타임아웃 시작 시점(첫 요청 도착 vs 호출 시점)
- [ ] `Reset()` 88행: 뷰 체인지 시 배치가 버려지는지, 요청이 풀로 돌아가는지
- [ ] `requestpool.go`: 풀 길이를 O(1)로 읽을 수 있는 API 가 있는지 (`Size()`?) — 적응형 제어 신호용
- [ ] 요청 타임아웃(`RequestForwardTimeout`, `RequestComplainTimeout`, `RequestAutoRemoveTimeout`): 배치를 키우거나 W 를 늘려 지연이 늘면 complain 이 오발되는 임계값
- [ ] `pkg/metrics`: 이미 있는 메트릭 목록 — 라운드 지연·배치 크기·풀 길이 중 없는 것
- 결론으로 적을 것: `SetBatchParams()` 시그니처, 제어기가 읽을 신호 목록과 측정 지점

## G. `probe/state-wal` — 상태 저장 (`internal/bft/state.go`, `pkg/wal`)

- [ ] `State.Save/Restore` 가 저장하는 메시지 종류 (`SavedMessage` proto 113행)
- [ ] 재시작 복구 시 in-flight 제안을 하나만 복원한다고 가정하는지
- [ ] WAL 에 W 개 제안을 쓸 때 크기·fsync 비용
- 결론으로 적을 것: 윈도우 상태 저장 형식 초안

## H. `probe/test-harness` — 테스트 환경 (`test/`)

- [ ] 인메모리 네트워크 하네스의 구조: 노드 생성, 메시지 지연 주입 가능 여부, 비잔틴 노드 흉내 가능 여부
- [ ] 기존 테스트 중 "리더 크래시 중 in-flight", "뷰 체인지 중 in-flight" 케이스 — 다중 in-flight 로 확장할 후보
- [ ] 라이브러리 단독으로 TPS/지연을 잴 수 있는 벤치가 있는지 (`examples/`?) — 없으면 Fabric 없이 1차 성능 비교용 벤치를 `bench/lib/` 에 작성
- 결론으로 적을 것: 플래그 ON 테스트 추가 계획

## I. Fabric 포크 (`orderer/consensus/smartbft/`) — 별도 `probe/fabric-verifier`

- [ ] `verifier.go` `VerifyProposal`: `PreviousHash` 를 무엇과 비교하는지 (마지막 커밋 블록? 체인 헤드?) — 파이프라인에서는 "직전 제안" 과 비교해야 함
- [ ] `VerificationSequence()` 가 반환하는 값과, 제안의 `VerificationSequence` 가 다를 때의 처리 → 설정 블록 배리어가 필요한 정확한 이유
- [ ] `assembler.go` `AssembleProposal`: 블록 헤더의 `PreviousHash`·`Number` 를 어디서 가져오는지 (ledger 마지막 블록?) — 리더가 미확정 블록 위에 다음 블록을 조립하려면 수정 필요
- [ ] `chain.go` `Deliver`: 블록을 원장에 쓰는 순서 보장, `Reconfig` 반환 조건
- [ ] `synchronizer.go`: 여러 블록을 한 번에 따라잡는 경로가 있는지
- [ ] 채널 설정(`configtx`, `ConsensusConfig`)에 플래그를 추가하는 방법과 설정 변경 시 orderer 재시작 필요 여부
- [ ] Fabric 이 쓰는 SmartBFT 버전 (`go.mod`) — 포크 커밋과 일치시키기

---

## 전체 결론 문서 (`docs/notes/00-summary.md`)

A~I 끝난 뒤 작성:
1. 파이프라이닝을 막는 의존성 목록 (우선순위순)
2. 각 의존성의 해소 방안과 채택 근거
3. `feat/` 브랜치 순서 확정 (`docs/branch-policy.md` 3절 갱신)
4. 1차 프로토타입에서 포기할 것 (예: 블랙리스트) 과 논문 한계 절에 적을 문장
