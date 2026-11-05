# bench

벤치마크 스크립트. 클라이언트 부하 생성, 메트릭 수집, TPS·지연시간 계산.

- 비교군은 항상 SmartBFT 포크의 `baseline` 태그로 빌드
- 제안 기법은 `main` (또는 `exp-*` 태그) 빌드에 플래그만 바꿔서 실행
  - `PipelineDepthMax=1`, `AdaptiveBatch=false` → 원본 동작
  - `PipelineDepthMax=W`, `AdaptiveBatch=true` → 제안 기법
- 결과는 `results/<yyyymmdd>-<topic>/` 에 저장

## 하네스 (Go, `bench/`)

단일 프로세스 안에서 N개의 `consensus.Consensus` 인스턴스를 띄우고, 링크별 지연 큐를 가진
인메모리 네트워크로 연결해 측정하는 벤치마크 하네스. 포크는 `go.mod`의
`replace github.com/hyperledger/SmartBFT => ../../SmartBFT` 로 참조한다 (vendoring 없음).

```
bench/
  lib/comm.go     Network/Comm: (from,to) 링크마다 goroutine + 채널, 편도 지연(RTT/2) 후 FIFO 전달
  lib/node.go     Node/App: 인메모리 WAL, 서명 = 노드 id 바이트, 길이 접두 배치, Deliver는 카운터와 지연시간만 기록
  lib/client.go   Client: closed-loop 부하 생성기 (K개의 워커가 리더에만 제출, 전달되면 다음 요청 제출)
  lib/runner.go   Run(RunConfig) Result: 노드 생성 → 워밍업(리더 결정 수 ≥ 30) → 측정 창 → 통계
  cmd/trend/      W × RTT 매트릭스 실행, 마크다운 표 출력 + CSV 저장
```

요청 페이로드는 64바이트: 클라이언트 id(8) + 순번(8) + 제출 시각 UnixNano(8) + 패딩.
지연시간은 리더의 `Deliver` 시점 − 제출 시각. 측정은 리더(노드 1)에서만 한다.

노드 설정은 `lib.Config` 참조: `types.DefaultConfig`에 `LeaderRotation=false`, `DecisionsPerLeader=0`,
`PipelineDepthMax=W`, 뷰 체인지가 일어나지 않도록 큰 타임아웃(복잡·하트비트·뷰체인지 30–120 s),
`RequestPoolSize=20000`, `RequestBatchMaxCount=50`, `RequestBatchMaxInterval=50ms`.
W=1 과 W>1 은 `PipelineDepthMax` 만 다르다.

### 실행

```sh
cd bench
go build ./... && go vet ./...
# 기본 매트릭스: W∈{1,2,4,8}, RTT∈{0,50,100}ms, N=4, K=256, 워밍업 30 결정, 셀당 15 s
go run ./cmd/trend -out ../results/<yyyy-mm-dd>-<topic>/trend.csv
# 옵션
go run ./cmd/trend -w 1,4 -rtt 100 -k 1024 -duration 10s -batch 50 -batch-interval 50ms -warmup 30 -n 4
```

표준 에러에 셀마다 한 줄(tps, dec/s, p50/p99, Deliver 평균 시간, 최대 in-flight, 이상 징후)이 출력되고,
표준 출력에 마크다운 표(셀 = TPS / p50)가 출력된다. CSV 열: `w, rtt_ms, ..., tps, lat_p50_ms, lat_p99_ms,
deliver_mean_us, max_in_flight, view_changes, syncs, warnings, dropped, follower_lag, note`.

- `view_changes`: 로그의 "Starting view with number X" (X>0) 횟수 합. 0 이어야 정상.
- `warnings`: 라이브러리의 Warn/Error 로그 수. W>1 이면 리더가 6 s(`LeaderHeartbeatTimeout/Count`)마다
  미결 pre-prepare 를 재전송하므로 이미 전달한 팔로워가 "Got pre-prepare for sequence N but we're in
  sequence M" 경고를 남긴다 — 손실 없는 정상 동작.
- `deliver_mean_us`: 리더 `Application.Deliver` 안에서 보낸 결정당 시간(클라이언트 워커 깨우기 포함, 상한값).
  `View.tryDeliver → Controller.Decide → Deliver` 가 뷰 goroutine에서 동기 실행되므로 이 시간 동안 창이 멈춘다.
