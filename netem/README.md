# netem

네트워크 지연·손실 시나리오 (`tc netem`) 설정.

시나리오 파일은 `<name>.sh` 로 두고 결과 README의 `netem:` 필드에 이름을 적습니다.

예정 시나리오:

- `rtt-sweep`: RTT 0 / 50 / 100 / 200 ms
- `loss`: 패킷 손실 0.1 / 1 %
- `asymmetric`: 리더만 지연 증가
