# 브랜치 정책 (SmartBFT 포크 · PipeBFT 연구)

## 0. 저장소 구성

| 저장소 | 역할 | upstream |
|---|---|---|
| `<me>/SmartBFT` | 합의 라이브러리 포크. 프로토콜 수정은 전부 여기 | hyperledger-labs/SmartBFT |
| `<me>/fabric` | Fabric 포크. `orderer/consensus/smartbft/` 만 수정 | hyperledger/fabric |
| `<me>/SmartBFT_Pipeline` (PipeBFT) | 연구 저장소. 벤치 스크립트·netem 설정·결과·논문 그림 | 없음 |

포크 직후 반드시 (스크립트: `scripts/setup-fork.sh`):

```bash
git remote add upstream https://github.com/hyperledger-labs/SmartBFT.git
git fetch upstream
git tag baseline upstream/main        # 비교군(원본)의 기준점. 절대 옮기지 않음
git push origin baseline
```

`baseline` 태그가 가리키는 커밋이 논문의 "기존 SmartBFT"입니다. 실험 비교군은 항상 이 태그로 빌드합니다.

## 1. 브랜치 종류

```
main                       통합 브랜치. 직접 커밋 허용 (아래 1-1 규칙 내에서)
├─ probe/<topic>           코드 읽기·로그 삽입·실험용. 병합하지 않음. 결론은 docs/로 옮긴 뒤 삭제
├─ feat/<area>             프로토콜·동작 변경. PR로만 main에 병합
├─ exp/<yyyymmdd>-<topic>  특정 실험 시점의 코드 고정. 병합하지 않고 태그로만 남김
└─ sync/upstream-<date>    upstream 변경 병합 작업용. 충돌 해결 후 main에 병합
```

### 1-1. main에 직접 커밋해도 되는 것

- 문서(`docs/`), 체크리스트, 설계 노트
- 계측·메트릭 추가 (`pkg/metrics`, 로그 라인) — 프로토콜 동작을 바꾸지 않는 것
- 설정 필드 추가 (`pkg/types/config.go`)로서 기본값이 원본과 동일한 동작을 내는 것
- 테스트 추가, 빌드·CI 스크립트
- 포맷·주석·오타

### 1-2. 반드시 feat/ 브랜치 + PR로 가야 하는 것

- `internal/bft/{view,controller,viewchanger,batcher,requestpool,state}.go` 의 동작 변경
- `smartbftprotos/*.proto` 변경
- `pkg/api` 인터페이스 변경 (Fabric 쪽에 파급됨)
- Fabric 포크의 `orderer/consensus/smartbft/` 변경

**판단 기준 한 줄:** `baseline` 과 같은 설정으로 돌렸을 때 메시지 흐름이 1바이트라도 달라지면 `feat/`.

## 2. 기능 플래그 원칙 (가장 중요)

모든 프로토콜 변경은 설정 플래그 뒤에 두고, **플래그 기본값은 원본 동작**이어야 합니다.

```go
// pkg/types/config.go
PipelineDepthMax   uint64        // 1 = 원본 동작 (기본값)
AdaptiveBatch      bool          // false = 원본 동작 (기본값)
```

이 원칙 덕분에:

- `feat/` 브랜치를 main에 병합해도 main은 여전히 baseline과 동일하게 동작 → 실험 비교군을 main 빌드 하나로 플래그만 바꿔 돌릴 수 있음
- 뷰 체인지처럼 반쯤 완성된 기능도 플래그가 꺼져 있으면 병합 가능 → 긴 브랜치가 썩지 않음
- 논문의 "제안 기법 = 원본 + diff"가 명확해짐

플래그 없이 원본 동작을 바꾸는 PR은 병합하지 않습니다.

## 3. feat/ 브랜치 목록과 순서

의존 관계 순입니다. 앞 브랜치가 main에 들어간 뒤 다음을 main에서 땁니다 (**스택 금지** — 한 번에 하나씩).

| 순서 | 브랜치 | 범위 | 플래그 |
|---|---|---|---|
| 1 | `feat/metrics-round-latency` | pre-prepare→commit 지연, 풀 길이, 배치 크기 메트릭 | 없음 (계측만, main 직접 커밋도 가능) |
| 2 | `feat/adaptive-batch` | `batcher.go` 파라미터 동적화 + 제어기 | `AdaptiveBatch` |
| 3 | `feat/leader-token-window` | `controller.go` leaderToken → 깊이 W 세마포어 | `PipelineDepthMax` |
| 4 | `feat/view-seq-window` | `view.go` current/next 2슬롯 → `map[seq]` 윈도우 | `PipelineDepthMax` |
| 5 | `feat/prev-commit-dependency` | `prev_commit_signatures` 의존성 해소 (설계 결정 후) | `PipelineDepthMax` |
| 6 | `feat/viewchange-multi-inflight` | `ViewData.in_flight_proposal` → repeated, 복구 규칙 | `PipelineDepthMax` |
| 7 | `feat/config-barrier` | 설정 트랜잭션 시 파이프라인 flush | `PipelineDepthMax` |
| 8 | (fabric) `feat/smartbft-pipeline-verifier` | PreviousHash·VerificationSequence 검증 완화 | Fabric 채널 설정 |

