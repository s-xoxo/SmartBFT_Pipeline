# 00-summary — 원본 소스 확인 종합 결론

- 날짜: 2026-11-02
- 대상 커밋: SmartBFT `32a628d6` (baseline), fabric `98dc3b24`
- 근거 노트: A [leader-token-flow](leader-token-flow.md) · B [view-slots](view-slots.md) · C [prev-commit-dependency](prev-commit-dependency.md) · D [viewchange-trace](viewchange-trace.md) · E [leader-rotation](leader-rotation.md) · F [batcher](batcher.md) · G [state-wal](state-wal.md) · H [test-harness](test-harness.md) · I [fabric-verifier](fabric-verifier.md)

한 줄 요약: **원본 SmartBFT 는 "뷰당 in-flight 제안 1개" 를 코드 전 계층(컨트롤러 토큰 → 뷰 2슬롯 → 요청 풀 → WAL 복원 → 뷰 체인지 → Fabric 검증기)에 구조적으로 박아 두었다.** 다행히 가장 우려했던 직전 커밋 서명 체인은 리더 로테이션이 꺼져 있으면 통째로 비활성화되며, Fabric 포크는 이미 로테이션을 강제로 끄고 있다. 따라서 1차 프로토타입은 `LeaderRotation=false` 를 전제로 하고, 설계 난점은 뷰 체인지 복구 규칙과 WAL 복원에 집중된다.

---

## 1. 파이프라이닝을 막는 의존성 (우선순위순)

| 순위 | 의존성 | 위치 | 현재 동작 | 왜 막히는가 | 노트 |
|---|---|---|---|---|---|
| 1 | **뷰의 시퀀스 수용 범위 [S-1, S+1]** | `view.go:220-233`, `nextPrePrepare` cap 1 (`:138`) | S, S+1 만 투표 누적, S+2 이상은 폐기 | 리더가 W 개를 연달아 보내면 **정확히 W-2 개가 영구 손실**(재전송 없음, `view.go:426`). 실측으로 확인 | B |
| 2 | **리더 토큰 cap 1 + run 루프 내 블로킹 propose** | `controller.go:802, 463-475, 496` | 토큰 1개, `NextBatch()` 가 run 고루틴을 막음 | W≥2 면 `decisionChan`(cap 1) 이 막혀 뷰 고루틴이 `Decide` 에서 정지 → proposer 고루틴 분리 필수 | A |
| 3 | **요청 풀 `NextRequests` 가 비파괴적** | `requestpool.go:295-330`, 제거는 `controller.go:935-940` | 항상 `fifo.Front()` 부터 읽고, 전달 후에만 제거 | seq n 결정 전에 다음 배치를 뽑으면 **같은 요청이 그대로 재제안**됨 → 풀에 "예약" 상태(in-flight 커서) 필요 | A, F |
| 4 | **결정 순서가 구조적으로만 보장** | `controller.go:887-920, 989-1000`, `view.go:856-862` | 단일 고루틴·단일 Phase 로 FIFO | 윈도우에서는 slot k 가 `low` 이고 committed 일 때만 `Decide` 하는 **명시적 in-order 게이트** 필요. out-of-order 는 `MutuallyExclusiveDeliver` 가 조용히 건너뜀 → 안전성 요건 | A, B |
| 5 | **뷰 체인지의 단일 in-flight** | `ViewChanger.InFlight` (`util.go:184-192`), `ViewData.in_flight_proposal`, `checkLastDecision :616`, `commitInFlightProposal :1277 Panicf` | in-flight 1개 저장·전파·커밋 | 새 리더가 **자기보다 2 이상 앞선 노드의 ViewData 를 거부**(`> mySequence+1`) → W 중첩 상태에서 리더가 따라잡을 수 없음. 가장 큰 설계 미해결 | D |
| 6 | **WAL 복원이 마지막 1~2 레코드만 읽음 + PP 저장 시 truncate** | `state.go:184-198`, `view.go:416 Save(truncateTo=true)` | PP 하나 + 그 Commit 만 복원 | PP 저장 시 절단이 **아직 미결정인 이전 seq 를 복구 집합에서 지움** → 재시작 시 in-flight 손실 | G |
| 7 | **Fabric 검증·조립이 "마지막 커밋 블록" 기준** | `verifier.go:98,247`, `assembler.go:45,52` | `PreviousHash` ↔ `LastCommittedBlockHash`, 블록 번호 ↔ `rtc.LastBlock` | 리더가 미확정 블록 n 위에 n+1 을 조립 못 함(두 번 호출 = 중복 블록); 팔로워는 직전 **제안** 헤더와 비교해야 함 | I |
| 8 | **설정 트랜잭션** | `verifier.go:114-117`, `view.go:587-591`, `blockwriter.go:149` | `VerificationSequence` 불일치 시 양쪽에서 거부 | 설정 블록이 in-flight 와 섞이면 노드마다 seq 판정이 갈림 → **설정 블록 전후 파이프라인 flush(배리어)** 필요. 배리어 키는 `IsConfigBlock`, `Reconfig` 가 아님 | I |
| 9 | **직전 커밋 서명 체인** | `view.go:965-995, 611-652`, 기준은 `RetrieveCheckpoint()`(깊이 1) | 로테이션 ON 이면 PP 에 직전 커밋 서명 포함·검증 | W>1 이면 팔로워 체크포인트가 N-1/N 으로 갈려 서명 검증이 **비결정적으로 실패 → Complain+Sync+ABORT** (라이브니스 위험). **단, `DecisionsPerLeader==0` 이면 생성·검증 모두 생략 (완전 우회)** | C |
| 10 | **리더 로테이션** | `controller.go:559-584` | 결정 3개마다 로컬·결정적 뷰 재시작 | 뷰 체인지 프로토콜 없이 일어나므로 in-flight 는 복구되지 않고 **버려짐**; 늦게 도착한 결정이 "뷰 교체됨" 경로로 빠져 노드 간 `decisionsInView` 가 갈림 | E |
| 11 | **배치 파라미터 1회 고정** | `consensus.go:459` → `batcher.go:29-31` | 설정에서 한 번 복사, 락 없이 읽음 | 런타임 변경 경로가 reconfig(컴포넌트 전체 재생성)뿐 → `SetBatchParams` 에 동기화 필요 | F |
| 12 | **라운드 지연 메트릭의 단일 `beginPrePrepare`** | `view.go:348, 399` | PP→commit 지연을 필드 하나로 측정 | W>1 이면 덮어써져 측정값이 깨짐 → 메트릭을 seq 별로 먼저 바꿔야 비교 실험이 성립 | F, H |

