# results

실험 결과. 디렉터리 하나 = 코드 태그 하나 (1:1).

```
results/
└─ <yyyymmdd>-<topic>/        ← SmartBFT 포크의 exp-<yyyymmdd>-<topic> 태그와 동일 이름
   ├─ README.md               ← 아래 템플릿
   ├─ raw/                    ← 원본 로그·메트릭 덤프
   └─ summary.csv             ← 집계 결과
```

## README 템플릿

```markdown
# <yyyymmdd>-<topic>

- 코드 태그: exp-<yyyymmdd>-<topic> (SmartBFT 포크), 커밋:
- 비교군: baseline 태그, 커밋:
- Fabric 포크 커밋:
- 플래그: PipelineDepthMax= / AdaptiveBatch=
- 노드 수 / f:
- netem:
- 클라이언트 부하 (TPS 목표, 지속 시간):

## 결과 요약
## 그림 (figures/ 링크)
## 비고
```
