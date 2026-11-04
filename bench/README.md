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