3~4는 합쳐서 하나로 가도 됩니다. 5·6은 분리 유지 (각각 안전성 논증이 필요하므로 diff를 독립적으로 보여줘야 함).

## 4. PR 병합 규칙

- `git rebase main` 후 PR. 병합은 `--no-ff` (merge commit 유지 — 논문에서 "이 변경은 커밋 X" 로 지칭하기 위해)
- 병합 전 통과해야 하는 것:

```bash
go build ./... && go vet ./...
go test ./internal/... ./pkg/...
go test ./test/...            # 인메모리 네트워크 테스트. 느리므로 -run 으로 범위 지정 가능
```

- 플래그 OFF 상태에서 기존 테스트 전부 통과 + 플래그 ON 테스트 최소 1개 추가
- 셀프 리뷰 체크리스트를 PR 본문에 붙임 (`docs/verification-checklist.md` 해당 절)
- 혼자 작업이므로 승인자는 없지만, PR 본문은 반드시 작성 — 나중에 논문 구현 장을 쓸 때 PR 본문이 1차 자료가 됨. 내용: 무엇을/왜/안전성에 미치는 영향/플래그/테스트 (템플릿: `templates/PULL_REQUEST_TEMPLATE.md`)

## 5. probe/ 브랜치 사용법

코드를 읽다가 로그를 박아 돌려보고 싶을 때 씁니다.

- 자유롭게 커밋. 병합 안 함
- 알아낸 것은 `docs/notes/<topic>.md` 에 정리해서 main에 직접 커밋
- 정리가 끝나면 브랜치 삭제 (`git branch -D`). 남겨두면 어느 게 진짜인지 헷갈림
- 예: `probe/leader-token-flow`, `probe/viewchange-trace`

## 6. exp/ 브랜치와 태그

실험을 돌린 시점의 코드를 고정합니다.

```bash
git checkout -b exp/20261115-rtt-sweep main
# 실험용 임시 설정·스크립트 커밋
git tag exp-20261115-rtt-sweep
git push origin exp-20261115-rtt-sweep
```

결과 데이터는 `SmartBFT_Pipeline/results/20261115-rtt-sweep/` 에 두고 README에 태그 이름을 적습니다. 코드 태그 ↔ 결과 디렉터리가 1:1이면 재현성 논란이 없습니다.

논문 제출 시점에는 `paper-v1` 태그를 main에 찍습니다.

## 7. upstream 동기화

- 주기: 2~4주에 한 번, 또는 upstream에 `view.go`/`viewchanger.go` 변경이 보일 때
- 절차:

```bash
git fetch upstream
git checkout -b sync/upstream-20261201 main
git merge upstream/main          # rebase 금지 — main 이력을 다시 쓰면 exp 태그가 어긋남
# 충돌 해결, 테스트
git checkout main && git merge --no-ff sync/upstream-20261201
```

- `baseline` 태그는 옮기지 않습니다. upstream이 많이 바뀌어 비교군을 갱신해야 하면 `baseline-2` 를 새로 찍고 논문에 두 버전을 명시
- 병합이 심하게 충돌하기 시작하면(특히 `view.go`), 그 시점부터는 동기화를 멈추고 "fork point: upstream 커밋 X" 로 고정하는 것이 연구에는 더 안전합니다

## 8. 커밋 메시지

```
<area>: <한 줄 요약>

<왜 바꿨는지, 어떤 동작이 달라지는지>
Flag: PipelineDepthMax (default 1 = unchanged)
Refs: docs/notes/prev-commit-dependency.md
```

`area` 는 `view`, `controller`, `viewchanger`, `batcher`, `proto`, `api`, `metrics`, `docs`, `test`, `bench` 중 하나. upstream 커밋 스타일(`bft: ...`)과 비슷하게 유지하면 나중에 기여 PR로 올릴 때도 편합니다.

## 9. 금지 사항

- `baseline` 태그 이동·삭제
- `main` 에서 `git push --force`
- 플래그 없는 프로토콜 변경
- `feat/` 브랜치 간 스택 (A 위에 B를 따서 작업) — A가 바뀌면 B 리베이스 지옥
- `vendor/` 수동 편집 (`go mod vendor` 로만)
