# G. 상태 저장 (`internal/bft/state.go`, `pkg/wal`)
- 날짜: 2026-11-02
- 대상 커밋: SmartBFT 32a628d6 (baseline)
- probe 브랜치: 없음 — 읽기만

(참고: 로컬 `main` HEAD 는 ea1d499d 이지만 32a628d6 위에 docs 커밋 하나만 올라가 있고 소스 경로 diff 가 비어 있어 아래 라인 번호는 baseline 과 동일함.)

## 체크리스트

### [x] `State.Save/Restore` 가 저장하는 메시지 종류 (`SavedMessage` proto, smartbftprotos/messages.proto 113행)
- `smartbftprotos/messages.proto:113-120`:
  ```proto
  message SavedMessage {
      oneof content {
          ProposedRecord proposed_record = 1;
          Message commit = 2;
          ViewMetadata new_view = 3;
          ViewChange view_change = 4;
      }
  }
  ```
  `ProposedRecord` (`messages.proto:43-46`) = `PrePrepare pre_prepare` + `Prepare prepare`. PrePrepare 는 `Proposal`(header/payload/metadata/verification_sequence) 전체를 포함하므로 **배치 payload 가 WAL 에 그대로 들어간다** (`messages.proto:29-34, 98-103`).
- `State` 인터페이스 (`internal/bft/view.go:50-60`): `Save(message *protos.SavedMessage, truncateTo bool) error`, `Restore(*View) error`. 주석: "A pre-prepare is normally a truncation point, while a commit never is: its pre-prepare must survive next to it."
- Save 호출 지점 4 종 (각각 truncateTo 값):
  | 레코드 | 지점 | truncateTo |
  |---|---|---|
  | `proposed_record` (PP+내 Prepare) | `view.go:408-416` processProposal, prepare 브로드캐스트 직전 | **true** |
  | `commit` (내 Commit 메시지, 서명 포함) | `view.go:504-513` processPrepares, commit 브로드캐스트 직전 | false |
  | `new_view` (ViewMetadata) | `controller.go:666-675` sync 후, `viewchanger.go:1207-1214` | false |
  | `view_change` | `viewchanger.go:440-446` | false |
- `PersistedState.Save` (`state.go:41-57`): WAL 에 쓰기 전에 **메모리 측 `InFlightProposal` 도 갱신** 한다 — `GetProposedRecord()` → `storeProposal` → `InFlightData.StoreProposal()`, `GetCommit()` → `storePrepared` → `StorePrepares(view, seq)`. 그 다음 `proto.Marshal` → `ps.WAL.Append(b, truncateTo)`. **Save 1회 = WAL 레코드 1개 = fsync 1회** (아래 G3). 호출자는 실패 시 panic (`view.go:417, 514`).
- Restore 는 `ProposalMaker.NewProposer` 에서 `sync.Once` 로 프로세스 생애 한 번만 호출된다 (`util.go:318-323 restoreOnceFromWAL.Do`).

### [x] 재시작 복구 시 in-flight 제안을 하나만 복원한다고 가정하는지
- **예, 전면적으로 하나를 가정한다.**
  1. `Restore` (`state.go:113-151`) 는 `entries[len(entries)-1]` 만 본다. 마지막이 `proposed_record` 면 `recoverProposed` → `v.Phase = PROPOSED`, `v.ProposalSequence = prp.GetSeq()` (`state.go:171-173`); 마지막이 `commit` 이면 `recoverPrepared` 가 **`entries[len(entries)-2]` 가 짝이 되는 pre-prepare 라고 가정** 한다:
     ```go
     // state.go:184-186
     if len(entries) < 2 {
         return errors.New("last message is a commit, but expected to also have a matching pre-prepare")
     }
     ```
     그리고 `state.go:196-198` 에서 그 레코드가 `ProposedRecord` 가 아니면 에러. W>1 에서 `[PP(n), PP(n+1), C(n), C(n+1)]` 처럼 섞이면 이 가정이 깨진다.
  2. `View` 는 단일 필드 `inFlightProposal *types.Proposal`, `inFlightRequests` (`view.go:99-100`), `lastBroadcastSent` 하나.
  3. `InFlightData` (`util.go:184-192`) 는 포인터 하나 `v *inFlightProposalData{proposal, prepared}`; `StoreProposal` 은 **덮어쓴다** (`util.go:238 ifp.v = &inFlightProposalData{proposal: &p}`). 두 번째 PP 를 Save 하면 첫 번째 in-flight 정보가 사라진다.
  4. 뷰 체인지 프로토콜도 하나만 운반: `ViewData.in_flight_proposal = 4; in_flight_prepared = 5` (`messages.proto:65-71`), `ValidateInFlight` 는 `in-flight seq == lastSequence+1` 을 요구 (`viewchanger.go:774-776`), `maybePruneInFlight` (`controller.go:695-718`) 도 단일.
  5. `recoverPrepared` 는 앱 체크포인트와 비교해 이미 커밋된 제안은 버린다: `v.ProposalSequence > prePrepareFromWAL.GetSeq()` → "safely committed" (`state.go:207-210`); `<` 이면 에러 (200-204). 여기서 `v.ProposalSequence` 는 앱 메타데이터의 `LatestSequence+1` (`consensus.go:525-528 c.controller.Start(view, seq+1, ...)`). 윈도우 복구도 같은 기준(체크포인트 seq 초과분만 in-flight) 을 쓰면 된다.
