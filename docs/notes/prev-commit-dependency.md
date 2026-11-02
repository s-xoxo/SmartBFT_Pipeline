# prev-commit-dependency
- 날짜: 2026-11-02
- 대상 커밋: SmartBFT 32a628d6 (baseline), fabric 98dc3b24
- probe 브랜치: 없음 — 읽기만

> 인용 형식: `internal/bft/view.go:NNN` 은 SmartBFT 포크(baseline) 기준. fabric 포크의 `vendor/github.com/hyperledger/SmartBFT/` 는 go.mod 가 `v1.0.2` 를 가리키며, 인용한 함수들의 **로직은 동일**하고 차이는 (1) `.Field` → `.GetField()` getter 변환, (2) `State.Save(msg)` → `State.Save(msg, truncateTo bool)` 시그니처, (3) `ViewSequences.Store` 호출 위치(`processCommits` 말미 → `startNextSeq`) 뿐이다. vendor 쪽 행번호는 4~11행 작다: `verifyProposal` 554, `verifyPrevCommitSignatures` 607, `verifyBlacklist` 650, `metadataWithUpdatedBlacklist` 928, `Propose` 952, `bindCommitSignaturesToProposalMetadata` 980, `updateBlacklistMetadata` 1027, `blacklistingSupported` 1069, `controller.go` `checkIfRotate` 569. 포크 HEAD(ea1d499d)는 baseline 위에 docs 커밋 1개만 있고 본 노트가 인용하는 파일은 baseline 과 바이트 단위로 같다.

## 체크리스트

### [x] `smartbftprotos/messages.proto` `PrePrepare.prev_commit_signatures` (33행), `ViewMetadata.prev_commit_signature_digest` (110행) 의 생성 지점: `Propose()` 963행, `bindCommitSignaturesToProposalMetadata()` 991행, `metadataWithUpdatedBlacklist()` 939행

- 프로토 정의 확인. `smartbftprotos/messages.proto:33` `repeated Signature prev_commit_signatures = 4;` (PrePrepare 안), `:110` `bytes prev_commit_signature_digest = 5;` (ViewMetadata 안). 보조 데이터 포맷은 `:56-58` `message PreparesFrom { repeated uint64 ids = 1; }`.
- **생성은 두 갈래로 나뉘고 둘 다 같은 소스(`RetrieveCheckpoint`)를 읽는다.**
  1. 메타데이터 쪽 (`controller.propose()` → `c.currView.GetMetadata()` `internal/bft/controller.go:472`):
     `internal/bft/view.go:907` `GetMetadata()` 가 `ViewId/LatestSequence/DecisionsInView` 를 채운 뒤 `:924` `prevProp, prevSigs = v.RetrieveCheckpoint()`, `:931` `metadata.BlackList = prevMD.GetBlackList()`, `:933` `metadataWithUpdatedBlacklist(...)`, `:934` `bindCommitSignaturesToProposalMetadata(metadata, prevSigs)`.
     - `metadataWithUpdatedBlacklist` `:939-961`: `verificationSeq == prevProp.GetVerificationSequence() && !membershipChange` 일 때만 `updateBlacklistMetadata` 로 블랙리스트를 갱신하고, 재구성(verification seq 변화)·멤버십 변화 시엔 직전 블랙리스트를 그대로 둔다.
     - `bindCommitSignaturesToProposalMetadata` `:991-1002`: `if v.DecisionsPerLeader == 0 { ... "Leader rotation is disabled, will not bind signatures to proposals"; return metadata }` 이후 `:996` `metadata.PrevCommitSignatureDigest = CommitSignaturesDigest(prevSigs)`.
  2. 메시지 쪽 (`controller.go:474` `c.currView.Propose(proposal)`):
     `view.go:963-988` `Propose()`. `:965-967`
     ```go
     if v.DecisionsPerLeader > 0 {
         _, prevSigs = v.RetrieveCheckpoint()
     }
     ```
     `:981` `PrevCommitSignatures: prevSigs,`. 즉 **회전 OFF(`DecisionsPerLeader == 0`)면 PrePrepare 에 서명이 아예 실리지 않는다.**