- 데이터 레이스 점검: `go run -race ./cmd/trend -w 1,4 -rtt 0,50 -duration 3s -out /tmp/smoke.csv`

## 2차 하네스 (프로세스 분리)

1차 하네스는 4개 노드가 한 프로세스를 공유해 RTT 0 에서 CPU 경합으로 팔로워가 영구히 뒤처졌다.
2차 하네스는 **노드 하나 = OS 프로세스 하나**로 띄우고 실제 TCP 로 연결한다. 라이브러리 설정은 1차와 같이
`lib.Config` (runner.go) 를 그대로 쓴다.

```
bench/
  lib/tcp.go       Transport: 노드별 TCP 리스너 + 피어별 송신 링크(FIFO, 송신 측에서 편도 지연 RTT/2 적용 후 소켓에 기록,
                   끊기면 100 ms 간격 재접속), api.Comm 구현, Sync 용 상태 전송(frameSyncReq/Resp)
  lib/result.go    NodeResult: 노드 프로세스가 끝에 쓰는 JSON
  cmd/node/        노드 프로세스 (리더 프로세스는 closed-loop 클라이언트도 실행)
  cmd/matrix/      오케스트레이터: 셀마다 노드 프로세스 N개 실행 → 워밍업 대기 → 측정 → JSON 수집 → CSV/마크다운 표
```

와이어 포맷: `kind(1) | length(4) | payload`. kind 0 = hello(송신 노드 id), 1 = `smartbftprotos.Message`(protobuf),
2 = 라이브러리가 전달(forward)한 요청(`HandleRequest`), 3 = 원격 클라이언트 제출(`SubmitRequest`),
4/5 = Sync 상태 전송 요청/응답. 피어 쌍마다 방향별 연결 1개. protobuf 마샬링은 링크 goroutine 에서 하므로 합의
goroutine 을 막지 않는다. 링크가 끊긴 동안의 프레임은 버린다(합의는 손실을 견딤; 리더가 죽어도 피어는 살아 있음).

### Sync 선택

라이브러리가 `Synchronizer.Sync()` 를 부르면(팔로워가 창 밖의 커밋 투표를 보거나 하트비트 뒤처짐 감지)
`App.Sync` 는 `Transport.Fetch(latestSeq)` 로 모든 피어에 "내 마지막 순번 이후 결정" 을 요청하고, 처음 온 비어 있지 않은
응답(최대 256 결정, 각 노드 인메모리 `Ledger` 가 보존하는 최근 4096 결정에서 서빙)을 순서대로 `Deliver` 한다.
새로 받은 것이 없을 때까지(최대 64 라운드, 라운드당 3 s + RTT 타임아웃) 반복한 뒤 자기 마지막 결정을 돌려준다.
요청자가 4096 결정보다 더 뒤처져 있으면 응답자는 최신 결정 하나만 보내 요청자가 건너뛰게 한다. 즉 라이브러리의
상태 전송과 별개로 하네스 레벨의 최소 상태 전송을 같은 TCP 연결로 구현했고, 뒤처진 팔로워가 영원히 루프하지 않는다.
sync 횟수는 `syncs`, 서빙 횟수는 `sync_served` 로 기록된다.

### cmd/node 플래그

```
-id N  -n 4  -peers "1=host:port,2=...,3=...,4=..."  [-listen addr]   # -listen 기본값은 -peers 의 자기 항목
-w W  -rtt ms  -batch 50  -batch-interval 50ms
-k K              이 프로세스가 돌릴 closed-loop 워커 수 (기본 0; 처리량 셀은 리더에만 지정)
-submit-to 1,2,3,4   워커가 제출할 노드 (워커별 라운드로빈; 기본은 자기 자신 = 로컬 SubmitRequest)
-resubmit 3s      이 시간 안에 전달되지 않은 요청을 다음 대상에 재제출 (0 = 영원히 대기)
-warmup 30        측정 시작 전 이 노드가 전달해야 하는 결정 수 (-warmup-timeout 60s)
-duration 15s     측정 창 (SIGTERM/SIGUSR2 로 조기 종료)
-result path.json 결과 JSON (lib.NodeResult)
-vc-timers        현실적 뷰 체인지 타이머: RequestForwardTimeout 500ms, Complain 2s, LeaderHeartbeatTimeout 5s/Count 10,
                  ViewChangeTimeout 20s (크래시 셀)
-heartbeat-timeout 60s   LeaderHeartbeatTimeout 만 바꿀 때 (리더의 pre-prepare 재전송 주기 = 이 값/10)
-verbose / -debug  라이브러리 Info 일부 / 전체 로그
```

