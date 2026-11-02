# bench

벤치마크 스크립트. 클라이언트 부하 생성, 메트릭 수집, TPS·지연시간 계산.

- 비교군은 항상 SmartBFT 포크의 `baseline` 태그로 빌드
- 제안 기법은 `main` (또는 `exp-*` 태그) 빌드에 플래그만 바꿔서 실행
  - `PipelineDepthMax=1`, `AdaptiveBatch=false` → 원본 동작
  - `PipelineDepthMax=W`, `AdaptiveBatch=true` → 제안 기법
- 결과는 `results/<yyyymmdd>-<topic>/` 에 저장
