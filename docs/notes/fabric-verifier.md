# fabric-verifier

- 날짜: 2026-11-02
- 대상 커밋: fabric 98dc3b24 (main), SmartBFT 32a628d6 (baseline; 포크 main HEAD 는 그 위에 docs 전용 커밋 ea1d499d 한 개가 더 있음 — `.github/PULL_REQUEST_TEMPLATE.md` 만 추가)
- probe 브랜치: 없음 — 읽기만

> 전제 수정: 과제 설명은 "이 트리에는 verifier.go 가 없다" 고 했지만 실제로는 **있다**.
> `orderer/consensus/smartbft/verifier.go` (463줄) 에 `VerifyProposal`(:98), `VerifyRequest`(:156), `VerifyConsenterSig`(:209),
> `VerificationSequence`(:240), `verifyHashChainAndDataHash`(:244), `verifyBlockDataAndMetadata`(:263) 가 모두 구현돼 있다.
> `util/util.go` 에는 `ConfigFromMetadataOptions` (채널 설정 → `types.Configuration` 변환) 하나만 있다.

## 체크리스트

### [x] `verifier.go` `VerifyProposal`: `PreviousHash` 를 무엇과 비교하는지 (마지막 커밋 블록? 체인 헤드?) — 파이프라인에서는 "직전 제안" 과 비교해야 함

**답: 마지막 *커밋된* 블록의 헤더 해시(`RuntimeConfig.LastCommittedBlockHash`)와 비교한다. 원장을 직접 읽지 않고 `RuntimeConfig` 스냅샷을 읽는다.**

- `orderer/consensus/smartbft/verifier.go:98-120`
  ```go
  func (v *Verifier) VerifyProposal(proposal types.Proposal) ([]types.RequestInfo, error) {
      block, err := ProposalToBlock(proposal)
      ...
      rtc := v.RuntimeConfig.Load().(RuntimeConfig)
      if err := verifyHashChainAndDataHash(block, rtc.LastCommittedBlockHash); err != nil {
  ```
- `verifier.go:244-261` `verifyHashChainAndDataHash`:
  ```go
  thisHdrHashOfPrevHdr := hex.EncodeToString(block.GetHeader().GetPreviousHash())
  if prevHeaderHash != thisHdrHashOfPrevHdr {
      return errors.Errorf("previous header hash is %s but expected %s", thisHdrHashOfPrevHdr, prevHeaderHash)
  }
  dataHash, err := protoutil.BlockDataHash(block.GetData())   // DataHash 재계산 비교
  ```
  **블록 `Number` 연속성은 여기서 검사하지 않는다** (PreviousHash·DataHash 만). Number 는 원장 append 시점에 `common/ledger/blkstorage/blockfile_mgr.go:286-288` ("block number should have been %d but was %d") 에서만 걸린다 → `BlockWriter.commitBlock` 에서 `Panicf("Could not append block")` (blockwriter.go:199-201).