- **WAL truncation 이 "하나" 가정을 강화한다**: PP 의 `truncateTo=true` 는 "이 레코드 이전 전부 삭제 가능" (`pkg/wal/writeaheadlog.go:405-408`, `ReadAll` 은 `rec.GetTruncateTo()` 에서 `items = items[0:0]` `writeaheadlog.go:562-565`). W>1 에서 PP(n+1) 을 truncateTo=true 로 쓰면 **아직 미결정인 PP(n)/C(n) 이 복구 집합에서 사라진다.** 윈도우 구현은 truncateTo 규칙을 반드시 바꿔야 한다.

### [x] WAL 에 W 개 제안을 쓸 때 크기·fsync 비용 (read pkg/wal: when is fsync called, is there batching, file size/rotation policy)
- **fsync 시점**: 레코드마다. `append()` (`writeaheadlog.go:426-520`):
  ```go
  nh, err := w.logFile.Write(w.headerBuff)   // 466  8B 헤더
  np, err := w.logFile.Write(payloadBuff)    // 471  proto(LogRecord) + 8B 패딩
  err = w.logFile.Sync()                     // 476  fsync — 매 Append
  ```
  `Append()` 는 `w.mutex` 를 잡고 `append()` 를 호출 (`writeaheadlog.go:420-423`) → **모든 Append 가 직렬화**, 그룹 커밋/배칭 없음. `BufferSizeBytes`(기본 1 MiB, 38행) 는 **마샬링 버퍼의 초기 cap 일 뿐** (`append()` 435행 `buf := make([]byte, 0, w.bufferSizeBytes)`), 쓰기 버퍼링이 아니다. 파일 전환 시 `saveCRC()` 도 추가 Sync (`writeaheadlog.go:784`), `truncateAndCloseLogFile` 도 Sync+truncate (`620-647`).