GOMAXPROCS 는 환경 변수로 준다(오케스트레이터는 노드마다 `GOMAXPROCS=2`). 노드는 리슨 → 모든 피어와 양방향 연결 확립
대기(재시도) → `Consensus.Start` → 클라이언트 시작 → 워밍업 → stdout 에 `READY <unixnano>` → 측정 → JSON 기록 →
`DONE ...` 순서로 동작한다. JSON 에는 측정값 외에 초 단위 전달 요청 수 히스토그램(`per_second`)과 전달 사이 1 s 이상의
공백(`gaps`: 전후 시각·순번, 공백 뒤 15 s 동안의 요청 수)이 들어 있어 크래시 복구 시간을 오케스트레이터가 계산한다.

### cmd/matrix 실행

```sh
cd bench && go build ./... && go vet ./...
# 전체: 처리량 매트릭스 W∈{1,2,4,8} × RTT∈{0,50,100,200} + 크래시 셀(W=4,1 @RTT 50) → matrix.csv, crash.csv
go run ./cmd/matrix -out ../results/<yyyy-mm-dd>-<topic> -logdir /tmp/bench2-logs
# 옵션
go run ./cmd/matrix -mode matrix -w 1,4 -rtt 0 -duration 3s -out /tmp/smoke -logdir /tmp/smoke-logs      # 스모크
go run ./cmd/matrix -mode crash -crash-w 4,1 -crash-rtt 50 -crash-after 5s -crash-duration 40s ...
go run ./cmd/matrix -k 1024 -batch 50 -batch-interval 50ms -warmup 30 -duration 15s -gomaxprocs 2 -base-port 23000
```

셀 i 는 포트 `base+10*i+id` 를 쓴다. 처리량 셀: 노드 1 이 리더이며 K 워커를 자기에게 제출한다. 모든 노드가 READY 를
찍으면 각자 -duration 동안 측정하고 종료한다. 행은 리더(노드 1)의 측정값이며 `view_changes/syncs/warnings/dropped` 는
노드 합, `follower_lag` 는 종료 시 리더 결정 수 − 가장 느린 팔로워 결정 수.

크래시 셀: 모든 노드 `-vc-timers`; 클라이언트는 노드 4 에서 돌며(`-crash-client-node`, `-k K -submit-to 1,2,3,4 -resubmit 3s`)
리더가 죽어도 살아 있다 (다음 리더인 노드 2 에 두면 클라이언트 부하로 뒤처진 노드가 새 리더가 되어 뷰 체인지가 길어졌다).
K 는 `-crash-k`(기본 256): K=1024 면 W=1 의 요청 지연(≈1.7 s)+전달(forward) 0.5 s 가 `RequestComplainTimeout` 2 s 를 넘어
부하만으로 뷰 체인지가 난다. 모든 노드 READY 후 `-crash-after`(5 s) 뒤 노드 1 에 SIGKILL (T0). 살아남은 노드마다 crash.csv 에
`tail_ms`(T0 뒤 첫 전달 — 생존자 셋이 이미 받은 pre-prepare 를 마무리하는 파이프라인 꼬리), `first_view_start_ms`
(첫 "Starting view with number X>0"), `recovery_ms`(**그 뷰 시작 뒤 첫 전달** − T0), 뷰 시작 수, "In-flight view ... has
committed a decision" 수, 크래시 전 TPS, 복구 뒤 15 s TPS, `clock_jump_ms`(벽시계 − 단조시계; 2 s 이상이면 머신이 잠든 것)
를 쓴다. 처리량 셀의 `Fetch` 는 연결된 피어의 응답만 기다리므로(죽은 리더는 제외) 뷰 체인지 중의 Sync 가 타임아웃까지
멈추지 않는다. 노트북에서는 `caffeinate -is go run ./cmd/matrix ...` 로 돌려야 한다 — 1차 실행 때 유휴 잠자기로 측정 창이
17 분이 된 셀이 있었다(JSON 의 `clock_jump_ms` 와 CSV note 로 감지).
stderr 로그는 `-logdir/<cell>-node<id>.log`, 원시 JSON 은 `-logdir/<cell>-node<id>.json`.
