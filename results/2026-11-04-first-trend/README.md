# 2026-11-04 첫 트렌드 실험: 파이프라인 깊이 W × RTT

첫 하네스(`bench/`)로 돌린 1회 실행. 추세 확인용이며 수치 자체는 반복 측정 전 값이다.

## 실행

```sh
cd bench && go build -o /tmp/trend ./cmd/trend
/tmp/trend -w 1,2,4,8 -rtt 0,50,100 -duration 15s -out ../results/2026-11-04-first-trend/trend.csv
# 보조 실행 (K=1024, RTT 100 만)
/tmp/trend -w 1,2,4,8 -rtt 100 -k 1024 -duration 15s -out ../results/2026-11-04-first-trend/trend-k1024-rtt100.csv
```

- 머신: Apple M1 Pro (10 CPU), 16 GB, macOS 14.6 (Darwin 24.6.0); Go go1.27.3 darwin/arm64
- 포크: `SmartBFT` main @ 323db6b2. 단일 프로세스, N=4, 인메모리 네트워크(링크별 FIFO + 편도 지연 RTT/2), 인메모리 WAL
- 부하: closed-loop K=256 (리더에만 제출), 요청 64 B, `RequestBatchMaxCount=50`, `RequestBatchMaxInterval=50ms`
- 측정: 리더 결정 30회 워밍업 후 15 s. `LeaderRotation=false`, `DecisionsPerLeader=0` 고정; 셀 간 차이는 `PipelineDepthMax`(W)와 지연만
- 뷰 체인지: 전 셀 0회

## 결과 (셀 = TPS / p50 지연, K=256)

| RTT (ms) | W=1 | W=2 | W=4 | W=8 |
|---|---|---|---|---|
| 0 ※ | 182,103 / 1.3 ms | 215,724 / 1.0 ms | 85,745 / 1.0 ms | 58,855 / 1.1 ms |
| 50 | 613 / 407 ms | 1,213 / 242 ms | 2,425 / 89 ms | 3,077 / 81 ms |
| 100 | 317 / 783 ms | 633 / 467 ms | 1,253 / 160 ms | 1,573 / 156 ms |

보조: RTT 100 ms, K=1024

| RTT (ms) | W=1 | W=2 | W=4 | W=8 |
|---|---|---|---|---|
| 100 | 317 / 3,145 ms | 633 / 1,576 ms | 1,253 / 790 ms | 2,506 / 470 ms |

결정당 `Deliver` 소요 시간(리더, 클라이언트 워커 깨우기 포함): RTT 0 에서 12–13 µs, RTT 50/100 에서 60–130 µs.
리더 최대 in-flight(`view_in_flight_proposals`): W 와 같음 (단 K=256, W=8 은 6–8).

## 읽기

1. **RTT 50/100 에서 TPS 는 W 에 거의 선형으로 증가한다.** W=1→2→4 에서 2배씩 (613→1,213→2,425; 317→633→1,253),
   p50 지연은 반대로 줄어든다(closed-loop 이므로 TPS ≈ K / 지연). W=1 의 결정 주기(≈81 ms @RTT 50, ≈158 ms @RTT 100)는
   3 phase × 편도 지연 + 배치 간격과 맞아, 순차 모드가 RTT 에 묶여 있음을 보여준다.
2. **K=256 에서는 W=4→8 에서 꺾인다**(2,425→3,077, 1,253→1,573). 이는 프로토콜의 knee 가 아니라 부하 상한이다:
   256 개 미결 요청으로는 50 개 배치를 8 개 채울 수 없어 W=8 셀의 배치 평균이 43–46 요청으로 떨어졌다.
   K=1024 로 올리면 RTT 100 에서 W=8 이 2,506 TPS, 즉 W=1 의 정확히 8 배가 된다 — 이 범위에서는 knee 가 보이지 않는다.
   다음 실험은 K ≥ W × 배치 크기 × 2 로 잡고 W=16 까지 확장할 것.
3. **RTT 0 은 CPU 경합 영역이며 결과를 믿기 어렵다(※).** 4 노드 + 12 링크 goroutine + 256 워커가 10 코어를 다투고,
   팔로워 하나가 몇 µs 밀리면 라이브러리가 "Seen 2 votes ... but I am in seq N" 경고 뒤 `Sync()` 를 호출해 뷰를 재시작한다
   (`view_in_flight` 창을 벗어난 커밋을 보면 동기화). W=2/4 셀은 sync 4,773/407 회, 측정 종료 시 가장 느린 팔로워가
   52,110/10,696 결정 뒤처진 채였다(하네스 레저 보존 4,096 결정을 넘어 복구 불가). 따라서 RTT 0 의 W 추세
   (182k→216k→86k→59k) 는 파이프라인 효과가 아니라 sync 폭주의 유무를 반영한다. RTT 0 셀은 노드별 프로세스 분리나
   GOMAXPROCS 핀 등으로 다시 설계해야 한다.
4. **Deliver 블로킹은 병목이 아니다(리뷰어 질문 ③).** `View.tryDeliver → Controller.Decide → Application.Deliver` 가
   뷰 goroutine 에서 동기로 돌지만, 결정당 Deliver 는 RTT 0 에서 ≈13 µs(3,600 결정/s × 13 µs ≈ 뷰 goroutine 시간의 5 %),
   RTT 50/100 에서 ≈100 µs 로 결정 주기(20–160 ms)의 0.1 % 수준이다. 실제 애플리케이션(디스크 fsync, 블록 검증)에서는
   달라질 수 있으므로 Deliver 에 인위적 지연을 넣는 셀을 다음 실험에 추가할 것.

## 이상 징후 (숨기지 않음)

- RTT 0 전 셀: `Seen 2 votes for digest ... in view 0, sequence N but I am in view 0 and seq N-2..N-9` 경고 후 `Sync()` 호출.
  W=1: 83 회, W=2: 4,773 회(팔로워 1 개 영구 지연), W=4: 407 회(영구 지연), W=8: 38 회. 위 3번 참조.
  (이전 실행에서는 하네스의 `Sync()` 가 catch-up 을 하지 않아 W=1 에서 51,593 회 sync 가 났고, 공유 레저로 고쳤다.)
- W>1 셀: `Got pre-prepare for sequence N but we're in sequence N+k` 경고 6–12 회/셀. 리더가 6 s
  (`LeaderHeartbeatTimeout/LeaderHeartbeatCount`)마다 미결 pre-prepare 를 재전송하면 이미 전달한 팔로워가 남기는 로그.
  손실·뷰 체인지 없음.
- 설정 거부(Validate 오류), 제출 실패(pool full), 링크 버퍼 드롭: 없음.

## 파일

- `trend.csv` — 12 셀 원시 값 (열: w, rtt_ms, decisions, requests, tps, lat_p50_ms, lat_p99_ms, deliver_mean_us, max_in_flight, view_changes, syncs, warnings, dropped, follower_lag, note …)
- `trend-k1024-rtt100.csv` — 보조 실행 4 셀