- **레코드 형식/크기**: 8B 헤더(하위 32b 길이, 상위 32b CRC32C 체인) + `LogRecord{type, truncate_to, data}` proto (`smartbftprotos/logrecord.proto:13-24`) + 8B 정렬 패딩 (`util.go:144-146 getPadSize`). 오버헤드는 수십 B; 크기는 `data` = `SavedMessage` 가 지배한다. `proposed_record` 는 배치 payload 전체 (최대 `RequestBatchMaxBytes`=10 MiB 기본), `commit` 레코드는 digest+서명이라 작다. **seq 당 WAL 쓰기 2 회(PP, C) 는 W 와 무관**; W 가 바꾸는 것은 (a) 동일 노드에서 PP(n+1) Save 와 C(n) Save 가 WAL mutex 에서 직렬화되고 뷰 goroutine 이 `Save` 를 동기 호출하므로 fsync 지연이 라운드 임계 경로에 W 번 겹친다는 점, (b) 아래 보존 크기.
- **파일 크기/회전**: `FileSizeBytesDefault = 64 MiB` (38행). 레코드를 쓴 **뒤** `offset > FileSizeBytes-16` 이면 `switchFiles()` (`500-507`) → 파일은 레코드 하나만큼 64 MiB 를 초과할 수 있다 (10 MiB 배치면 최대 ~74 MiB). `switchFiles` → `truncateAndCloseLogFile` → `deleteAndCreateFile` (`677-748`): `activeIndexes[i] < truncateIndex` 인 파일만 삭제 (`680-694`). `truncateIndex` 는 truncateTo 레코드를 쓴 파일 인덱스 (`488-490`). 즉 **삭제는 파일 단위·지연 실행**(파일 전환 때만) 이고, "마지막 truncation point 가 들어 있는 파일" 이전의 파일만 지운다.
- **W 개 보존 시 함의**: truncation point 를 "모든 이전 seq 가 결정됐을 때" 로만 둘 수 있으므로, 지속 부하에서 윈도우가 비는 일이 없으면 truncation point 가 전진하지 않고 WAL 이 파일 단위로 계속 쌓인다 (`wal_count_of_files` 증가). 복구 시 `ReadAll` 은 마지막 truncation point 이후 전부를 메모리에 올린다 (`writeaheadlog.go:536-570`). 예상 보존량 ≈ (마지막 truncation 이후 결정된 seq 수 + W) × (PP 크기 + C 크기).
- WAL 옵션은 라이브러리 외부(호출 앱) 가 `wal.InitializeAndReadAll(logger, dir, options)` (`writeaheadlog.go:794-858`) 로 만들고 `Consensus.WAL`/`WALInitialContent` 로 주입한다 (`pkg/consensus/consensus.go:32-33, 131-133`). `api.WriteAheadLog` 인터페이스는 `Append(entry, truncateTo)` 만 노출 (`pkg/api/dependencies.go:40-44`) — `TruncateTo()` 제어 레코드 (`writeaheadlog.go:390-401`) 는 인터페이스에 없다.
- WAL 지연 메트릭은 없다 (`pkg/wal/metrics.go` 는 `wal_count_of_files` 만). `view_latency_batch_save` 는 WAL 이 아니라 `Application.Deliver` 시간이다 (`controller.go:1002-1004`).

## 결론 / 설계 결정

### 윈도우 상태 저장 형식 초안
원칙: `PipelineDepthMax=1` 에서 디스크 바이트열이 원본과 **동일** 해야 한다 (truncateTo 규칙이 W=1 로 환원되면 원본과 같아짐).

1. **레코드 종류는 그대로** (`proposed_record`, `commit` 재사용; seq 는 각각 `pre_prepare.seq`, `commit.seq` 에 이미 있다). 신규 oneof 하나만 추가:
   ```proto
   message SavedMessage {
       oneof content {
           ProposedRecord proposed_record = 1;
           Message commit = 2;
           ViewMetadata new_view = 3;
           ViewChange view_change = 4;
           WindowMark window_mark = 5;   // 신규 (W>1 에서만 기록)
       }
   }
   message WindowMark {
       uint64 view = 1;
       uint64 decided_seq = 2;   // 이 seq 이하 전부 결정·전달됨
   }
   ```
2. **truncateTo 규칙**: `proposed_record(seq)` 의 truncateTo = "Save 시점에 seq 보다 작은 미결정 seq 가 없음" (윈도우 저수위 == seq). W=1 이면 항상 true → 원본과 동일. W>1 에서 윈도우가 비어 있지 않으면 false. `commit` 은 항상 false (원본 유지).
3. **compaction**: 윈도우가 지속적으로 차 있어도 WAL 이 무한히 자라지 않도록, decide(n) 시점에 `WindowMark{decided_seq=n}` 을 truncateTo=**true** 로 쓰고 **직후 아직 in-flight 인 seq>n 의 PP/C 레코드를 재-Append** (truncateTo=false) 한다. 비용은 결정마다 최대 (W-1) 개 제안 재기록 + fsync (W-1)+1 회 → 쓰기 증폭 ≈ W. 대안: K 결정마다 또는 윈도우가 빌 때만 WindowMark 를 쓰는 지연 compaction (보존량 ↑, 증폭 ↓). **어느 쪽을 택할지는 벤치마크로 결정 — 열린 문제 1.**
4. **Restore 알고리즘** (`state.go:113` 대체):
   - `entries` (마지막 truncation point 이후) 를 순서대로 fold: `map[seq]{pp *ProposedRecord, commit *Message}`; `WindowMark` 가 나오면 `decided = max(decided, mark.decided_seq)`; `new_view`/`view_change` 가 **마지막** 레코드이면 원본과 같이 처리.
   - in-flight 집합 = `seq > max(decided, checkpointSeq)` 인 항목 (checkpointSeq 는 현재 `recoverPrepared` 가 쓰는 `v.ProposalSequence-1` 기준, `state.go:200-210`).
   - 각 항목 phase = commit 있으면 PREPARED, 없으면 PROPOSED; 재브로드캐스트 메시지 = commit ?? prepare (원본 `lastBroadcastSent` 복원 `state.go:166-170, 227` 을 seq 별로).
   - `InFlightData` 를 `map[seq]*inFlightProposalData` (정렬 가능) 로 확장하고 `StoreProposal` 이 덮어쓰지 않도록 변경 (`util.go:232-240`). `InFlightProposal()` 단일 getter 는 "최저 seq" 를 반환하도록 유지해 뷰 체인지 쪽 기존 호출 (`viewchanger.go:469`, `controller.go:696`) 을 깨지 않는다.
   - W=1 이면 fold 결과가 항목 1개 → 원본 `recoverProposed/recoverPrepared` 와 같은 상태.
