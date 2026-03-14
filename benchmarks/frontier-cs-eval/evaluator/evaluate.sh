#!/usr/bin/env bash
set -euo pipefail

# The Frontier-CS judge server must be running on the host (or reachable
# via JUDGE_URLS env var).  Start it before running this benchmark:
#   cd Frontier-CS/algorithmic && docker compose up -d

python /benchmark/evaluator.py "$1"