부수 발견:
- `Pool.Size()` 는 O(1) 이지만 배타 락 (`requestpool.go:285-290`); `sizeBytes`·요청 체류 시간은 추적되나 getter 없음. 메트릭 인터페이스는 write-only 라 적응형 제어기는 in-process 신호를 직접 읽어야 한다. (F)
- 하드 제약: `W × RequestBatchMaxCount ≤ RequestPoolSize` (기본 100×4 = 400, 딱 맞음). 초과 시 `Submit` 이 세마포어에서 블록. (F)
- WAL 은 Append 마다 fsync, 그룹 커밋 없음 (`writeaheadlog.go:476`). W 가 커지면 fsync 지연이 크리티컬 패스에 겹쳐 쌓임 → 로컬 지연 바닥. 파이프라이닝 이득을 보려면 ≥10 ms 링크 지연 주입이 필요. (G, H)
- Fabric 은 `AuxiliaryData` 를 전부 버리고 (`verifier.go:89-95`, `chain.go:394` 주석 처리) 로테이션을 `configverifier.go:185-187` 에서 거부함. 블랙리스트 구제 경로는 Fabric 에서 이미 죽은 코드. (C)
- Fabric 이 vendoring 한 SmartBFT v1.0.2 (`a6719ef9`) 는 포크보다 35 커밋 뒤. 포크가 쓰는 심볼은 모두 존재하지만 `pkg/wal` 등이 달라 WAL 포맷 호환 확인 필요. (I)
- 테스트 하네스의 `Restart()` 는 `DecisionsPerLeader/LeaderRotation` 만 이어받아 **새 플래그가 재시작 후 0 으로 초기화**됨 (`test_app.go:481-484`); reconfig 변환기도 동일. 테스트는 로그 문자열 15개에 훅을 걸어 동기화하므로 해당 로그 라인을 지우면 테스트가 영원히 대기한다. (H)