- `RetrieveCheckpoint` 의 정체: `view.go:69` `type CheckpointRetriever func() (*protos.Proposal, []*protos.Signature)`, 실제로는 `pkg/types/types.go:77` `Checkpoint.Get()`. `Checkpoint` 는 proposal 1개 + 서명 슬라이스 1개만 보유(`types.go:71-75`), 깊이가 1이다. 갱신 지점은 `controller.go:1007` `med.C.Checkpoint.Set(proposal, signature)` — `Application.Deliver` 직후(`:1003`) — 와 sync 경로 `:642`, `:994`, 그리고 재시작 시 `pkg/consensus/consensus.go:137` `c.checkpoint.Set(c.LastProposal, c.LastSignatures)`.
- 리더 측 순서 보장: `controller.run()` 은 단일 goroutine 이고(`controller.go:477-514`) `decide(d)` → `Deliver` → `Checkpoint.Set` → `checkIfRotate` → `acquireLeaderToken` 뒤에야 `<-c.leaderToken` → `propose()` 가 돈다. 따라서 baseline 에서 리더가 seq N 을 제안할 때 체크포인트는 **항상 N-1** 이다. 이는 "seq-1" 을 숫자로 하드코딩한 결과가 아니라, 한 번에 하나만 in-flight 라는 구조에서 나온 **암묵적** 결과다.
- `CommitSignaturesDigest` `internal/bft/util.go:556`: `len(sigs)==0 → nil`, 아니면 `(Signer, Value, Msg)` 를 ASN.1 로 직렬화해 SHA-256.

### [x] 검증 지점: `verifyProposal()` 558행 → `verifyPrevCommitSignatures()` 611행 → `verifyBlacklist()` 654행. 각각 무엇이 없으면 실패하는지

호출 경로: `view.go:379` `prevCommits = prePrepare.GetPrevCommitSignatures()` → `:389` `requests, err := v.verifyProposal(proposal, prevCommits)`. **실패 시 처리가 치명적이다** (`:390-395`):
```go
v.Logger.Warnf("%d received bad proposal from %d: %v", v.SelfID, v.LeaderID, err)
v.FailureDetector.Complain(v.Number, false)
v.Sync.Sync()
v.stop()
return ABORT
```
즉 검증 실패 = 뷰 변경 요구(Complain) + 동기화 + 뷰 중단. 단순 drop 이 아니다.

`verifyProposal` `:558-608` 의 검사 순서와 에러 문자열:

| 순서 | 조건 (실패 시) | 에러 | 위치 |
|---|---|---|---|
| 1 | `v.Verifier.VerifyProposal(proposal)` 실패 | 앱 에러 그대로 | `:560-564` |
| 2 | `md.GetViewId() != v.Number` | `"invalid view number"` | `:572-575` |
| 3 | `md.GetLatestSequence() != v.ProposalSequence` | `"invalid proposal sequence"` | `:577-580` |
| 4 | `md.GetDecisionsInView() != v.DecisionsInView` | `"invalid decisions in view"` | `:582-585` |
| 5 | `uint64(proposal.VerificationSequence) != expectedSeq` | `"verification sequence mismatch"` | `:587-591` |
| 6 | `verifyPrevCommitSignatures(prevCommits, expectedSeq)` 에러 | 아래 | `:593-596` |
| 7 | `verifyBlacklist(prevCommits, expectedSeq, md.GetBlackList(), prepareAcks)` 에러 | 아래 | `:598-601` |
| 8 | `!bytes.Equal(CommitSignaturesDigest(prevCommits), md.GetPrevCommitSignatureDigest()) && v.DecisionsPerLeader > 0` | `"prev commit signatures received from leader mismatches the metadata digest"` | `:603-606` |

- 검사 3 이 파이프라이닝의 1차 장벽이다: `v.ProposalSequence` 는 뷰 내부 상태로 `decide()` 안의 `startNextSeq()` (`decide` `:856-862`, `startNextSeq` `:864`) 에서만 `++` 된다. 따라서 뷰는 구조적으로 seq 를 하나씩만 받아들인다 (`processPrePrepare` `:303-326` 은 current/next 두 채널만 있고, 채널이 차 있으면 `"Got a pre-prepare for %s sequence without processing previous one, dropping message"` 로 버린다).

