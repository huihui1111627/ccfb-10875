"""独立应收（oracle）：不依赖 Go 引擎任何代码，按 docs/SPEC.md 独立重放全部规则。

用法:
  python3 -I oracle.py --data DATA_DIR

输出:
  DATA_DIR/oracle/user_summary.jsonl   逐用户×账期月应收（金额单位：厘）
  DATA_DIR/oracle/rated_sample.jsonl   批价明细样本（每用户最多 3 条，供抽查）
  DATA_DIR/oracle/exception_counts.json 按原因的异常计数（与引擎交叉验证）
  DATA_DIR/oracle/oracle_manifest.json  规模与总额汇总

确定性约定（与 Go 引擎一致）:
  * 全部比例运算使用整数乘除 //，金额无浮点
  * 输入文件按文件名排序；分片键 fnv32a(user_id) % N（仅用于计数核对，不影响金额）
  * DER 流遇到第一条不可解码记录即停止该文件后续解析
"""
from __future__ import annotations

import argparse
import json
import os
import struct
import sys
from collections import defaultdict
from datetime import datetime, timedelta, timezone

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from domain import (DELIM, E1, E2, E3, E4, E5, FIXED_WIDTH, bill_month,
                    fnv32a, parse_delimited, parse_fixed, parse_time)
from der import DERError, decode_cdr

UTC = timezone.utc
BASIS = 10000
MONTH_FMT = "%Y-%m"


# ---------------------------------------------------------------- 配置加载

def load_tariffs(tariff_dir: str) -> tuple[dict, dict]:
    by_plan: dict[str, list[dict]] = defaultdict(list)
    for name in sorted(os.listdir(tariff_dir)):
        if not name.endswith(".json"):
            continue
        with open(os.path.join(tariff_dir, name)) as fp:
            t = json.load(fp)
        t["_start"] = parse_time(t["effective_start"])
        t["_end"] = parse_time(t["effective_end"])
        by_plan[t["plan_id"]].append(t)
    for plan in by_plan:
        by_plan[plan].sort(key=lambda x: x["_start"])
    return by_plan, {pid: vs for pid, vs in by_plan.items()}


def load_subscriptions(path: str) -> dict:
    subs: dict[str, dict[str, list[dict]]] = defaultdict(lambda: defaultdict(list))
    with open(path) as fp:
        for line in fp:
            s = json.loads(line)
            s["_start"] = parse_time(s["effective_start"])
            s["_end"] = parse_time(s["effective_end"])
            subs[s["user_id"]][s["service"]].append(s)
    for u in subs:
        for svc in subs[u]:
            subs[u][svc].sort(key=lambda x: x["_start"])
    return subs


def tariff_at(by_plan: dict, plan_id: str, t: datetime) -> dict:
    for v in by_plan[plan_id]:
        if v["_start"] <= t < v["_end"]:
            return v
    raise KeyError("no tariff version of %s at %s" % (plan_id, t))


def sub_at(subs: dict, user: str, service: str, t: datetime):
    for s in subs.get(user, {}).get(service, []):
        if s["_start"] <= t < s["_end"]:
            return s
    return None


def month_start(dt: datetime) -> datetime:
    return datetime(dt.year, dt.month, 1, tzinfo=UTC)