## 2. 해소 방안과 채택 근거

| 의존성 | 해소 방안 | 근거 |
|---|---|---|
| 1, 4 (뷰) | `View` 를 `low/head/slots map[uint64]*seqState` 로 재구성 (B 노트의 Go 스케치, 불변식 I1~I11). 라우팅: `s<low-W` 폐기, `[low-W,low)` prev 재전송, `[low,low+W)` 슬롯(lazy 생성), `≥low+W` sync 판정. `Decide` 는 `slot==low && committed` 일 때만. **리더는 미커밋 slot 전체의 pre-prepare 를 `doPhase` 주기로 재브로드캐스트**(pre-prepare 영구 손실 대책). **팔로워는 slot k 의 Prepare 를 `low..k-1` 의 pre-prepare 를 수신·해시 검증한 뒤에만 전송**(prepared(k-1) 은 요구하지 않음, viewchange-trace D2). `ViewSequences.Store` 는 **low watermark** 를 발행 → 하트비트·statecollector·`controller.go:664` 가 수정 없이 동작 | 투표가 PP 보다 먼저 도착하는 것이 정상(`view.go:248`)이므로 lazy 생성 필수. low watermark 선택으로 세 독자를 건드리지 않음 |
| 2 (토큰) | `leaderToken` 을 cap W 세마포어로, `proposerLoop` 고루틴 분리, 회수 규칙 R1~R7 (A 노트). 로테이션 분기를 탔으면 `decide:567` 의 재발급 생략((c) 채택으로 W>1 에서는 실제로 타지 않는 경로지만 Validate 규칙 변경에 대비해 고침). 모든 drain 지점(4곳)을 "빌 때까지" 루프로 | 불변식 `len(token)+inflight+restored == W` 를 유지하면 누수·중복 발급을 디버그 모드에서 패닉으로 잡을 수 있음 |
| 3 (풀) | 풀에 예약 상태 도입: 제안 시 예약, 전달 시 제거, 뷰 중단 시 예약 해제. `NextRequests` 는 예약된 요청을 건너뜀 | 배처가 요청을 꺼내지 않는 현 설계를 유지하면서 최소 변경 |
| 5 (뷰 체인지) | proto 는 기존 필드 4/5 유지(W=1 바이트 동일) + `repeated InFlightEntry in_flight = 6` 추가. 복구 규칙: `s=base+1..` 순회하며 per-seq 로 A1(noArgument≥quorum)/A2(preprepared≥f+1)/B(noInFlight≥quorum) 적용, A 면 접두사에 추가, B 면 중단. `commitInFlightProposal` 을 접두사 길이만큼 반복(`p.seq==checkpoint+1` 불변). **안전성은 전달(Deliver) 기준으로 정의**하고, 접두사 이후 seq 의 보유 상태(투표·commit 인증서·in-order 게이트 서명)는 모두 파기(규칙 D1). 뒤처진 리더는 1차안으로 sync 후 뷰 체인지 재시작 | 서명은 `raw_view_data` 바이트 위에 걸리고 리더가 그대로 전달하므로 스키마 추가는 서명을 깨지 않음. per-seq A/B 가 독립이고 커밋이 seq 순이므로 B 가 성립한 s 이후는 정직 노드가 커밋한 적 없음 → 접미사 폐기 안전. **미해결: 리더가 2 이상 뒤처진 경우** (ViewData 가 last_decision 하나만 운반) |
| 6 (WAL) | `SavedMessage` oneof 에 `WindowMark{view, decided_seq}` 추가. PP 의 `truncateTo` = "내 아래 미결정 seq 없음"(W=1 이면 원본과 바이트 동일). `Restore` 는 마지막 절단 이후 전체를 `map[seq]` 로 접고 `seq > max(decided, checkpoint)` 를 in-flight 로. 기존 `api.WriteAheadLog.Append` 만으로 구현 가능 | 디스크 포맷 호환 유지, W=1 동작 불변. 압축 정책(WindowMark 후 재기록 vs 지연)은 벤치로 결정 |
| 7 (Fabric 조립·검증) | 라이브러리가 직전 **제안 헤더**를 넘기는 `AssembleProposalAfter(prevHeader)` / `VerifyProposalAfter(prevHeader)` 류 메서드를 `pkg/api` 에 추가(기존 메서드는 W=1 용으로 유지). Fabric 은 무상태 유지. 기대 `PreviousHash = sha256(prev.Header)` | `Proposal.Header = BlockHeaderBytes`, `BlockHeaderHash = sha256(BlockHeaderBytes)` 이므로 직전 제안 헤더 바이트만 있으면 충분. 블록 번호 연속성은 검증기가 아니라 원장 append 에서만 검사됨 |
| 8 (설정 배리어) | 리더: 설정 트랜잭션을 포함하는 배치는 파이프라인이 비었을 때만 제안하고, 제안 후 결정까지 추가 제안 금지. 팔로워: `IsConfigBlock` 인 제안이 in-flight 면 그 위의 PP 거부. 신호 전달 방식(Proposal 필드 vs Fabric 헬퍼)은 미결 | `VerificationSequence` 는 `Deliver` 안의 `WriteConfigBlock → Update(bundle)` 에서만 증가하므로 설정 블록 결정 전후에 파이프라인을 비우면 노드 간 seq 판정이 일치 |
| 9, 10 (서명 체인·로테이션) | **선택지 (c) 채택**: `Config.Validate()` 에서 `PipelineDepthMax>1 ⇒ LeaderRotation==false` 강제. 서명 체인 코드는 건드리지 않음. (b) "서명·블랙리스트 갱신을 윈도우 경계에서만" 은 로테이션 ON 확장으로 문서화만. (a) `seq-W` 서명 탑재는 기각 | Fabric 기준선이 이미 로테이션 OFF 이므로 **비교 실험의 공정성이 유지**되고 수정 범위가 0. (a) 는 깊이 W 체크포인트를 WAL/sync/Fabric 복원 전체에 요구하고, 팔로워가 아직 전달하지 않은 seq-W 증거를 검증 못 하는 라이브니스 역설이 있음 |
| 11 (배처) | `SetBatchParams(BatchParams{MaxMsgCount, MaxSizeBytes, Timeout})` 를 `BatchBuilder` 와 `Batcher` 인터페이스에 추가, `atomic.Pointer` 로 교체. 제어기 입력 신호: 풀 길이·바이트, 최고령 요청 나이, 배치 충만 사유(full/timeout), PP→commit 지연, WAL append 지연, in-flight 수 | 타임아웃이 `NextBatch()` 호출 시점에 시작(문서와 다름)하므로 제어기가 이 대기를 백오프로 활용 가능 |
| 12 (메트릭) | `beginPrePrepare` 를 seq 별 map 으로; WAL append 지연·in-flight 수·풀 바이트 메트릭 추가; `view_latency_batch_save` 가 실제로는 `Deliver` 시간임을 문서화 | 계측만이므로 main 직접 커밋 가능. 비교 실험 전에 반드시 선행 |

