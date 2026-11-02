#!/usr/bin/env bash
# 포크 직후 1회 실행. upstream 리모트를 추가하고 baseline 태그를 찍어 push합니다.
# 사용법:
#   scripts/setup-fork.sh smartbft   (SmartBFT 포크 디렉터리에서)
#   scripts/setup-fork.sh fabric     (fabric 포크 디렉터리에서)
set -euo pipefail

case "${1:-}" in
  smartbft) UPSTREAM=https://github.com/hyperledger-labs/SmartBFT.git ;;
  fabric)   UPSTREAM=https://github.com/hyperledger/fabric.git ;;
  *) echo "usage: $0 {smartbft|fabric}" >&2; exit 1 ;;
esac

if git rev-parse -q --verify refs/tags/baseline >/dev/null; then
  echo "baseline 태그가 이미 있습니다. 절대 옮기지 마세요." >&2
  exit 1
fi

git remote get-url upstream >/dev/null 2>&1 || git remote add upstream "$UPSTREAM"
git fetch upstream
git tag baseline upstream/main
git push origin baseline
echo "baseline -> $(git rev-parse --short baseline)"
