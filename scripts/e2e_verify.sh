#!/usr/bin/env bash
# 端到端验收：生成数据 -> Go 引擎批价 -> 独立 oracle -> 逐用户对账 -> 重算 -> 断点续跑
# 用法: bash scripts/e2e_verify.sh [数据条数，默认 100000] [数据目录，默认 /tmp/rating-e2e]
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
COUNT="${1:-100000}"
DATA="${2:-/tmp/rating-e2e}"
SHARDS=16
PARALLEL=8
SEED=20261001

echo "=== 0. 环境 ==="
go version
python3 --version
rm -rf "$DATA"
mkdir -p "$DATA"

echo "=== 1. 生成资费/订阅（脚本独立实现）==="
python3 -I "$ROOT/scripts/gen_config.py" --out "$DATA" --users 300 --plans 16 --seed "$SEED"

echo "=== 2. 生成 ${COUNT} 条话单（含重复/乱码/坏时间/坏用量/无订阅/坏拆段/跨月）==="
python3 -I "$ROOT/scripts/gen_cdrs.py" --data "$DATA" --count "$COUNT" --shards "$SHARDS" --seed "$SEED"

echo "=== 3. 构建 Go 引擎 ==="
go build -o "$DATA/engine" "$ROOT/cmd/engine"

echo "=== 4. 执行批价任务 ==="
"$DATA/engine" run --data "$DATA" --shards "$SHARDS" --parallel "$PARALLEL" --name e2e

echo "=== 5. 独立应收 oracle（Python，与 Go 无共享代码）==="
python3 -I "$ROOT/scripts/oracle.py" --data "$DATA"

echo "=== 6. 逐用户×月对账（金额单位：厘，要求 0 差异）==="
"$DATA/engine" reconcile --data "$DATA" --task e2e

echo "=== 7. 断点续跑：注入失败分片后 retry ==="
python3 - "$DATA" <<'PY'
import json, sys, glob
data = sys.argv[1]
mp = f"{data}/tasks/e2e/task.json"
m = json.load(open(mp)); m["status"] = "FAILED"
for sh in (2, 5):
    p = f"{data}/tasks/e2e/shard-{sh:04d}/checkpoint.json"
    c = json.load(open(p)); c["status"] = "FAILED"; c["error"] = "injected"
    m["shards"][sh]["status"] = "FAILED"
    json.dump(c, open(p, "w"), indent=2)
json.dump(m, open(mp, "w"), indent=2)
print("injected FAILED on shards 2,5")
PY
"$DATA/engine" retry --data "$DATA" --task e2e
"$DATA/engine" reconcile --data "$DATA" --task e2e

echo "=== 8. 资费变更重算（11 月窗口）==="
cp -r "$DATA/tariffs" "$DATA/tariffs_bak"
python3 - "$DATA" <<'PY'
import json, glob, sys
data = sys.argv[1]
p = glob.glob(f"{data}/tariffs/P0000_v2.json")[0]
t = json.load(open(p))
for tier in t["services"]["voice"]["tiers"]:
    tier["rate"] = int(tier["rate"] * 1.1)
json.dump(t, open(p, "w"), indent=1)
PY
"$DATA/engine" recompute --data "$DATA" --old e2e \
  --start 2026-11-01T00:00:00Z --end 2026-12-01T00:00:00Z --name e2e-rec
rm -rf "$DATA/tariffs"; mv "$DATA/tariffs_bak" "$DATA/tariffs"

echo ""
echo "=== 全部验收通过 ✅ ==="
"$DATA/engine" show --data "$DATA" --task e2e