def add_months(dt: datetime, n: int) -> datetime:
    m = dt.month - 1 + n
    return datetime(dt.year + m // 12, m % 12 + 1, 1, tzinfo=UTC)


def split_by_month(s: datetime, e: datetime) -> list[tuple[datetime, datetime]]:
    out, cur = [], s
    while cur < e:
        nxt = add_months(cur, 1)
        if nxt >= e:
            out.append((cur, e))
            break
        out.append((cur, nxt))
        cur = nxt
    return out


def split_boundaries(subs, by_plan, user, service, s, e) -> list[datetime]:
    pts = set()
    cur = add_months(s, 1)
    while cur < e:
        if s < cur:
            pts.add(cur)
        cur = add_months(cur, 1)
    for sub in subs.get(user, {}).get(service, []):
        for b in (sub["_start"], sub["_end"]):
            if s < b < e:
                pts.add(b)
    seen_plans = set()
    for sub in subs.get(user, {}).get(service, []):
        if sub["plan_id"] in seen_plans:
            continue
        seen_plans.add(sub["plan_id"])
        for v in by_plan.get(sub["plan_id"], []):
            for b in (v["_start"], v["_end"]):
                if s < b < e:
                    pts.add(b)
    return sorted(pts)


# ---------------------------------------------------------------- 接入解码

def list_cdr_files(inbox: str) -> list[str]:
    out = []
    for name in sorted(os.listdir(inbox)):
        if name.endswith((".fixed", ".delim", ".asn1", ".lpbin")):
            out.append(os.path.join(inbox, name))
    return out


def is_garbled_text(raw: str) -> bool:
    # 与 Go utf8.ValidString 对齐：严格校验 UTF-8 合法性与 NUL 字节。
    # 文件以 errors="replace" 读入，非法序列会变成 U+FFFD，这里通过
    # 重新严格编码检测（U+FFFD 本身合法，因此另检查原始字节由 decode 层完成，
    # 见 decode_text_line）。
    return "\x00" in raw


def decode_text_line(raw_bytes: bytes):
    """按 Go 引擎语义解码文本行：非法 UTF-8 直接判 E1。"""
    try:
        raw = raw_bytes.decode("utf-8")
    except UnicodeDecodeError:
        return None, True
    if "\x00" in raw:
        return raw, True
    return raw, False


def decode_all(data_dir: str) -> tuple[list[dict], dict]:
    """返回 (成功解码的话单, 异常计数)。与 Go ingest 的首错即停规则一致。"""
    inbox = os.path.join(data_dir, "inbox")
    records: list[dict] = []
    counts = defaultdict(int)
    for path in list_cdr_files(inbox):
        ext = os.path.splitext(path)[1]
        if ext in (".fixed", ".delim"):
            # 二进制读行，严格 UTF-8 解码（与 Go bufio + utf8.Valid 一致）
            with open(path, "rb") as fp:
                for raw_bytes in fp:
                    raw_bytes = raw_bytes.rstrip(b"\r\n")
                    if not raw_bytes:
                        continue
                    raw, garbled = decode_text_line(raw_bytes)
                    if garbled:
                        counts[E1] += 1
                        continue
                    try:
                        cdr = parse_fixed(raw) if ext == ".fixed" else parse_delimited(raw)
                        records.append(cdr)
                    except (ValueError, KeyError):
                        counts[E1] += 1
        elif ext == ".lpbin":
            with open(path, "rb") as fp:
                blob = fp.read()
            pos = 0
            while pos + 4 <= len(blob):
                n = struct.unpack(">I", blob[pos:pos + 4])[0]
                pos += 4
                payload = blob[pos:pos + n]
                pos += n
                try:
                    text = payload.decode("utf-8")
                    if is_garbled_text(text):
                        raise ValueError("garbled")
                    cdr = parse_fixed(text)
                    cdr["source_type"] = "lpbin"
                    records.append(cdr)
                except (ValueError, KeyError, UnicodeError):
                    counts[E1] += 1
        elif ext == ".asn1":
            with open(path, "rb") as fp:
                blob = fp.read()
            pos = 0
            while pos < len(blob):
                try:
                    cdr, pos = decode_cdr(blob, pos)
                    cdr["source_type"] = "asn1"  # DER 结构内无格式字段，按文件类型标记
                    records.append(cdr)
                except DERError:
                    counts[E1] += 1
                    break  # 与 Go 引擎一致：DER 错位后停止本文件解析
    return records, dict(counts)


# ---------------------------------------------------------------- 额度池与批价

def parse_window(w: list[str]):
    def hm(x: str):
        h, m = x.split(":")
        return int(h) * 60 + int(m)
    return hm(w[0]), hm(w[1])


def in_night(window, t: datetime) -> bool:
    start_min, end_min = window
    cur = t.astimezone(UTC).hour * 60 + t.astimezone(UTC).minute
    if start_min <= end_min:
        return start_min <= cur < end_min
    return cur >= start_min or cur < end_min


def chargeable_units(service: str, cfg: dict, qty: int) -> int:
    if service == "sms":
        return qty
    size = cfg["unit_size"]
    if size <= 1:
        return qty
    return (qty + size - 1) // size


def round_half_up(num: int, den: int) -> int:
    return (num + den // 2) // den


class Rater:
    """单用户单业务批价状态机，逐位对应 Go pipeline.Rater。"""

    def __init__(self, subs, by_plan, user: str, service: str):
        self.subs, self.by_plan = subs, by_plan
        self.user, self.service = user, service
        self.allowance: dict[tuple, int] = defaultdict(int)
        self.cursors: dict[tuple, int] = defaultdict(int)   # (plan, version) -> used
        self.windows: dict[tuple, tuple] = {}
        self._init_allowance()

    def _init_allowance(self) -> None:
        for sub in self.subs.get(self.user, {}).get(self.service, []):
            v = tariff_at(self.by_plan, sub["plan_id"], sub["_start"])
            total = v["services"][self.service].get("allowance_units", 0)
            if total <= 0:
                continue
            self._build_pools(sub, total)

    def _versions_between(self, plan_id: str, s: datetime, e: datetime) -> list[dict]:
        first = tariff_at(self.by_plan, plan_id, s)
        out = [v for v in self.by_plan[plan_id]
               if v["_start"] < e and v["_start"] >= first["_start"]]
        out.sort(key=lambda x: x["_start"])
        return out

    def _build_pools(self, sub: dict, total: int) -> None:
        months = split_by_month(sub["_start"], sub["_end"])
        sub_secs = int((sub["_end"] - sub["_start"]).total_seconds())
        month_allow, assigned = [], 0
        for i, (ms, me) in enumerate(months):
            secs = int((me - ms).total_seconds())
            if i == len(months) - 1:
                month_allow.append(total - assigned)
            else:
                part = total * secs // sub_secs
                month_allow.append(part)
                assigned += part
        for i, (ms, me) in enumerate(months):
            vers = self._versions_between(sub["plan_id"], ms, me)
            if not vers:
                continue
            m_total, m_secs, m_assigned = month_allow[i], int((me - ms).total_seconds()), 0
            for j, v in enumerate(vers):
                vs, ve = max(ms, v["_start"]), min(me, v["_end"])
                if not vs < ve:
                    continue
                key = (ms.strftime(MONTH_FMT), sub["plan_id"], v["version"])
                if j == len(vers) - 1:
                    self.allowance[key] += m_total - m_assigned
                else:
                    part = m_total * int((ve - vs).total_seconds()) // m_secs
                    self.allowance[key] += part
                    m_assigned += part

    def _night_window(self, cfg: dict):
        w = cfg.get("night_window")
        if not w:
            return None
        key = (w[0], w[1])
        if key not in self.windows:
            self.windows[key] = parse_window(w)
        return self.windows[key]

    def rate(self, cdr: dict) -> list[dict]:
        bounds = split_boundaries(self.subs, self.by_plan,
                                  self.user, self.service, cdr["start_time"], cdr["end_time"])
        edges = [cdr["start_time"]] + bounds + [cdr["end_time"]]
        works = [(edges[i], edges[i + 1]) for i in range(len(edges) - 1)
                 if edges[i] < edges[i + 1]]
        total_secs = int((cdr["end_time"] - cdr["start_time"]).total_seconds())
        remaining = cdr["quantity"]
        frags = []
        for i, (ws, we) in enumerate(works):
            sub = sub_at(self.subs, self.user, self.service, ws)
            if sub is None:
                raise KeyError(E4)
            v = tariff_at(self.by_plan, sub["plan_id"], ws)
            cfg = v["services"][self.service]
            if self.service == "sms":
                raw_qty = cdr["quantity"] if i == 0 else 0
            else:
                if i == len(works) - 1:
                    raw_qty = remaining
                else:
                    secs = int((we - ws).total_seconds())
                    raw_qty = cdr["quantity"] * secs // total_secs
                    remaining -= raw_qty
            if raw_qty <= 0:
                continue
            units = chargeable_units(self.service, cfg, raw_qty)
            if units <= 0:
                continue
            month = ws.astimezone(UTC).strftime(MONTH_FMT)
            key = (month, sub["plan_id"], v["version"])
            free = min(units, self.allowance[key])
            self.allowance[key] -= free
            bill_units = units - free
            nw = self._night_window(cfg)
            mid = ws + (we - ws) // 2
            night = bool(nw) and in_night(nw, mid)
            fee = self._charge(cfg, sub["plan_id"], v["version"], bill_units,
                               night, bool(cdr["roaming"]))
            frags.append({
                "bill_month": month, "plan_id": sub["plan_id"], "tariff_version": v["version"],
                "units": units, "free_units": free, "bill_units": bill_units,
                "night": night, "roaming": bool(cdr["roaming"]), "fee": fee,
            })
        return frags

    def _charge(self, cfg, plan_id, version, units: int, night: bool, roaming: bool) -> int:
        if units <= 0:
            return 0
        key = (plan_id, version)
        used_before = self.cursors[key]
        base, left = 0, units
        for tier in cfg["tiers"]:
            if left == 0:
                break
            if tier["upto"] < 0:
                cap = left
            else:
                if used_before >= tier["upto"]:
                    continue
                cap = min(tier["upto"] - used_before, left)
            base += cap * tier["rate"]
            used_before += cap
            left -= cap
        if left > 0:
            raise RuntimeError("tiers exhausted %s" % (key,))
        self.cursors[key] = used_before
        fee = base
        if night and cfg.get("night_factor", 0) > 0:
            fee = round_half_up(fee * cfg["night_factor"], BASIS)
        if roaming and cfg.get("roaming_factor", 0) > 0:
            fee = round_half_up(fee * cfg["roaming_factor"], BASIS)
        return fee


# ---------------------------------------------------------------- 主流程

def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--data", required=True)
    ap.add_argument("--shards", type=int, default=64)
    args = ap.parse_args()

    by_plan, _ = load_tariffs(os.path.join(args.data, "tariffs"))
    subs = load_subscriptions(os.path.join(args.data, "subscriptions.jsonl"))
    records, ingest_ex = decode_all(args.data)
    ex_counts = dict(ingest_ex)

    # 去重：record_id 保留首条（文件已按文件名排序，文件内保持生成顺序）
    seen, uniq = set(), []
    dup_count = 0
    for c in records:
        if c["record_id"] in seen:
            dup_count += 1
            continue
        seen.add(c["record_id"])
        uniq.append(c)

    # 拆段分组：键为 (user_id, call_ref)，防止引用号跨用户碰撞
    groups, standalone = defaultdict(list), []
    for c in uniq:
        (groups[(c["user_id"], c["call_ref"])] if c["call_ref"] else standalone).append(c)

    work = [{"cdr": c, "merged": False, "ids": []} for c in standalone]
    merge_count = 0
    for ref, segs in groups.items():
        segs.sort(key=lambda x: (x["seg_no"], x["record_id"]))
        seg_total = segs[0]["seg_total"]
        nos = [s["seg_no"] for s in segs]
        bad = (seg_total != len(segs)
               or any(s["seg_total"] != seg_total for s in segs)
               or len(set(nos)) != len(nos)
               or any(n < 1 or n > seg_total for n in nos)
               or set(nos) != set(range(1, seg_total + 1)))
        if bad:
            ex_counts[E5] = ex_counts.get(E5, 0) + len(segs)
            continue
        head = dict(segs[0])
        ids = []
        total_qty = 0
        for s in segs:
            ids.append(s["record_id"])
            total_qty += s["quantity"]
            head["start_time"] = min(head["start_time"], s["start_time"])
            head["end_time"] = max(head["end_time"], s["end_time"])
        head["quantity"] = total_qty
        work.append({"cdr": head, "merged": True, "ids": ids})
        merge_count += 1

    # 按 user|service 分组，确定性排序后过 E2/E3/E4 再批价
    buckets: dict[tuple, list[dict]] = defaultdict(list)
    for item in work:
        c = item["cdr"]
        buckets[(c["user_id"], c["service"])].append(item)

    # 用户×月 汇总
    summary: dict[tuple, dict] = defaultdict(
        lambda: {"service_fee": defaultdict(int), "service_units": defaultdict(int),
                 "total_fee": 0, "valid_count": 0})
    rated_total = 0
    sample_per_user: dict[str, int] = defaultdict(int)
    rated_samples = []

    for (user, service) in sorted(buckets):
        items = buckets[(user, service)]
        valid = []
        for item in items:
            c = item["cdr"]
            if c["end_time"] < c["start_time"] or (
                    c["end_time"] == c["start_time"] and service != "sms"):
                ex_counts[E2] = ex_counts.get(E2, 0) + 1
                continue
            if c["quantity"] <= 0:
                ex_counts[E3] = ex_counts.get(E3, 0) + 1
                continue
            if sub_at(subs, user, service, c["start_time"]) is None:
                ex_counts[E4] = ex_counts.get(E4, 0) + 1
                continue
            valid.append(item)
        valid.sort(key=lambda x: (x["cdr"]["start_time"], x["cdr"]["record_id"]))

        rater = Rater(subs, by_plan, user, service)
        for item in valid:
            c = item["cdr"]
            try:
                frags = rater.rate(c)
            except KeyError:
                ex_counts[E4] = ex_counts.get(E4, 0) + 1
                continue
            fee = sum(f["fee"] for f in frags)
            rated_total += 1
            months_seen = set()
            for f in frags:
                row = summary[(user, f["bill_month"])]
                row["service_fee"][service] += f["fee"]
                row["service_units"][service] += f["units"]
                row["total_fee"] += f["fee"]
                if f["bill_month"] not in months_seen:
                    row["valid_count"] += 1
                    months_seen.add(f["bill_month"])
            if sample_per_user[user] < 3:
                sample_per_user[user] += 1
                rated_samples.append({
                    "record_id": c["record_id"], "user_id": user, "service": service,
                    "merged": item["merged"], "fragments": frags, "total_fee": fee,
                })

    out_dir = os.path.join(args.data, "oracle")
    os.makedirs(out_dir, exist_ok=True)
    grand_total = 0
    month_totals: dict[str, int] = defaultdict(int)
    with open(os.path.join(out_dir, "user_summary.jsonl"), "w") as fp:
        for (user, month) in sorted(summary):
            row = summary[(user, month)]
            grand_total += row["total_fee"]
            month_totals[month] += row["total_fee"]
            fp.write(json.dumps({
                "user_id": user, "bill_month": month,
                "service_fee": dict(sorted(row["service_fee"].items())),
                "service_units": dict(sorted(row["service_units"].items())),
                "total_fee": row["total_fee"], "valid_count": row["valid_count"],
            }, ensure_ascii=False) + "\n")
    with open(os.path.join(out_dir, "rated_sample.jsonl"), "w") as fp:
        for s in rated_samples:
            fp.write(json.dumps(s, ensure_ascii=False) + "\n")
    with open(os.path.join(out_dir, "exception_counts.json"), "w") as fp:
        json.dump(dict(sorted(ex_counts.items())), fp, ensure_ascii=False, indent=2)
    manifest = {
        "decoded_records": len(records), "unique_records": len(uniq),
        "duplicates": dup_count, "merged_groups": merge_count,
        "rated_cdrs": rated_total, "exception_counts": dict(sorted(ex_counts.items())),
        "grand_total_fee": grand_total, "month_totals": dict(sorted(month_totals.items())),
        "user_month_rows": len(summary),
    }
    with open(os.path.join(out_dir, "oracle_manifest.json"), "w") as fp:
        json.dump(manifest, fp, ensure_ascii=False, indent=2)
    print("[oracle]", json.dumps(manifest, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