`verifyPrevCommitSignatures` `:611-652`:
- `:612` `prevPropRaw, _ := v.RetrieveCheckpoint()` — **검증 기준 proposal 은 검증자 자신의 체크포인트.** 리더가 보낸 어떤 seq 번호와도 매칭하지 않는다.
- `:619-623` `if prevPropRaw.GetVerificationSequence() != currVerificationSeq { ... "Skipping verifying prev commit signatures due to verification sequence advancing from %d to %d"; return nil, nil }` — 재구성 직후엔 **통째로 생략**.
- `:635-649` 리더가 보낸 **모든** 서명에 대해 `v.Verifier.VerifyConsenterSig(sig, prevProp)`. 실패 시 `"failed verifying consenter signature of %d: %w"`. 이어서 `proto.Unmarshal(aux, prpf)` 실패 시 `"failed unmarshaling auxiliary input from %d: %w"`.
- **빠져 있어도 실패하지 않는 것**: 서명 **개수**. 서명 0개면 루프가 돌지 않고 빈 맵을 반환한다. 개수 하한은 `verifyBlacklist` 의 `blacklistingSupported` 조건에서만 검사된다(아래).
- **있으면 실패하는 것**: 검증자 체크포인트 proposal 과 다른 proposal 에 대한 서명. Fabric 의 `VerifyConsenterSig` 는 `verifySignatureIsBoundToProposal(sig, id, prop)` (`fabric/orderer/consensus/smartbft/verifier.go:225`, 정의 `:376`) 로 서명의 `BlockHeader` 바이트가 `prop.Header` 와 같은지 본다 → 체크포인트가 다른 seq 에 있으면 즉시 실패.

`verifyBlacklist` `:654-720`:
- `:655-662` `if v.DecisionsPerLeader == 0 { ... if len(pendingBlacklist) > 0 { return fmt.Errorf("rotation is inactive but blacklist is not empty: %v", ...) } return nil }` — **회전 OFF 면 블랙리스트가 비어 있기만 하면 통과, 서명 개수·내용 무관.**
- `:664` `prevPropRaw, myLastCommitSignatures := v.RetrieveCheckpoint()` — 역시 자기 체크포인트.
- `:671-679` 재구성 시: 블랙리스트가 직전과 다르면 `"blacklist changed (%v --> %v) during reconfiguration"`, 같으면 skip.
- `:681-688` 멤버십 변화 시: 다르면 `"blacklist changed (%v --> %v) during membership change"`, 같으면 skip.
- `:690-695`
  ```go
  _, f := computeQuorum(v.N)
  if v.blacklistingSupported(f, myLastCommitSignatures) && len(prevCommitSignatures) < len(myLastCommitSignatures) {
      return fmt.Errorf("only %d out of %d required previous commits is included in pre-prepare", ...)
  }
  ```
  — 서명 개수 하한은 **여기 한 곳**, 그리고 `blacklistingSupported` 가 true 일 때만.
- `:701-718` `blacklist{ prevMD: prevProposalMetadata(체크포인트 메타데이터), preparesFrom: prepareAcknowledgements, currView: v.Number, ... }.computeUpdate()` 와 `pendingBlacklist` 가 `slices.Equal` 이 아니면 `"proposed blacklist %v differs from expected %v blacklist"`.

### [x] `verifyPrevCommitSignatures` 가 요구하는 `expectedSeq` 가 어떻게 계산되는지 — "직전 seq" 가 하드코딩인지

- **`expectedSeq` 는 proposal 시퀀스가 아니다.** `view.go:587` `expectedSeq := v.Verifier.VerificationSequence()` — Fabric 에서는 `verifier.go:240-242` `v.VerificationSequencer.Sequence()`, 즉 채널 **config 시퀀스**(마지막 config 블록 기준). `verifyPrevCommitSignatures(prevCommits, expectedSeq)` 의 두 번째 인자 이름도 `currVerificationSeq` 다(`:611`). 용도는 "재구성이 있었는가"를 판별해 검증을 **생략**하는 것뿐(`:619-623`).
- 따라서 "직전 seq" 는 코드 어디에도 숫자로 등장하지 않는다. 의존성의 실체는 **"리더가 보낸 서명은 검증자의 `Checkpoint.Get()` 이 돌려주는 proposal 에 대한 유효 서명이어야 한다"** 이고, 체크포인트는 깊이 1(`types.go:71-75`)이며 `Deliver` 직후 갱신된다(`controller.go:1007`). baseline 에서는 한 번에 하나만 in-flight 이므로 이것이 곧 seq-1 이 된다.
- 파이프라이닝에 대한 함의 (W ≥ 2): 리더는 seq N+1 을 N 커밋 전에 제안하므로 체크포인트가 N-1 이고 N-1 의 서명을 싣는다. 팔로워는 N 을 이미 `Deliver` 했는지 여부에 따라 체크포인트가 N-1 또는 N → 전자는 통과, 후자는 `"failed verifying consenter signature of %d"` → Complain + Sync + ABORT(`:390-395`). **노드별 전달 타이밍에 따라 비결정적으로 뷰 변경이 유발된다.** 즉 의존성은 "엄격"하다기보다 **파이프라인과 양립 불가능한 형태(단일 체크포인트에 암묵 결합)** 다.