## 3. `feat/` 브랜치 순서 확정

원안(정책 3절)에서 바뀐 점: ① 설정·하네스 배관을 별도 소형 브랜치로 분리, ② 컨트롤러·뷰·풀 변경은 플래그 ON 테스트가 서로를 필요로 하므로 **하나로 합침**, ③ WAL 윈도우 복원을 신규 브랜치로 추가(재시작 테스트의 선행 조건), ④ `prev-commit-dependency` 는 설정 검증 규칙으로 축소되어 ②에 흡수.

| 순서 | 브랜치 | 범위 | 플래그 | 선행 | 플래그 ON 테스트 (H 노트) |
|---|---|---|---|---|---|
| 1 | `feat/metrics-round-latency` | seq 별 `beginPrePrepare`, WAL append 지연, in-flight 수, 풀 바이트, 배치 충만 사유 메트릭. `view_latency_batch_save` 이름 정정은 문서로 | 없음 (main 직접 커밋 가능) | — | 기존 테스트 통과 |
| 2 | `feat/pipeline-config` | `PipelineDepthMax`(기본 1, 0→1 정규화)·`AdaptiveBatch`(기본 false) 필드, `Validate()` 에 `PipelineDepthMax>1 ⇒ !LeaderRotation`, `W×MaxCount ≤ PoolSize` 검사. 테스트 하네스 `Restart()` 이어받기·reconfig 변환기 반영, **하트비트·뷰체인지 타임아웃을 수 초로 주입하는 노드 생성자** | 둘 다 (기본값 원본) | 1 | `TestConfigValidatePipelineRequiresRotationOff`, `TestRestartKeepsPipelineConfig` |
| 3 | `feat/adaptive-batch` | `SetBatchParams` + 제어기(배치 크기·타임아웃만; W 조절은 5 이후) | `AdaptiveBatch` | 2 | `TestAdaptiveBatchGrowsUnderLoad` |
| 4 | `feat/pipeline-window` (구 3+4 통합) | 컨트롤러: 토큰 세마포어·`proposerLoop`·회수 규칙 R1~R7·in-order Decide 게이트. 뷰: `slots map[seq]`·라우팅·low watermark·prev 재전송 링. 풀: 예약 상태. `InFlightData` 다중화. **리더의 미커밋 slot pre-prepare 재브로드캐스트, 팔로워 prepare 선행 조건(D2), `decide:567` 중복 발급 수정** | `PipelineDepthMax` | 2 | `TestMultiInFlightBasic` (W=4, 손실 0 확인), `TestPipelineDecideInOrder` |
| 5 | `feat/state-wal-window` (신규, 구 G) | `WindowMark` 레코드, 절단 규칙, `Restore` map 접기 | `PipelineDepthMax` | 4 | `TestRestartKeepsMultipleInFlightProposals` ← `TestRestartKeepsViewAndInFlightProposal` |
| 6 | `feat/viewchange-multi-inflight` | `repeated in_flight`, 접두사 복구 규칙, `commitInFlightProposal` 반복, `checkLastDecision` 의 ≥2 뒤처짐 처리(설계 결정 후) | `PipelineDepthMax` | 5 | `TestMultiInFlightLeaderCrashThenRecovers` ← T3, `TestMultiInFlightThenViewChange` ← T4, `TestLaggingLeaderSyncsThenViewChanges`(ViewChangeTimeout 단축, sync→재시작 경로 로그 확인, 연쇄 뷰체인지 없음) |
| 7 | `feat/config-barrier` | 설정 트랜잭션 전후 파이프라인 flush, 팔로워 측 거부 규칙 | `PipelineDepthMax` | 4 | `TestReconfigFlushesPipeline` ← `TestBasicReconfig` |
| 8 | `feat/api-prev-proposal` | `pkg/api` 에 `*After(prevHeader)` 메서드 추가 (Fabric 파급) | `PipelineDepthMax` | 4 | 하네스 `App` 구현 추가 |
| 9 | (fabric) `feat/smartbft-pipeline-verifier` | `verifier.go`/`assembler.go` 의 `*After` 구현, 배리어 신호, `Options` proto 필드(fabric-protos 포크 + replace), `go.mod` replace → 포크 | Fabric 채널 설정 | 8 | Fabric 통합 smoke |