5. **WAL 지연 계측**: `PersistedState.Save` 에서 `Append` 전후 시간을 측정해 `wal_append_latency` 히스토그램 + 제어기용 atomic 값으로 노출 (F 노트 신호 표 참조).
6. **인터페이스 변경 없음**: `api.WriteAheadLog.Append(entry, truncateTo)` 만으로 위 규칙 구현 가능 (`TruncateTo()` 제어 레코드 불필요).

## 열린 문제
1. **compaction 정책**: 결정마다 재기록(쓰기 증폭 W) vs 지연 compaction(보존량 증가, 복구 시 `ReadAll` 메모리 증가). 10 MiB 배치 × W=4 에서 fsync 횟수가 라운드 지연에 미치는 영향은 측정 전 ([?] — 수치 미확정).
2. **Save 의 동기성과 WAL mutex 직렬화**: 뷰가 W 개 제안을 동시에 진행하면 `Save` 호출들이 `w.mutex` 에서 줄을 선다. 그룹 커밋(여러 레코드를 한 Sync 로) 을 WAL 에 추가하려면 `pkg/wal` 변경이 필요한데, WAL 은 호출 앱(Fabric) 이 생성·주입하므로 라이브러리 인터페이스 밖이다. 1차는 WAL 무변경, 측정 후 판단.
3. **뷰 체인지 프로토콜의 단일 in-flight 가정** (`ViewData.in_flight_proposal`, `ValidateInFlight`, `CheckInFlight` `viewchanger.go:851`) 은 이 섹션 범위 밖이지만 WAL 복구 집합(여러 in-flight) 과 반드시 정합해야 한다 → 뷰 체인지 담당 노트와 교차 확인 필요.
4. **`new_view`/`view_change` 레코드 위치**: 현재 `LoadNewViewIfApplicable/LoadViewChangeIfApplicable` (`state.go:75-111`, 호출 `consensus.go:482,497`) 는 "마지막 레코드" 만 본다. compaction 재기록이 이들 뒤에 PP/C 를 다시 쓰면 "마지막" 이 바뀌어 뷰 체인지 복구가 무시될 수 있다 → 재기록은 뷰 체인지 레코드 뒤에는 하지 않거나, Load* 도 fold 방식으로 바꿔야 한다. 미해결.
5. **파일 크기 초과**: `offset > FileSizeBytes-16` 검사가 쓰기 뒤라 (`writeaheadlog.go:502`) 레코드 하나만큼 초과 가능 — 원본 동작이며 바꾸지 않지만, W×10 MiB 재기록 묶음이 한 파일에 몰리면 파일이 64 MiB + 40 MiB 가 될 수 있음. 문제인지 여부 미판단.
6. **`WindowMark` 의 하위 호환**: W=1 로 되돌린 노드가 W>1 시절 WAL(`window_mark` 포함) 을 읽을 때 원본 `Restore` 는 `unrecognized record` 에러 (`state.go:150`) 를 낸다. 포크에서는 항상 새 Restore 를 쓰므로 문제 없지만, 업스트림 바이너리와 WAL 디렉터리 공유는 불가하다고 명시해야 한다.