### [x] `blacklistingSupported()` 1080행, `updateBlacklistMetadata()` 1038행: 블랙리스트가 꺼진 경우(`f` 조건) 이 검증이 통째로 생략되는지 → 생략된다면 1차 프로토타입에서 우회 경로가 됨

두 개의 "꺼짐" 을 구분해야 한다.

**(1) 회전 OFF — `DecisionsPerLeader == 0`.** 이 경우 체인 의존성은 **송신·검증 양쪽에서 완전히 no-op** 가 된다:
- 송신: `Propose()` `:965-967` 서명 미포함(`prevSigs == nil`); `bindCommitSignaturesToProposalMetadata` `:992-995` digest 미설정; `updateBlacklistMetadata` `:1039-1043` `"Rotation is disabled, setting blacklist to be empty"; metadata.BlackList = nil`.
- 검증: `verifyPrevCommitSignatures` 는 호출되지만(`:593`, 조건 없음) `prevCommits == nil` 이라 루프가 돌지 않고 빈 맵 반환(체크포인트 조회·메타데이터 unmarshal 은 수행되나 결과에 영향 없음); `verifyBlacklist` `:655-662` 는 블랙리스트가 비어 있으면 즉시 `return nil`; digest 검사 `:604` 는 `&& v.DecisionsPerLeader > 0` 로 가드.
- 설정 결합: `pkg/types/config.go:176-180` `Validate()` 가 `LeaderRotation && DecisionsPerLeader == 0`, `!LeaderRotation && DecisionsPerLeader != 0` 을 모두 거부하므로 "회전 OFF ⇔ `DecisionsPerLeader == 0`" 이 보장된다. 라이브러리 기본값은 `config.go:109-110` `LeaderRotation: true, DecisionsPerLeader: 3` (ON).
- **Fabric 포크는 이미 회전 OFF 를 강제한다**: `fabric/orderer/consensus/smartbft/configverifier.go:185-187`
  ```go
  if configOptions.GetLeaderRotation() == smartbft.Options_ROTATION_ON {
      return errors.New("leader rotation must be turned off for this version or be unspecified")
  }
  ```
  `fabric/orderer/consensus/smartbft/util/util.go:59-65` 는 `ROTATION_ON` 이 아니면 `config.LeaderRotation = false; config.DecisionsPerLeader = 0`. `fabric/internal/configtxgen/encoder/encoder.go:229` 도 `Options_ROTATION_OFF` 를 기본 생성. → **Fabric 위에서 돌아가는 baseline 은 이미 이 체인 의존성을 쓰지 않는다.**

**(2) 블랙리스트 witness 부족 — `blacklistingSupported(f, myLastCommitSignatures) == false`.** `view.go:1080-1104`: 체크포인트 서명 중 `len(v.Verifier.AuxiliaryData(sig.GetMsg())) > 0` 인 것을 세어 `count > f` 이면 true, 한 번 true 가 되면 `v.blacklistSupported` 에 캐시(`:1102`, "one way trip"). false 일 때 생략되는 것은 **`:692-695` 의 서명 개수 하한 검사 하나뿐**이다. 서명 내용 검증(`:635-649`), 블랙리스트 재계산 일치(`:714-717`), digest 일치(`:603-606`) 는 그대로 수행된다. 따라서 (2) 는 우회 경로가 **아니다**.
- 단, Fabric 에서는 (2) 가 항상 false 다: `fabric/.../verifier.go:89-95` `AuxiliaryData` 는 unmarshal 만 시도하고 **무조건 `return nil`**; `signer.go:43` `SignProposal(proposal types.Proposal, _ []byte)` 는 aux 입력을 버린다; `signature.go:21-25` `Signature{IdentifierHeader, BlockHeader, OrdererBlockMetadata}` 에 aux 필드가 없다. 따라서 Fabric 에선 회전을 켜더라도 `preparesFrom` 이 항상 빈 `PreparesFrom{}` 이고 `pruneBlacklist`(`util.go:515`) 가 블랙리스트에서 노드를 복권시키지 못한다(재구성으로 제거되거나 `len > f` 트림만 가능). 반면 SmartBFT 자체 테스트 앱은 `test/test_app.go:253-255` `AuxiliaryData(msg) { return msg }`, `:273-279` `SignProposal(_, aux) → Signature{Msg: aux}` 로 aux 를 온전히 실어 `TestBlacklistAndRedemption`(`test/basic_test.go:2027`) 등을 돈다.

**결론: 우회 경로는 (1) 이며, 그것은 Fabric 소비자 입장에서 이미 현실 설정이다.**

### [x] `controller.go` `checkIfRotate()` 571행: 블랙리스트가 리더 로테이션 판단에 쓰이는 방식