`prev-commit-dependency` 는 독립 브랜치를 두지 않는다 (2 의 `Validate()` 규칙 + C 노트가 설계 장 근거). 로테이션 ON 확장(선택지 b, 경계 규칙 B1~B5)은 논문 범위 밖 future work.

벤치: 라이브러리에 벤치마크가 없으므로 `bench/lib/` 를 별도 Go 모듈로 작성. **`test/` 하네스를 재사용하지 않고 `api.Comm` 을 직접 구현해 링크별 지연 큐를 둔다** — 송신 고루틴을 막지 않고 RTT 를 모사할 수 있어 Fabric+netem 전에 W·RTT 스윕의 1차 결과를 뽑을 수 있다. 로드 생성기·PP→Deliver 타이밍·CSV 포함. **비교군(baseline)도 `LeaderRotation=false` 로 돌린다**(Fabric 기준선과 동일 조건). 4 번 병합 직후 착수. PR 게이트 비용: `go test ./internal/... ./pkg/...` 약 18 s, `go test ./test` 약 2분 20초 (하트비트·뷰체인지 타임아웃 1분씩이 지배).

## 4. 1차 프로토타입에서 포기할 것과 논문 한계 문장

| 포기 항목 | 이유 | 한계 절 문장 (초안) |
|---|---|---|
| 리더 로테이션 ON 상태의 파이프라이닝 (확정) | 서명 체인이 깊이 1 체크포인트에 묶여 있고, 로테이션이 뷰 체인지 없이 in-flight 를 버림. **Fabric 기준선 근거: `orderer/consensus/smartbft/configverifier.go:185-187` 이 `ROTATION_ON` 을 거부, `util/util.go:59-61` 이 `DecisionsPerLeader=0` 강제, `configtxgen encoder.go:229` 가 `ROTATION_OFF` 생성.** 서론에서 범위를 "SmartBFT 라이브러리 일반이 아니라 Fabric 통합 설정에서의 파이프라이닝" 으로 명시하면 이 근거가 그대로 방어 논리가 됨 | "제안 기법은 리더 로테이션이 비활성화된 설정을 전제로 한다. 이는 Hyperledger Fabric 3.x 의 SmartBFT 통합이 현재 로테이션을 허용하지 않는 것과 일치하므로 비교 실험의 공정성에는 영향이 없다. 로테이션과 파이프라이닝의 결합은 윈도우 경계에서만 서명 체인과 블랙리스트를 갱신하는 확장으로 남겨 둔다." |
| 블랙리스트 기반 리더 제외 (확정) | 로테이션 OFF 에서 비활성; Fabric 은 보조 데이터를 버려(`verifier.go:89-95`, `chain.go:394`) 구제 경로가 이미 죽어 있음 | "블랙리스트에 의한 비정상 리더 제외는 평가 범위에서 제외한다." |
| 뷰 체인지에서 새 리더가 2 이상 뒤처진 경우의 최적 복구 | 설계 미결 (D 노트 열린 문제 1) | 1차: 그런 리더는 sync 로 따라잡은 뒤 뷰 체인지를 재시작 — 라이브니스는 유지, 지연은 증가. "뒤처진 리더의 뷰 체인지 비용은 측정하되 최적화하지 않는다." |
| 적응형 제어기의 W 동적 조절 | 배치 크기·타임아웃 조절을 먼저 검증 | "파이프라인 깊이는 실험별 고정값으로 두고, 적응형 제어는 배치 파라미터에 한정한다." (일정이 허락하면 해제) |
| WAL 그룹 커밋 | 범위 외 최적화 | "WAL 동기 쓰기 비용은 두 기법에 동일하게 적용되는 바닥 지연이다." |
| 라이브러리 하네스에서의 서명 검증·링크 지연 | 하네스가 서명을 검증하지 않고 지연 훅이 없음 | 지연 실험은 Fabric 네트워크 + netem 으로 수행; 하네스 벤치는 기능·순서 검증 위주 |

