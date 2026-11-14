"""生成资费计划（几百种）、资费版本与用户订阅。

用法:
  python3 -I gen_config.py --out DATA_DIR --users 50000 --plans 240 --seed 20261001

输出:
  DATA_DIR/tariffs/PLAN_vN.json   资费版本（每计划 v1/v2 两版）
  DATA_DIR/subscriptions.jsonl    用户订阅区间
  DATA_DIR/config_manifest.json   生成参数与规模
"""
from __future__ import annotations

import argparse
import json
import os
import random
import sys
from datetime import datetime, timedelta, timezone

# 隔离模式（python -I）不自动加入脚本目录，显式引导
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from domain import SERVICES, TIME_FMT, bill_month

UTC = timezone.utc
WINDOW_START = datetime(2026, 10, 1, tzinfo=UTC)
WINDOW_END = datetime(2026, 12, 1, tzinfo=UTC)
MID_OCT = datetime(2026, 10, 15, tzinfo=UTC)
NOV1 = datetime(2026, 11, 1, tzinfo=UTC)


def iso(dt: datetime) -> str:
    return dt.strftime(TIME_FMT)


def service_config(rng: random.Random, service: str, v2: bool) -> dict:
    # 费率单位：厘/批价单位
    if service == "voice":
        rate_base = rng.choice([8000, 10000, 12000, 15000, 18000])
        cfg = {
            "unit_size": 60,
            "rounding": "up",
            "allowance_units": rng.choice([0, 100, 200, 300, 500, 800]),
            "tiers": [
                {"upto": 100, "rate": rate_base},
                {"upto": 300, "rate": int(rate_base * 0.9)},
                {"upto": -1, "rate": int(rate_base * 0.75)},
            ],
            "night_window": ["23:00", "07:00"],
            "night_factor": rng.choice([4000, 5000, 6000]),
            "roaming_factor": rng.choice([15000, 20000, 25000]),
        }
    elif service == "sms":
        rate_base = rng.choice([500, 800, 1000, 1500])
        cfg = {
            "unit_size": 1,
            "rounding": "up",
            "allowance_units": rng.choice([0, 100, 200, 500]),
            "tiers": [
                {"upto": 200, "rate": rate_base},
                {"upto": -1, "rate": int(rate_base * 0.8)},
            ],
            "roaming_factor": rng.choice([10000, 15000, 20000]),
        }
    else:  # data
        rate_base = rng.choice([150, 200, 300, 500])  # 厘/MB
        cfg = {
            "unit_size": 1048576,
            "rounding": "up",
            "allowance_units": rng.choice([0, 5120, 10240, 20480, 51200]),  # MB
            "tiers": [
                {"upto": 10240, "rate": rate_base},
                {"upto": 51200, "rate": int(rate_base * 0.8)},
                {"upto": -1, "rate": int(rate_base * 0.6)},
            ],
            "night_window": ["23:00", "07:00"],
            "night_factor": rng.choice([5000, 7000, 10000]),
            "roaming_factor": rng.choice([15000, 20000, 30000]),
        }
    if v2:
        # 资费变更：单价整体下调 5%~15%，部分计划调整免费额度
        for tier in cfg["tiers"]:
            tier["rate"] = max(1, int(tier["rate"] * rng.uniform(0.85, 0.95)))
        if rng.random() < 0.4:
            cfg["allowance_units"] = int(cfg["allowance_units"] * rng.choice([1.2, 1.5, 0.8]))
    return cfg


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", required=True)
    ap.add_argument("--users", type=int, default=50000)
    ap.add_argument("--plans", type=int, default=240)
    ap.add_argument("--seed", type=int, default=20261001)
    ap.add_argument("--change-ratio", type=float, default=0.25,
                    help="月中换套餐用户比例")
    args = ap.parse_args()

    rng = random.Random(args.seed)
    tariff_dir = os.path.join(args.out, "tariffs")
    os.makedirs(tariff_dir, exist_ok=True)

    plan_ids = [f"P{i:04d}" for i in range(args.plans)]
    # v2 生效点：一半 10-15，一半 11-01
    v2_start = {}
    for i, pid in enumerate(plan_ids):
        v2_start[pid] = MID_OCT if i % 2 == 0 else NOV1

    for pid in plan_ids:
        vs = v2_start[pid]
        v1 = {
            "plan_id": pid, "version": "v1",
            "effective_start": iso(WINDOW_START), "effective_end": iso(vs),
            "services": {s: service_config(rng, s, False) for s in SERVICES},
        }
        v2 = {
            "plan_id": pid, "version": "v2",
            "effective_start": iso(vs), "effective_end": iso(WINDOW_END),
            "services": {s: service_config(rng, s, True) for s in SERVICES},
        }
        with open(os.path.join(tariff_dir, f"{pid}_v1.json"), "w") as fp:
            json.dump(v1, fp, ensure_ascii=False, indent=1)
        with open(os.path.join(tariff_dir, f"{pid}_v2.json"), "w") as fp:
            json.dump(v2, fp, ensure_ascii=False, indent=1)

    # 订阅：每用户每业务一条基础订阅；change_ratio 用户在 10-15 / 11-01 换套餐
    sub_path = os.path.join(args.out, "subscriptions.jsonl")
    change_dates = [MID_OCT, NOV1]
    n_changed = 0
    with open(sub_path, "w") as fp:
        for u in range(args.users):
            user_id = "166%08d" % u
            changes = rng.random() < args.change_ratio
            for service in SERVICES:
                base_plan = rng.choice(plan_ids)
                if not changes:
                    fp.write(json.dumps({
                        "user_id": user_id, "service": service, "plan_id": base_plan,
                        "effective_start": iso(WINDOW_START), "effective_end": iso(WINDOW_END),
                    }, ensure_ascii=False) + "\n")
                    continue
                # 换套餐：两段订阅，边界与某 v2 生效点对齐（保证版本/订阅边界都出现）
                cut = rng.choice(change_dates)
                next_plan = rng.choice([p for p in plan_ids if p != base_plan])
                n_changed += 1
                fp.write(json.dumps({
                    "user_id": user_id, "service": service, "plan_id": base_plan,
                    "effective_start": iso(WINDOW_START), "effective_end": iso(cut),
                }, ensure_ascii=False) + "\n")
                fp.write(json.dumps({
                    "user_id": user_id, "service": service, "plan_id": next_plan,
                    "effective_start": iso(cut), "effective_end": iso(WINDOW_END),
                }, ensure_ascii=False) + "\n")

    manifest = {
        "generated_at": iso(datetime.now(UTC)),
        "users": args.users, "plans": args.plans, "seed": args.seed,
        "change_ratio": args.change_ratio,
        "subscriptions_with_change": n_changed,
        "window": [iso(WINDOW_START), iso(WINDOW_END)],
        "bill_months": sorted({bill_month(WINDOW_START), bill_month(NOV1)}),
    }
    with open(os.path.join(args.out, "config_manifest.json"), "w") as fp:
        json.dump(manifest, fp, ensure_ascii=False, indent=2)
    print(f"[gen_config] plans={args.plans} (x2 versions) users={args.users} "
          f"changed-subscriptions={n_changed} -> {args.out}")


if __name__ == "__main__":
    main()