- 호출: `controller.go:552-566` `decide()` 에서 `d.proposal.Metadata` 를 unmarshal 한 뒤 `:559` `if c.checkIfRotate(md.GetBlackList()) { c.changeView(c.getCurrentViewNumber(), md.GetLatestSequence()+1, c.getCurrentDecisionsInView()); c.RequestPool.RestartTimers() }` — 블랙리스트는 **방금 결정된 proposal 의 메타데이터**에서 읽는다(체크포인트가 아닌 decision 자체).
- `:571-584`
  ```go
  currLeader := getLeaderID(view, c.N, c.NodesList, c.LeaderRotation, decisionsInView-1, c.DecisionsPerLeader, blacklist)
  nextLeader := getLeaderID(view, c.N, c.NodesList, c.LeaderRotation, decisionsInView, c.DecisionsPerLeader, blacklist)
  shouldWeRotate := currLeader != nextLeader
  ```
- `internal/bft/util.go:72-100` `getLeaderID`: `:86-88` `if !leaderRotation { return nodes[view%n] }` → 회전 OFF 면 블랙리스트 인자가 무시되고 `currLeader == nextLeader` 가 항상 성립, 절대 회전하지 않는다. 회전 ON 이면 `index := (view + decisionsInView/decisionsPerLeader) + i` 를 순회하며 블랙리스트에 없는 첫 노드를 고른다(모두 블랙리스트면 panic `"all %d nodes are blacklisted"`).
- 뷰 시작 시의 리더 계산도 같은 함수: `controller.go:213` 이 `c.blacklist()`(`:134-141`, 체크포인트 메타데이터의 BlackList) 를 넘긴다.
- 파이프라이닝 함의: `checkIfRotate` 는 `decide()` 당 1회, `decisionsInView` 증분 직후 호출된다. W>1 에서 결정이 몰려 들어와도 각 decision 마다 호출되므로 회전 경계 판정 자체는 바뀌지 않지만, **회전이 트리거되면 `changeView(view, md.LatestSequence+1, ...)` 로 뷰를 재시작**하므로 윈도우 안에 남아 있던 in-flight proposal 들이 모두 버려진다. 회전 ON + 파이프라인은 drain 없이는 양립하지 않는다.

### [x] `Signer.SignProposal` / `AuxiliaryData` (`pkg/api/dependencies.go`): 커밋 서명에 실리는 보조 데이터가 Fabric 쪽에서 블록 메타데이터로 들어가는지

- 인터페이스: `pkg/api/dependencies.go:51` `SignProposal(proposal bft.Proposal, auxiliaryInput []byte) *bft.Signature`, `:62` `VerifyConsenterSig(signature bft.Signature, prop bft.Proposal) ([]byte, error)` (반환 `[]byte` 가 aux), `:70` `AuxiliaryData([]byte) []byte`.
- 라이브러리가 넘기는 aux: `view.go:476-485` prepare 2f+1 수집 후 `PreparesFrom{Ids: voterIDs}` 를 marshal 해 `v.Signer.SignProposal(*proposal, prpFromRaw)`. 서명은 `:496-498` Commit 메시지에 실리고, `decide` `:860` 에서 자기 서명을 덧붙여 `Decider.Decide(proposal, signatures, requests)`.
- **Fabric 은 aux 를 버린다** (위 (2) 참조): `signer.go:43` `_ []byte`; 서명 Msg 는 `BlockHeader‖IdentifierHeader‖OrdererBlockMetadata{LastConfig, ConsenterMetadata: proposal.Metadata}` (`signer.go:50-58`). `VerifyConsenterSig` 는 `return nil, v.ConsenterVerifier.Evaluate(...)` (`verifier.go:237`) 로 aux 로 nil 을 돌려준다.
- 블록 메타데이터로 들어가는 것: `chain.go:367-411` `Deliver()` 는 각 서명의 `Msg` 를 `Signature` 로 unmarshal 하여 `cb.MetadataSignature{Signature: s.Value, IdentifierHeader: sig.IdentifierHeader}` 만 만든다. `:394` `//	AuxiliaryInput: sig.AuxiliaryInput,` 로 **주석 처리**되어 있다. `ordererBlockMetadata` 는 첫 서명의 것을 그대로 쓴다(`:389-391`). 따라서 **블록에는 aux(PreparesFrom) 가 들어가지 않는다.** 들어가는 것은 `ConsenterMetadata = proposal.Metadata`(= `ViewMetadata` 직렬화: `black_list`, `prev_commit_signature_digest` 포함) 이다 — `assembler.go:60-69` 가 제안 시점에 `BlockMetadataIndex_SIGNATURES.Value.OrdererBlockMetadata.ConsenterMetadata` 로 넣고, `Deliver` 가 서명 쪽 `OrdererBlockMetadata` 로 덮어쓴다.
- 재시작 복구: `chain.go:266-269` `proposal, signatures := c.lastPersistedProposalAndSignatures()` → `consensus.LastProposal/LastSignatures` → `consensus.go:137` 체크포인트. `chain.go:252-253` 은 `BlackList`, `PrevCommitSignatureDigest` 를 마지막 블록의 `ConsenterMetadata` 에서 읽어 초기 `ViewMetadata` 로 넘긴다. 즉 **`prev_commit_signature_digest` 는 Fabric 블록에 영구 기록되고 재시작 후 체크포인트 메타데이터로 복원되지만, 회전 OFF 에서는 항상 빈 값**이다.
- 안전성 관점 메모: `ViewMetadata` 는 `proposal.Metadata` 로 서명 대상에 포함되므로(`signer.go:57`), 블랙리스트/digest 규칙을 바꾸는 어떤 선택지도 "모든 정직 노드가 같은 결정적 규칙으로 검증" 하기만 하면 합의 안전성(동일 seq 에 서로 다른 블록 커밋 불가)에는 영향이 없다. 블랙리스트는 리더 선택(라이브니스) 장치다.