## 5. 열린 문제 (설계 결정 필요, 우선순위순)

1. **뷰 체인지: 새 리더가 2 이상 뒤처진 경우** — 1차안은 sync 후 뷰 체인지 재시작(PBFT 표준 처리). 연쇄 뷰 체인지 여부를 feat 6 테스트에서 로그로 확인. ViewData 에 last_decision 여러 개를 싣는 방식은 2차 후보. (D)
2. **설정 배리어 신호** — `Proposal` 에 필드 추가(WAL 호환 영향) vs Fabric 측 헬퍼. (I)
3. **WAL 압축 정책** — WindowMark 후 재기록(쓰기 증폭 ~W) vs 지연 압축. 벤치로 결정. (G)
4. **접두사 커밋의 타임아웃 예산** — `checkIfTimeout` 이 `startViewChangeTime` 기준이라 m 개 순차 커밋이 타임아웃을 유발할 수 있음. (D)
5. **`Controller.Decide` 의 블로킹** — 전달 완료까지 대기하는 현 정책을 윈도우에서 유지할지. (A)
6. **fabric-protos 포크** — `Options` proto 에 필드를 넣으려면 fabric-protos / fabric-protos-go-apiv2 포크와 replace 가 필요. 저장소 구성(정책 0절)에 추가 여부. (I)
