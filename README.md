# SmartBFT_Pipeline

Hyperledger Fabric SmartBFT의 합의 파이프라이닝 및 적응형 배치 프로토콜 연구

## 연구 목표

Hyperledger Fabric 3.x에서 사용하는 SmartBFT 합의 알고리즘의 처리 성능을 개선합니다.

## 배경 및 문제점

- 기존 SmartBFT는 합의를 순차적으로 처리하는 **비파이프라인 구조**로 설계되어 있어, 네트워크 지연이 증가하거나 트랜잭션 요청이 많아지는 환경에서 처리량이 제한됩니다.
- **고정된 배치 설정**은 트랜잭션 요청량과 네트워크 상태 변화에 유연하게 대응하기 어렵습니다.

## 제안 기법

SmartBFT 원본 소스 코드를 수정하여 다음 두 가지를 도입합니다.

1. **합의 파이프라이닝**: 여러 블록의 합의를 중첩 수행하는 구조 (`PipelineDepthMax`)
2. **적응형 배치 프로토콜**: 네트워크 상태와 트랜잭션 부하에 따라 배치 크기 및 파이프라인 깊이를 동적으로 조절 (`AdaptiveBatch`)

## 평가 계획

기존 SmartBFT(`baseline` 태그)와 제안 기법을 다음 항목에서 비교합니다.

- TPS (초당 트랜잭션 처리량)
- 합의 지연시간
- 노드 수 증가에 따른 성능 변화
- Byzantine 장애 및 리더 변경 상황에서의 합의 안전성과 정상 복구 여부

## 저장소 구성

이 저장소는 **연구 저장소**입니다. 코드 수정은 별도 포크에서 합니다.

| 저장소 | 역할 |
|---|---|
| SmartBFT 포크 | 합의 라이브러리. 프로토콜 수정은 전부 여기 |
| fabric 포크 | `orderer/consensus/smartbft/` 만 수정 |
| **SmartBFT_Pipeline** (이 저장소) | 벤치 스크립트·netem 설정·실험 결과·논문 그림·설계 노트 |

```
docs/
├─ branch-policy.md          브랜치·플래그·PR·태그 규칙 (반드시 읽기)
├─ verification-checklist.md feat/ PR 셀프 리뷰 체크리스트
└─ notes/                    probe 결론, 설계 노트
bench/                       벤치마크 스크립트
netem/                       네트워크 지연 시나리오
results/<yyyymmdd>-<topic>/  실험 결과 (exp 태그와 1:1)
figures/                     논문 그림
scripts/setup-fork.sh        포크 직후 upstream·baseline 태그 설정
templates/                   PR 템플릿, 커밋 메시지 양식 (포크에 복사)
```

## 시작하기

1. `hyperledger-labs/SmartBFT`, `hyperledger/fabric` 을 포크하고 클론
2. 각 포크에서 `scripts/setup-fork.sh smartbft|fabric` 실행 → `baseline` 태그 생성
3. `templates/PULL_REQUEST_TEMPLATE.md` 를 SmartBFT 포크의 `.github/` 에 복사
4. [docs/branch-policy.md](docs/branch-policy.md) 3절 순서대로 `feat/` 브랜치 진행