## 선택지 비교

| 선택지 | 안전성 영향 | 라이브니스 영향 | 수정 범위 | 논문 방어 가능성 |
|---|---|---|---|---|
| **(a)** 윈도우 내에서는 `seq - W` 의 커밋 서명을 싣는다 (지연 증거) | 없음 — 검증 규칙이 결정적이면 유지. 단 `verifyBlacklist` 의 `prevMD` 가 W 단계 전 메타데이터가 되어 `computeUpdate` 의 뷰 변경 감지(`prevMD.ViewId != currView`)와 `offset` 계산(`util.go:454-456`) 의미가 달라짐 → 재검증 필요 | 블랙리스트 증거가 W 만큼 지연. 팔로워가 `seq-W` 를 아직 `Deliver` 하지 않은 상태에서 PrePrepare 를 받으면 검증 불가 → 버퍼링 없으면 Complain/ABORT(`view.go:390-395`) 유발 → **뷰 변경 폭증 위험** | **크다.** `Checkpoint` 를 깊이 W 링버퍼로(`types.go:71-105`), `CheckpointRetriever` 시그니처에 seq 인자 추가(`view.go:69`), `GetMetadata/Propose/verifyPrevCommitSignatures/verifyBlacklist/updateBlacklistMetadata` 전부 seq 인지형으로, 초기 W-1 개 seq 에 대한 특례, Fabric `lastPersistedProposalAndSignatures` 를 W 블록 복원으로(`chain.go:266`), WAL/sync 경로의 체크포인트 복원 모두 수정 | 가능하지만 "증거 지연 W" 가 블랙리스트 복권(`pruneBlacklist`)의 정확성 논증을 새로 요구. Fabric 에선 aux 가 nil 이어서 복권 자체가 동작하지 않으므로 실증 불가 |
| **(b)** 블랙리스트 갱신을 윈도우 경계(파이프라인 drain 시점)에서만 한다 | 없음 — 경계가 결정적(예: `seq % W == 0` 또는 메타데이터 플래그)이면 모든 정직 노드가 같은 규칙 적용. 윈도우 내부 proposal 은 "블랙리스트 불변 + 서명 0개 + digest nil" 을 검증(이미 `:671-679`, `:681-688` 의 skip 패턴과 동형) | 블랙리스트 갱신·복권이 W 결정마다 1회로 느려짐. 경계에서 리더가 drain(직전 윈도우 전부 커밋 대기)해야 하므로 처리량이 주기적으로 꺼짐. 회전 ON 이면 `checkIfRotate` 가 경계에서만 뷰를 재시작하도록 `DecisionsPerLeader` 를 W 의 배수로 묶어야 함 | **중간.** `verifyProposal` 에 "경계 여부" 분기, `Propose/GetMetadata` 에 경계에서만 `RetrieveCheckpoint` 서명 포함, `verifyBlacklist` 에 "비경계 → 블랙리스트 불변 검사" 분기. 체크포인트는 깊이 1 유지(drain 덕분에 경계 시점엔 체크포인트가 정확히 `seq-1`). `checkIfRotate` 는 decision 마다가 아닌 경계에서만 평가 | 좋음. 기존 코드의 "재구성/멤버십 변화 시 블랙리스트 동결" 과 같은 패턴을 확장한 것이라 설명이 쉽고, 블랙리스트 정확성 논증은 그대로(경계 사이의 증거만 합산 불가, 경계 직전 결정의 증거만 사용) |
| **(c)** 블랙리스트·로테이션 OFF 로 실험, 논문 한계로 명시 | 없음 — `DecisionsPerLeader == 0` 에서 체인 의존성은 송신·검증 양쪽 no-op (`:965-967`, `:992-995`, `:1039-1043`, `:655-662`, `:604`) | 리더 고정. 리더 장애 시 뷰 변경으로만 복구(기존과 동일). 블랙리스트 기반 리더 회피 없음 | **최소.** `Validate()` 에 `PipelineDepthMax > 1 && LeaderRotation → error` 한 줄 추가. 기존 검증 경로 무수정, W=1 회귀 테스트(`TestBlacklistAndRedemption` 등) 그대로 통과 | **강함.** 비교 baseline 인 Fabric+SmartBFT 가 이미 회전 OFF 를 강제(`configverifier.go:185-187`, `util/util.go:59-61`, `encoder.go:229`)하므로 "실험 설정이 운영 설정과 같다" 고 쓸 수 있음. 한계는 "라이브러리 단독 기본값(회전 ON)과 다르다" 는 점 하나 |