- `LastCommittedBlockHash` 갱신 지점: `util.go:65` (일반 블록), `util.go:91` (설정 블록) 모두 `hex.EncodeToString(protoutil.BlockHeaderHash(block.GetHeader()))`. `RuntimeConfig.BlockCommitted` 는 (a) `chain.go:565-573 updateRuntimeConfig` ← `Deliver` (chain.go:432), (b) 동기화 `OnCommit` (synchronizer_factory.go:78-81, 96-99), (c) 체인 생성 시 (chain.go:153-162) 에서만 호출된다.
- 라이브러리 쪽 호출 지점: `SmartBFT/internal/bft/view.go:558-560` `verifyProposal` → `v.Verifier.VerifyProposal(proposal)` (pre-prepare 처리 시).
- 추가로 `verifyBlockDataAndMetadata` (verifier.go:301-327) 가 **LastConfig 인덱스**도 커밋 상태에 묶는다:
  ```go
  lastConfig := rtc.LastConfigBlock.GetHeader().GetNumber()
  if protoutil.IsConfigBlock(block) { lastConfig = block.GetHeader().GetNumber() }
  ...
  if ordererMetadataFromSignature.GetLastConfig().GetIndex() != lastConfig {
      return nil, errors.Errorf("last config in block orderer metadata points to %d but our persisted last config is %d", ...)
  ```
  → 설정 제안 n 이 미확정인 상태에서 n+1 을 검증하면 n+1 의 LastConfig(=n) 와 `rtc.LastConfigBlock`(이전 설정 블록) 이 달라 실패한다. (설정 배리어 근거 #3, 아래 참조)

**파이프라인에서 필요한 변경**: proposal n+1 의 기대 PreviousHash 는 `sha256(proposal_n.Header)` 다. 근거: `assembler.go:78` `Header: protoutil.BlockHeaderBytes(block.GetHeader())`, `protoutil/blockutils.go` `BlockHeaderHash(b) = sha256.Sum256(BlockHeaderBytes(b))`. 즉 **직전 제안의 `Header` 바이트만 있으면 원장/RuntimeConfig 없이 검증 가능**하다. `Verifier` 가 "직전 제안 헤더" 를 받을 경로가 필요하다 (결론 절).

### [x] `VerificationSequence()` 가 반환하는 값과, 제안의 `VerificationSequence` 가 다를 때의 처리 → 설정 블록 배리어가 필요한 정확한 이유

**반환값**: `verifier.go:240-242` `return v.VerificationSequencer.Sequence()`; `VerificationSequencer` 는 `support` (chain.go:633) = `multichannel.ChainSupport`.
`orderer/common/multichannel/chainsupport.go:176-178` `return cs.ConfigtxValidator().Sequence()` → `common/configtx/validator.go:204-206` 현재 번들의 **config sequence**.
이 값은 **설정 블록이 원장에 쓰일 때만** 올라간다: `blockwriter.go:90-159 WriteConfigBlock` 이 `bw.support.Update(bundle)` (blockwriter.go:149) 를 호출 → 이건 `Deliver` (chain.go:426-427) 안에서 동기적으로 일어난다. 따라서 "Sequence() 가 s→s+1 이 되는 순간" = "설정 블록 n 의 Deliver 가 WriteConfigBlock 을 끝낸 순간".

**불일치 처리 (양쪽에서 거부)**:
- Fabric `verifier.go:114-117`:
  ```go
  verificationSeq := v.VerificationSequence()
  if verificationSeq != uint64(proposal.VerificationSequence) {
      return nil, errors.Errorf("expected verification sequence %d, but proposal has %d", verificationSeq, proposal.VerificationSequence)
  ```
- 라이브러리 `SmartBFT/internal/bft/view.go:587-591`: `expectedSeq := v.Verifier.VerificationSequence(); if uint64(proposal.VerificationSequence) != expectedSeq { ... errors.New("verification sequence mismatch") }`
- 라이브러리는 또 seq 변화 시 이전 커밋 서명 검증을 **건너뛴다** (view.go:618-622 "Skipping verifying prev commit signatures due to verification sequence advancing from %d to %d"), 블랙리스트 갱신을 건너뛴다 (view.go:945-953), 요청 풀을 다시 prune 한다 (controller.go:745-760 `MaybePruneRevokedRequests`: "Verification sequence changed: %d --> %d").
- 제안 측 스탬프: `assembler.go:81` `VerificationSequence: int64(a.VerificationSeq())` = 조립 시점의 `Sequence()`.
- 관례: 설정 블록을 제안으로 복원할 때 `chain.go:499-504` `VerificationSequence: int64(c.verifier.VerificationSequence())` 후 `if protoutil.IsConfigBlock(block) { prop.VerificationSequence-- }` → **설정 블록 n 의 제안은 설정 적용 *이전* 시퀀스 s 를 가진다**. 즉 "제안 n(설정) 은 s, 제안 n+1 부터 s+1" 이 불변식.

**설정 블록 배리어가 필요한 정확한 이유 (설정 제안 n 이 미확정인 채로 n+1 을 제안하면)**:
1. **VerificationSequence 불일치가 노드별로 갈린다.** 리더는 n 커밋 전에 n+1 을 조립하므로 `VerificationSequence = s`. n 을 먼저 커밋한 팔로워(정상 경로)는 `Sequence() = s+1` 이라 verifier.go:116 에서 n+1 을 거부하고, n 을 아직 커밋 못한 팔로워는 통과시킨다 → 같은 pre-prepare 에 대한 판정이 노드마다 달라져 뷰 체인지 유발. 리더가 n+1 에 s+1 을 미리 찍는 것도 불가(아직 아무 노드도 s+1 이 아님).
2. **n+1 안의 트랜잭션 유효성이 n 의 설정에 의존.** `verifyRequest` (verifier.go:160-206) 는 `AccessController.Evaluate` (ChannelWriters 정책, chain.go:657-669), `ConfigValidator.ValidateConfig` → `ProposeConfigUpdate` (configverifier.go:227, 현재 번들 기준) 를 **커밋된 번들**로 평가한다. Fabric 자체도 이걸 알아서 `chain.go:332-342 Order()` 에서 `configSeq < seq` 면 `ProcessNormalMsg` 로 재검증한다. n 커밋 후 라이브러리도 풀을 다시 prune 한다 (controller.go:745). 미확정 n 위의 n+1 은 "어느 설정으로 검증해야 하는지" 가 정의되지 않는다.
3. **LastConfig 인덱스 검사** (verifier.go:313-315, 위 인용) — n+1 의 메타데이터는 `LastConfig = n` 이어야 하지만 검증자의 `rtc.LastConfigBlock` 은 아직 이전 설정 블록. `Signer.LastConfigBlockNum` (chain.go:239-245) 도 같은 `rtc` 를 읽으므로 서명 메타데이터도 어긋난다.
4. **Reconfig 가 컴포넌트를 재시작한다.** `Deliver` 가 `Reconfig.InLatestDecision=true` 를 돌려주면 (chain.go:580-587: 노드 집합 또는 `BFTConfig` 가 바뀐 경우) 라이브러리 `pkg/consensus/consensus.go:76-82` 가 `reconfigChan` 으로 보내고 `reconfig()` (consensus.go:186-262) 가 `viewChanger.Stop(); controller.StopWithPoolPause(); collector.Stop()` 후 `c.Config = reconfig.CurrentConfig` 로 재생성·재시작한다. 멤버십이 바뀌면 n+1 의 정족수 자체가 바뀐다. 진행 중인 n+1.. 은 버려진다.

→ **규칙**: 설정 tx 를 담은 제안은 파이프라인 창의 마지막이어야 하고(그 뒤 제안 금지), 그 제안이 커밋되어 `Sequence()` 가 올라가고 Reconfig 처리가 끝난 뒤에만 다음 제안을 조립한다 (depth 를 일시적으로 1 로 축소 = "drain"). 조립 측에서는 `singleConfigTxOrSeveralNonConfigTx` (assembler.go:87-117) 가 이미 "설정 tx 는 단독 블록" 을 보장하므로, 배리어 판정은 "조립된 블록이 `protoutil.IsConfigBlock` 인가" 로 할 수 있다 (assembler.go:56 에서 이미 계산함).

### [x] `assembler.go` `AssembleProposal`: 블록 헤더의 `PreviousHash`·`Number` 를 어디서 가져오는지 (ledger 마지막 블록?) — 리더가 미확정 블록 위에 다음 블록을 조립하려면 수정 필요

**답: 원장이 아니라 `RuntimeConfig.LastBlock` (마지막 *커밋된* 블록 객체) 에서 가져온다.**

- `orderer/consensus/smartbft/assembler.go:41-52`
  ```go
  rtc := a.RuntimeConfig.Load().(RuntimeConfig)
  lastConfigBlockNum := rtc.LastConfigBlock.GetHeader().GetNumber()
  lastBlock := rtc.LastBlock
  ...
  block := protoutil.NewBlock(lastBlock.GetHeader().GetNumber()+1, protoutil.BlockHeaderHash(lastBlock.GetHeader()))
  block.Data = &cb.BlockData{Data: batchedRequests}
  block.Header.DataHash = protoutil.ComputeBlockDataHash(block.GetData())
  ```
- `LastConfig` 메타데이터(:60-70) 도 `rtc.LastConfigBlock` 기준 (자기 자신이 설정 블록이면 자기 번호, :56-58).
- `VerificationSequence` (:81) = `a.VerificationSeq()` = `c.verifier.VerificationSequence` (chain.go:227).
- `Assembler` 는 상태가 없다(`RuntimeConfig`, `Logger`, `VerificationSeq` 만; :34-38). 라이브러리 인터페이스 `api.Assembler.AssembleProposal(metadata []byte, requests [][]byte) bft.Proposal` (`SmartBFT/pkg/api/dependencies.go:33-37`) 에는 "직전 제안" 인자가 없다.
- `rtc.LastBlock` 은 `Deliver` 가 끝나야 바뀌므로 (chain.go:432 → util.go:69/95), 현재 코드로 W>1 에서 n 커밋 전에 다시 호출하면 **같은 Number·같은 PreviousHash 의 블록을 또 만든다**.

**수정 필요**: 리더가 n+1 을 조립할 때 `Number = n.Number+1`, `PreviousHash = sha256(proposal_n.Header)` 를 쓰도록, "마지막으로 *제안한* (미확정 포함) 헤더" 를 입력으로 받아야 한다. 뷰 체인지/동기화로 미확정 제안이 폐기되면 이 추정 헤드도 폐기되어야 하므로, Fabric 쪽에서 내부 상태로 추적하는 것보다 **라이브러리가 직전 제안을 인자로 넘기는 편이 안전**하다 (결론 절).

### [x] `chain.go` `Deliver`: 블록을 원장에 쓰는 순서 보장, `Reconfig` 반환 조건

- `orderer/consensus/smartbft/chain.go:367-437`. 흐름: `ProposalToBlock` → 서명들을 `cb.MetadataSignature` 로 변환 (:378-406; 서명 언마샬 실패 시 `c.Halt()` 후 빈 `Reconfig{}` 반환 :380-387) → `BlockMetadataIndex_SIGNATURES` 채움 (:408-411) → 설정 블록이면 `c.support.WriteConfigBlock(block, nil)`, 아니면 `c.support.WriteBlock(block, nil)` (:426-430) → `c.updateRuntimeConfig(block)` (:432) 의 반환값을 그대로 돌려줌.
- **순서 보장**:
  - 라이브러리는 결정 순서대로 `Application.Deliver` 를 단일 경로로 호출한다 (`pkg/consensus/consensus.go:76-82`, 컨트롤러 decide 경로).
  - `blockwriter.go:162-170 WriteBlock` 은 `bw.committingBlock.Lock(); bw.lastBlock = block; go func(){ defer Unlock(); bw.commitBlock(...) }()` — **비동기지만 뮤텍스로 직렬화**되어, 다음 `WriteBlock` 은 앞 블록 append 가 끝날 때까지 블록된다. 따라서 append 순서 = Deliver 호출 순서.
  - `WriteConfigBlock` (blockwriter.go:146-149) 은 `bw.committingBlock.Lock(); Unlock()` 으로 앞 goroutine 을 기다린 뒤 `bw.support.Update(bundle)` 하고 `WriteBlockSync` 로 동기 기록한다. 주석: "prevent bw.support.Sequence() from advancing before the go-routine reads it".
  - 부작용: 일반 블록 Deliver 가 반환된 직후 `support.Height()` 는 아직 이전 값일 수 있다 (append 가 goroutine). 그래서 verifier/assembler 가 `Height()` 대신 `rtc.LastBlock` 을 쓰는 구조가 맞다. 파이프라인에서도 Deliver 는 여전히 순차이므로 이 부분은 그대로 둬도 된다.
- **`Reconfig` 반환 조건**: `chain.go:565-588 updateRuntimeConfig`
  ```go
  newRTC, err := prevRTC.BlockCommitted(block, c.bccsp)
  if err != nil { ...; c.Halt(); return types.Reconfig{} }
  c.RuntimeConfig.Store(newRTC)
  if protoutil.IsConfigBlock(block) {
      c.Comm.Configure(c.Channel, newRTC.RemoteNodes)
      c.ClusterService.ConfigureNodeCerts(c.Channel, newRTC.consenters)
      c.pruneBadRequests()
  }
  membershipDidNotChange := reflect.DeepEqual(newRTC.Nodes, prevRTC.Nodes)
  configDidNotChange := reflect.DeepEqual(newRTC.BFTConfig, prevRTC.BFTConfig)
  noChangeDetected := membershipDidNotChange && configDidNotChange
  return types.Reconfig{ InLatestDecision: !noChangeDetected, CurrentNodes: newRTC.Nodes, CurrentConfig: newRTC.BFTConfig }
  ```
  즉 **모든 설정 블록이 Reconfig 가 아니다**: 컨센터 집합(`Nodes`) 이나 `types.Configuration` (`BFTConfig`) 이 실제로 바뀐 설정 블록만 `InLatestDecision=true`. 그 외 설정 변경(정책·MSP 등)은 `false` 지만 `Sequence()` 는 여전히 올라가므로 배리어 이유 1~3 은 그대로 적용된다 → 배리어 판정은 "Reconfig 인가" 가 아니라 "설정 블록인가" 로 해야 한다.
- `PipelineDepthMax` 를 `types.Configuration` 에 넣으면 `DeepEqual` 에 자동으로 포함되어, 플래그만 바꾼 설정 업데이트도 `InLatestDecision=true` → 라이브러리 `reconfig()` 가 새 `Config` 로 컴포넌트를 재생성한다.

### [x] `synchronizer.go`: 여러 블록을 한 번에 따라잡는 경로가 있는지

**있다. 두 구현 모두 `[Height, targetHeight-1]` 범위를 루프로 받아 블록마다 원장에 쓰고 `OnCommit` 을 호출하며, 마지막 블록만 `Decision` 으로 돌려준다.**

- 선택: `synchronizer_factory.go:69-113` `localConfigCluster.ReplicationPolicy` 가 `"consensus"` → `BFTSynchronizer`, `"simple"` → `Synchronizer`. 둘 다 `OnCommit = pruneCommittedRequests + updateRuntimeConfig` (:78-81, :96-99).
- `synchronizer.go:82-151` (`simple`): `blockPuller.HeightsByEndpoints()` → `computeTargetHeight` (:157-171, 내림차순 정렬 후 `heights[f]`, f=(n-1)/3) → 루프 :119-138
  ```go
  for seq <= targetSeq {
      block := blockPuller.PullBlock(seq)
      if protoutil.IsConfigBlock(block) { s.Support.WriteConfigBlock(block, nil) } else { s.Support.WriteBlockSync(block, nil) }
      lastPulledBlock = block
      prevInLatestDecision := s.lastReconfig.InLatestDecision
      s.lastReconfig = s.OnCommit(lastPulledBlock)
      s.lastReconfig.InLatestDecision = s.lastReconfig.InLatestDecision || prevInLatestDecision
  ```
  반환 `s.BlockToDecision(lastPulledBlock)` (:150).
- `synchronizer_bft.go:88-124, 241-281` (`consensus`): `BFTDeliverer` 가 goroutine 으로 `SyncBuffer` (sync_buffer.go) 에 블록을 넣고, `getBlocksFromSyncBuffer` 가 같은 루프 (:248-269) 로 `WriteConfigBlock`/`WriteBlockSync` + `OnCommit`. `Reconfig` 누적 OR 동일 (:263-265).
- 실패 시 (`Sync` :40-55 / :47-62) 원장 마지막 블록을 `BlockToDecision` 으로 돌려주며 `InReplicatedDecisions: false`.
- 동기화 중 블록 기록은 **`WriteBlockSync`** (동기) 라서 루프 종료 시 `Height()` 와 `rtc.LastBlock` 이 일치한다.
- `BlockToDecision` (chain.go:509-551) 는 블록 하나를 제안+서명으로 복원; 라이브러리는 이 마지막 결정에서 `seq+1` 로 재시작한다 (`consensus.go` `startComponents(view, seq, dec, verificationSeq, ...)`).
- **파이프라인 함의**: 동기화는 이미 "여러 블록" 을 다루므로 그대로 쓸 수 있다. 단, Sync 후 라이브러리가 들고 있던 미확정 제안(추정 헤드) 은 전부 무효 — Assembler/Verifier 의 "직전 제안" 기준점이 `Sync` 반환 결정으로 리셋되어야 한다. `createBFTDeliverer` (synchronizer_bft.go:183) 가 `s.Support.Block(startHeight-1)` 로 원장을 읽는데, Deliver 의 비동기 append 와 겹치면 1 블록 뒤처질 수 있다 (기존 동작, 파이프라인과 무관).

### [x] 채널 설정(`configtx`, `ConsensusConfig`)에 플래그를 추가하는 방법과 설정 변경 시 orderer 재시작 필요 여부

**옵션이 저장되는 곳**: Orderer 그룹의 `ConsensusType.Metadata` 에 `orderer.smartbft.Options` proto 를 마샬해 넣는다.
- proto 는 fabric 트리에 **없고** `github.com/hyperledger/fabric-protos-go-apiv2 v0.3.7` (go.mod:21) 에 생성 코드로만 들어온다: `vendor/github.com/hyperledger/fabric-protos-go-apiv2/orderer/smartbft/configuration.pb.go:83-100`. 필드 1~18 (`request_batch_max_count` … `leader_rotation`=17, `decisions_per_leader`=18). 소스 `.proto` 는 별도 저장소 `hyperledger/fabric-protos` 의 `orderer/smartbft/configuration.proto`. `scripts/compile_protos.sh` 는 `vendor` 를 제외한 트리 내 `.proto` 만 컴파일하므로 **API proto 재생성은 fabric 저장소 안에서 할 수 없다**.
- 생성(configtxgen) 경로: `sampleconfig/configtx.yaml:622` `Orderer.SmartBFT:` → `internal/configtxgen/genesisconfig/config.go:174` `SmartBFT *smartbft.Options \`yaml:"SmartBFT"\``; 기본값 :216-233 (`types.DefaultConfig` 에서 복사), 미지정 시 :439-442 에서 기본값 대입 → `internal/configtxgen/encoder/encoder.go:225` `channelconfig.MarshalBFTOptions(conf.SmartBFT)` (`common/channelconfig/util.go:340`, proto.Clone 후 Marshal, 검증 없음) → :236 `channelconfig.ConsensusTypeValue(conf.OrdererType, consensusMetadata)`. :229 에서 `LeaderRotation` 을 강제로 `ROTATION_OFF`.
- 읽기(orderer) 경로: `channelconfig.Orderer.ConsensusMetadata()` (`common/channelconfig/orderer.go:163`) → `createSmartBftConfig` (`orderer/consensus/smartbft/util.go:449-458`, `proto.Unmarshal` 후 `RequestBatchMaxCount/Bytes` 를 `BatchSize` 로 덮어씀) → `util.ConfigFromMetadataOptions(selfID, options)` (`util/util.go:17-78`) → `types.Configuration`, `config.Validate()` (:67). 호출자:
  1. `consenter.go:180,191 HandleChain` — 체인 생성 시 초기 `types.Configuration` (`BFTChain.Config`, `smartbft.Consensus.Config`).
  2. `util.go:100-125 configBlockToBFTConfig` ← `configBlockCommitted` (:74-98) ← `BlockCommitted` — **커밋되는 모든 설정 블록마다** 재계산 → `RuntimeConfig.BFTConfig`.
  3. `orderer/common/msgprocessor/maintenancefilter.go:137-146` — Raft→BFT 마이그레이션 시 유효성 검사.
  4. `configverifier.go:180-188 checkConsentersMatchPolicy` — 설정 tx 검증 시 `Options` 언마샬, `ROTATION_ON` 이면 "leader rotation must be turned off for this version or be unspecified" 로 거부.
- 설정 변경 허용 범위: `maintenancefilter.go:105-118` 은 `ConsensusType.State` 가 바뀌는 tx 에서만 `Metadata` 변경을 금지하고 (:114 "attempted to change ConsensusType.Metadata, but ConsensusType.State is changing"), Type 변경은 유지보수 모드에서만 허용 (:120-135). **정상 모드에서 Type 유지·Metadata 만 바꾸는 업데이트는 허용**된다.

**플래그 추가 절차 (제안)**:
1. `fabric-protos` 포크: `orderer/smartbft/configuration.proto` 의 `Options` 에 `uint64 pipeline_depth_max = 19;` 추가 → `fabric-protos-go-apiv2` 포크 재생성 → fabric `go.mod` 에 `replace github.com/hyperledger/fabric-protos-go-apiv2 => <포크>` + `go mod vendor`. (`Options` 에 예약/확장 필드가 없어 proto 변경 없이 넣을 깨끗한 방법은 없다. 생성 코드의 `rawDesc` 수정은 비현실적.)
2. 라이브러리: `types.Configuration.PipelineDepthMax uint64`, `DefaultConfig` 에 1, `Validate()` 에 범위 검사 (`pkg/types/config.go:92,115`).
3. `util/util.go ConfigFromMetadataOptions` 에 `config.PipelineDepthMax = options.GetPipelineDepthMax()`; **0 이면 1 로** (proto3 에서 필드 누락 = 0 → 기존 채널은 자동으로 원본 동작). `RequestMaxBytes` 의 0 처리 패턴 (:71-75) 과 동일.
4. `internal/configtxgen/genesisconfig/config.go:216` 기본값에 `PipelineDepthMax: types.DefaultConfig.PipelineDepthMax`; `sampleconfig/configtx.yaml:622` 에 예시.
5. (선택) `configverifier.go:checkConsentersMatchPolicy` 에서 `PipelineDepthMax` 상한 검사.

**재시작 필요 여부: 불필요 (프로세스·체인 재시작 없음).**
경로: 설정 tx → `chain.go:350 Configure` → `ValidateConfig` → 합의 → `Deliver` → `WriteConfigBlock` 이 번들 교체 (blockwriter.go:149) → `updateRuntimeConfig` → `configBlockCommitted` → 새 `BFTConfig` → `DeepEqual` 불일치로 `Reconfig.InLatestDecision=true` (chain.go:581-584) → 라이브러리 `consensus.go:78-81` `reconfigChan` → `reconfig()` (consensus.go:186-262) 가 `c.Config = reconfig.CurrentConfig` 후 `createComponents()`/`startComponents()` 로 **컨트롤러만 재생성**. `multichannel.Registrar` 는 설정 블록으로 체인을 재시작하지 않는다 (`registrar.go:437`, `:887-894` 의 `Halt()` 는 채널 join/remove 경로). 따라서 라이브러리가 `createComponents` 에서 `c.Config.PipelineDepthMax` 를 읽기만 하면 다음 Reconfig 부터 적용된다. 단 `Reconfig` 시 진행 중 제안은 폐기되므로, 플래그 변경 설정 블록 자체에도 배리어가 적용된다(위 규칙과 일치).

### [x] Fabric 이 쓰는 SmartBFT 버전 (`go.mod`) — 포크 커밋과 일치시키기

- `fabric/go.mod:17` `github.com/hyperledger/SmartBFT v1.0.2`; `vendor/modules.txt:200-209` (`## explicit; go 1.26.3`, 패키지 `internal/bft, pkg/api, pkg/consensus, pkg/metrics, pkg/metrics/disabled, pkg/types, pkg/wal, smartbftprotos`). `go.mod` 에 `replace` 는 없음. fabric `go 1.27.3` (go.mod:3), 포크 `go 1.27.3` (SmartBFT/go.mod:3) — 일치.
- 포크에서 `v1.0.2` 태그 = `a6719ef9` (2026-09-28). `git log --oneline v1.0.2..HEAD` = **35 커밋** (32a628d6 "bft: enqueue a decision when its view is aborted (#742)" 가 2026-11-01), `git log HEAD..v1.0.2` = **0** → **vendored 사본은 포크의 순수 조상(더 오래됨)**.
- 파일 비교 (`diff -q`, vendor vs 포크):
  - 동일: `pkg/types/config.go`, `pkg/api/dependencies.go` (`api.Verifier`/`api.Assembler` 인터페이스 변동 없음).
  - 다름: `internal/bft/{view,controller,state,batcher,heartbeatmonitor,requestpool,statecollector,support,util,viewchanger}.go`, `pkg/consensus/consensus.go`, `pkg/api/metrics.go`, `pkg/wal/{reader,util,writeaheadlog}.go`, `smartbftprotos/{logrecord,messages}.pb.go`.
  - vendor 에만 있음: `internal/bft/sched.go` (#723 "Remove the unused Scheduler from internal/bft" 로 삭제됨). 단 `pkg/consensus.Consensus.Scheduler`, `ViewChangerTicker` 필드는 포크에도 남아 있어 `chain.go:262-263` 은 그대로 컴파일된다.
  - 의미 있는 내부 변화 예: `view.go` `State.Save(message, truncateTo bool)` (WAL truncation 플래그 명시, #740), `consensus.go` `startComponents(view, seq, dec, verificationSeq, configSync)` 및 `reconfig()` 내 풀 prune (#729), `controller.Start(..., verificationSequence, ...)`.
  - fabric 이 쓰는 공개 심볼은 포크에 모두 존재 확인: `wal.InitializeAndReadAll` (pkg/wal/writeaheadlog.go:794), `wal.DefaultOptions` (:110), `wal.NewMetrics`/`(*Metrics).With` (pkg/wal/metrics.go:23,30), `api.NewMetrics`/`With` (pkg/api/metrics.go:81,91). `pkg/consensus/consensus.go` 의 v1.0.2..HEAD diff 에 exported 필드 변경 없음.
- **replace 계획**: `fabric/go.mod` 에
  ```
  replace github.com/hyperledger/SmartBFT => ../SmartBFT
  ```
  (또는 `=> github.com/<org>/SmartBFT <pseudo-version>`) 추가 후 `go mod tidy && go mod vendor` (fabric 은 `vendor/` 가 있어 `-mod=vendor` 로 빌드되므로 `vendor/modules.txt` 갱신 필수). 파이프라인 작업은 두 저장소를 함께 바꾸므로 Fabric 포크의 replace 대상은 `SmartBFT_Pipeline` 쪽 feature 브랜치를 가리키게 된다.

## 결론 / 설계 결정

**바꿔야 하는 Fabric 함수 (모두 `orderer/consensus/smartbft/`)**

| 함수 | 현재 의존 | 변경 |
|---|---|---|
| `Verifier.VerifyProposal` (verifier.go:98) → `verifyHashChainAndDataHash` (:244) | `rtc.LastCommittedBlockHash` | depth>1 이면 기대 PreviousHash = `sha256(prevProposal.Header)` (직전 제안, 커밋 여부 무관). depth==1 또는 직전 제안이 마지막 커밋 블록이면 기존 경로와 결과가 같아야 함 |
| `Verifier.verifyBlockDataAndMetadata` (:263, LastConfig 검사 :301-315) | `rtc.LastConfigBlock` | 설정 배리어가 있으면 **변경 불필요** (미확정 설정 블록 위에 제안이 올 수 없으므로). 배리어를 assert 로 명시 |
| `Verifier.VerifyProposal` seq 검사 (:114-117) | `support.Sequence()` | 변경 불필요 (배리어로 보장). 배리어 위반 시 이 에러가 나오는 것이 정상 |
| `Assembler.AssembleProposal` (assembler.go:41) | `rtc.LastBlock` (Number, PreviousHash) | depth>1 이면 Number/PreviousHash 를 직전 제안 헤더에서 계산: `Number = prev.Number+1`, `PreviousHash = sha256(prev.Header)`. `LastConfig`·`VerificationSequence` 는 기존대로(배리어 전제) |
| `Assembler` 조립 결과가 설정 블록(`IsConfigBlock`, assembler.go:56)일 때 | — | 라이브러리에 "이 제안 뒤로는 커밋 전까지 제안 금지" 신호 필요 (배리어). 신호 전달 방식은 라이브러리 측 결정; Fabric 은 `types.Proposal` 에 플래그를 넣거나 `IsConfigBlock(ProposalToBlock(p))` 를 라이브러리가 호출할 수 있게 헬퍼 노출 |
| `BFTChain.Deliver` (chain.go:367) | — | 변경 불필요. 순차 호출·뮤텍스 직렬화로 순서 보장됨. `Reconfig` 로직 그대로 |
| `util.ConfigFromMetadataOptions` (util/util.go:17) | `smartbft.Options` | `PipelineDepthMax` 매핑 (0→1) |
| `Synchronizer`/`BFTSynchronizer` | — | 변경 불필요. Sync 후 추정 헤드 리셋은 라이브러리 책임 |

**"직전 제안" 을 Verifier/Assembler 에 전달하는 방식 (권장)**: Fabric 쪽이 추정 헤드를 내부 상태로 추적하면 뷰 체인지·Sync·제안 폐기 때마다 리셋 훅이 필요해 두 저장소 사이 상태 동기화가 생긴다. 대신 라이브러리가 자기 상태(진행 중 제안 큐)에서 직전 제안을 꺼내 인자로 넘기면 Fabric 은 **무상태**로 남는다. `api.Verifier`/`api.Assembler` 인터페이스에 추가 메서드(예: `VerifyProposalAfter(prop, prevHeader []byte)`, `AssembleProposalAfter(metadata, requests, prevHeader []byte)`) 를 두고, 기존 메서드는 depth==1 경로로 유지하면 라이브러리 기존 테스트도 깨지지 않는다. 필요한 정보는 `prev.Header` 바이트 하나면 충분하다 (sha256 으로 PreviousHash, asn1 디코드(`ProposalToBlock`) 로 Number).

**설정 블록 배리어 규칙**: "설정 tx 를 담은 제안 n 은 창의 마지막; n 이 `Deliver` 되어 `Sequence()` 가 올라가고 `Reconfig` 처리가 끝난 뒤에만 n+1 조립." 판정 기준은 `IsConfigBlock` (Reconfig 여부 아님 — Reconfig 가 아닌 설정 블록도 `Sequence()` 와 LastConfig 를 바꾼다).

**플래그 배관 경로**: `configtx.yaml Orderer.SmartBFT.PipelineDepthMax` → `genesisconfig.Orderer.SmartBFT` → `encoder.go MarshalBFTOptions` → `ConsensusType.Metadata` → (orderer) `createSmartBftConfig` → `ConfigFromMetadataOptions` → `types.Configuration.PipelineDepthMax` → `HandleChain` (초기) / `configBlockCommitted` (변경 시) → `Reconfig.CurrentConfig` → 라이브러리 `reconfig()` 가 `c.Config` 로 적용. **orderer 재시작 불필요**, proto 변경은 `fabric-protos`/`fabric-protos-go-apiv2` 포크 + replace 필요.

**버전 정렬**: vendored v1.0.2 는 포크보다 35 커밋 오래됨(조상). `replace github.com/hyperledger/SmartBFT => ../SmartBFT` + `go mod vendor`. 공개 API 는 호환 (인터페이스 파일 동일, fabric 이 쓰는 심볼 모두 존재).

## 열린 문제

- [?] **포크로 replace 한 fabric 빌드 검증 안 함** — 읽기 전용 제약으로 `go.mod` 수정·`go build` 을 하지 않았다. 심볼 존재는 grep 으로만 확인. 특히 WAL 포맷 변경 (#739 anchor-only tail 재사용, #694 레코드 헤더 길이 검증, #740 truncation 플래그) 이 기존 WAL 디렉터리와 호환되는지는 실험으로 확인해야 한다.
- [?] **`VerifyProposal` 의 다른 호출 지점** — `view.go:560` 만 확인했다. 뷰 체인지 시 진행 중 제안 검증 경로(`viewchanger.go`) 가 `VerifyProposal` 을 다시 부르는지, 그때 "직전 제안" 이 무엇이어야 하는지는 라이브러리 측 노트(viewchange 관련) 에서 확정 필요. 파이프라인에서 뷰 체인지 시 여러 in-flight 제안이 있으면 각 제안의 PreviousHash 기준점이 다르다.
- [?] **`VerifyConsenterSig` / `prevCommits`** — `view.go:600-644 verifyPrevCommitSignatures` 는 `RetrieveCheckpoint()` (마지막 *커밋* 제안) 의 서명을 검증한다. depth>1 이면 pre-prepare n+1 에 실린 prev-commit 서명은 n 의 것이지만 체크포인트는 n-1 이하일 수 있다 — 블랙리스트/prev-commit 의존성 노트(`prev-commit-dependency.md`) 와 교차 확인 필요. Fabric 쪽 `VerifyConsenterSig` (verifier.go:209) 자체는 `prop.Header` 만 비교하므로 어떤 제안을 넘기든 동작한다.
- [?] **설정 배리어 신호의 전달 방식** — `types.Proposal` 에 필드를 추가(=smartbftprotos `Proposal` 메시지 변경, WAL 레코드 호환성 영향) 할지, 라이브러리가 Fabric 헬퍼를 콜백으로 호출할지 미정. Fabric 쪽에서는 `IsConfigBlock(ProposalToBlock(p))` 가 이미 가능하므로 콜백형이 proto 변경을 피한다.
- [?] `BlockToDecision` 의 `VerificationSequence--` 관례 (chain.go:502-504) 가 파이프라인에서도 그대로 성립하는지 — 배리어가 있으면 성립하지만, Sync 가 설정 블록 바로 뒤 블록으로 끝날 때 라이브러리가 `startComponents(..., verificationSeq, ...)` 에 넘기는 값과 어긋나지 않는지 재검토.
- Deliver 의 비동기 `WriteBlock` 때문에 `support.Height()` 가 1 뒤처질 수 있는 기존 창(`createBFTDeliverer` synchronizer_bft.go:183) — 파이프라인과 무관한 기존 동작이지만 벤치마크 중 Sync 가 잦아지면 관찰될 수 있음.