프로즈:

- (a) 는 가장 "일반적" 이지만 비용 대비 얻는 것이 없다. 깊이 W 체크포인트와 seq 인지형 검증은 WAL 복구·sync·Fabric 블록 복원까지 번진다. 더 중요한 것은 **라이브니스 역설**: 지연 증거를 검증하려면 팔로워가 `seq-W` 를 이미 전달했어야 하는데, 파이프라인 윈도우의 정의상 팔로워는 `seq-W` 의 커밋을 아직 못 받았을 수 있다. 그러면 `verifyProposal` 실패 → `Complain + Sync + ABORT` (`view.go:390-395`) 로 정상 상황에서 뷰 변경이 난다. 이를 막으려면 PrePrepare 버퍼링이 필요하고, 이는 `processPrePrepare` 의 current/next 2채널 구조(`:303-326`) 를 다시 설계하는 일이다. 더구나 Fabric 에서는 aux 가 항상 nil 이어서 (a) 가 보존하려는 "복권 증거" 는 어차피 동작하지 않는다.
- (b) 는 기존 코드의 어법("특정 조건에서 블랙리스트 동결 + 서명 검증 skip", `:671-679`, `:681-688`) 을 그대로 확장한다. 체크포인트 깊이 1 을 유지할 수 있는 유일한 회전-ON 선택지다. 그러나 drain 이 주기적으로 파이프라인을 비우므로 **W 의 이점을 일부 상쇄**하고, 이 자체가 실험 변수(drain 주기)를 하나 더 만든다. 1차 프로토타입에 넣으면 PipeBFT 의 효과와 drain 의 비용이 섞여 측정된다.
- (c) 는 수정이 거의 없고, **Fabric 포크가 이미 강제하는 설정과 일치**한다. 논문에서 "SmartBFT 의 Fabric 통합은 리더 회전을 비활성화한 상태로만 배포된다(configverifier.go:185)" 를 인용하면 한계가 아니라 **현실 반영**으로 서술할 수 있다. 라이브러리 단독 벤치(test 하네스)에서는 회전 ON 기본값(`config.go:109-110`)을 OFF 로 바꿔야 하는데, 이는 설정 한 줄이다.

## 결론 / 설계 decision

**채택: (c) 를 1차 프로토타입의 요구 조건으로 고정하고, (b) 를 "회전 ON 확장" 으로 설계 장에 기술한다.**

1. **설정 규칙**: `PipelineDepthMax > 1` 이면 `LeaderRotation == false`(⇔ `DecisionsPerLeader == 0`) 를 `Config.Validate()` (`pkg/types/config.go:176-180` 근처) 에서 강제. 에러 문자열 예: `"PipelineDepthMax > 1 requires LeaderRotation to be off"`. `PipelineDepthMax == 1` 은 baseline 과 동일하게 회전 ON/OFF 모두 허용.
2. **코드 영향**: 회전 OFF 에서 `Propose()` 는 서명을 싣지 않고(`:965-967`), `GetMetadata()` 는 빈 블랙리스트·nil digest 를 만들며(`:992-995`, `:1039-1043`), 팔로워의 `verifyPrevCommitSignatures` 는 빈 루프, `verifyBlacklist` 는 `:655-662` 에서 즉시 통과, digest 검사는 `:604` 가드로 비활성. 따라서 **체인 의존성 코드를 건드리지 않고** 파이프라인을 올릴 수 있다. 파이프라인 구현이 바꿔야 하는 것은 `verifyProposal` 의 검사 3(`md.LatestSequence != v.ProposalSequence`, `:577`) 과 `startNextSeq()` 의 단일 seq 상태, `processPrePrepare` 의 2채널 구조, 그리고 `controller.decide()`/`Checkpoint.Set` 의 순서 보장이다 — 이는 다른 섹션의 범위.
3. **근거**: (i) Fabric 소비자는 이미 회전 OFF 를 강제(`configverifier.go:185-187`) → 실험 설정 = 운영 설정. (ii) Fabric 은 aux 를 서명에 넣지 않으므로(`signer.go:43`, `verifier.go:89-95`) 블랙리스트 복권 메커니즘이 어차피 동작하지 않는다 → 보존할 기능이 없다. (iii) (a) 는 정상 동작에서 뷰 변경을 유발할 수 있는 라이브니스 역설을 안고 있어 1차 프로토타입에 부적합.
4. **논문 서술**: "PipeBFT 는 리더 회전이 비활성화된 SmartBFT 를 대상으로 한다. 이는 현행 Fabric 통합이 유일하게 허용하는 설정이다. 리더 회전 및 블랙리스트와의 결합은 윈도우 경계에서만 `prev_commit_signatures` 를 싣고 블랙리스트를 갱신하는 방식(선택지 (b))으로 확장 가능하며, 이는 SmartBFT 가 재구성·멤버십 변화 시 이미 사용하는 '블랙리스트 동결' 패턴의 일반화다."

## 열린 문제

- **(b) 의 경계 정의**: `seq % W == 0` 같은 산술 경계는 뷰 변경 후 `ProposalSequence` 가 임의 값에서 재시작할 때(`controller.changeView(view, seq, dec)`) 어긋난다. 메타데이터 플래그(예: `ViewMetadata.window_boundary bool`)로 하는 편이 결정적이지만 proto 필드 추가 = Fabric 블록 포맷 변경(`ConsenterMetadata`)이다. 결정 보류.
- **`Checkpoint.Set` 와 out-of-order 전달**: `controller.go:989-990` 은 `latest >= pending.LatestSequence` 면 "이미 커밋됨" 으로 보고 sync 로 우회한다. W>1 에서 decision 들이 순서대로 `Deliver` 되는 것을 파이프라인 구현이 보장해야 하며, 보장되지 않으면 체크포인트가 뒤로 가거나 블록 쓰기가 거부된다. 이 노트의 범위 밖(전달 순서 섹션)이나 체인 의존성과 맞닿아 있어 기록.
- **라이브러리 자체 테스트**: `test/test_app.go` 는 aux 를 온전히 싣고 기본 설정이 회전 ON 이므로, `PipelineDepthMax > 1` 테스트는 회전 OFF 로 설정을 바꿔 작성해야 한다. 기존 `TestBlacklistAndRedemption`(`test/basic_test.go:2027`), `TestBlacklistMultipleViewChanges`(`:2138`) 는 W=1 회귀 테스트로 유지.
- **[?] Byzantine 리더의 빈 서명 전송 (baseline 자체의 이슈, 파이프라인 무관)**: 회전 ON 이지만 `blacklistingSupported == false` 인 상태(Fabric 은 항상 이 상태)에서 리더가 `PrevCommitSignatures = nil`, `PrevCommitSignatureDigest = nil` 을 일관되게 보내면 `:635-649` 루프 생략, `:692-695` 생략, `:603-606` nil==nil 로 통과하는 것으로 읽힌다. 이때 `computeUpdate` 는 빈 `preparesFrom` 으로 돌아 복권만 막힌다(안전성 영향 없음). 코드 읽기만으로 확인했고 테스트로 재현하지는 않았다. 논문 범위 밖이지만 "Fabric 에서 블랙리스트 증거 메커니즘이 실질적으로 비활성" 이라는 주장을 뒷받침하는 관찰이므로 기록.
- **vendor 동기화**: fabric 포크의 `vendor/github.com/hyperledger/SmartBFT` 는 `v1.0.2` 이고 `replace` 지시자가 없다. 파이프라인 구현을 Fabric 에서 실험하려면 `replace github.com/hyperledger/SmartBFT => ../SmartBFT_Pipeline` 류의 지시자와 `go mod vendor` 가 필요하다. 이 노트 범위 밖이나 체인 의존성 코드가 vendor 에도 복제되어 있음을 명시.
