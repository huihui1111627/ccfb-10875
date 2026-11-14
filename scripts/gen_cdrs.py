"""生成海量话单（默认一千万条起），覆盖四种格式与全部异常注入。

用法:
  python3 -I gen_cdrs.py --data DATA_DIR --count 10000000 --shards 64 --seed 20261001

输出到 DATA_DIR/inbox/:
  cdr-001.fixed   定长文本
  cdr-002.delim   分隔符文本
  cdr-003.asn1    ASN.1 DER（多条 SEQUENCE 串联）
  cdr-004.lpbin   4 字节大端长度前缀 + 定长负载
  gen_manifest.json

注入比例（占总量）:
  重复 1.5% / 乱码 0.5% / 坏时间 0.2% / 坏用量 0.2% / 无订阅 0.1% / 坏拆段 0.2%
  拆段通话约 3%（正常合并）；跨月/跨套餐边界话单约 1%
"""
from __future__ import annotations

import argparse
import json
import os
import random
import struct
import sys
from datetime import datetime, timedelta, timezone

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from domain import (FIXED_WIDTH, bill_month, cdr_to_fields, encode_delimited,
                    encode_fixed, fmt_time, parse_time)
from der import encode_cdr

UTC = timezone.utc
WIN_S = datetime(2026, 10, 1, tzinfo=UTC)
WIN_E = datetime(2026, 12, 1, tzinfo=UTC)
MID_OCT = datetime(2026, 10, 15, tzinfo=UTC)
NOV1 = datetime(2026, 11, 1, tzinfo=UTC)
BOUNDARIES = [MID_OCT, NOV1]


def record_id(seq: int, rng: random.Random) -> str:
    # REC + 10 位序号 + - + 6hex，恰好 20 字符
    return "REC%010d-%06x" % (seq, rng.getrandbits(24))


def load_subs(path: str) -> dict:
    """user -> service -> [(start, end, plan), ...]"""
    subs: dict = {}
    with open(path) as fp:
        for line in fp:
            s = json.loads(line)
            subs.setdefault(s["user_id"], {}).setdefault(s["service"], []).append((
                parse_time(s["effective_start"]), parse_time(s["effective_end"]), s["plan_id"]))
    for u in subs:
        for svc in subs[u]:
            subs[u][svc].sort()
    return subs


def pick_start(rng: random.Random) -> datetime:
    """窗口内随机时刻，留足结束余量。"""
    secs = int((WIN_E - WIN_S).total_seconds()) - 7200
    return WIN_S + timedelta(seconds=rng.randrange(secs))


def gen_quantity(rng: random.Random, service: str) -> int:
    if service == "voice":
        return rng.choice([rng.randint(5, 59), rng.randint(60, 600),
                           rng.randint(600, 3600), rng.randint(3600, 7200)])
    if service == "sms":
        return 1
    # data：字节，覆盖不足 1MB 与多 MB
    return rng.choice([rng.randint(1, 1048575), rng.randint(1048576, 20 * 1048576),
                       rng.randint(20 * 1048576, 200 * 1048576)])


def random_cdr(rng: random.Random, seq: int, users: list[str], subs: dict,
               force_service: str | None = None, force_start: datetime | None = None) -> dict:
    user = rng.choice(users)
    service = force_service or rng.choices(("voice", "sms", "data"), weights=(4, 3, 3))[0]
    start = force_start or pick_start(rng)
    qty = gen_quantity(rng, service)
    if service == "voice":
        end = start + timedelta(seconds=qty)
    elif service == "sms":
        end = start + timedelta(seconds=1)
    else:
        end = start + timedelta(seconds=rng.randint(1, 120))
    return {
        "record_id": record_id(seq, rng), "user_id": user, "service": service,
        "start_time": start, "end_time": end, "quantity": qty,
        "roaming": rng.random() < 0.18, "call_ref": "", "seg_no": 0, "seg_total": 0,
    }


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--data", required=True)
    ap.add_argument("--count", type=int, default=10_000_000)
    ap.add_argument("--shards", type=int, default=64)
    ap.add_argument("--seed", type=int, default=20261001)
    args = ap.parse_args()

    rng = random.Random(args.seed)
    inbox = os.path.join(args.data, "inbox")
    os.makedirs(inbox, exist_ok=True)
    with open(os.path.join(args.data, "config_manifest.json")) as fp:
        manifest = json.load(fp)
    n_users = manifest["users"]
    users = ["166%08d" % i for i in range(n_users)]
    subs = load_subs(os.path.join(args.data, "subscriptions.jsonl"))

    n = args.count
    # 注入配额
    q_dup = int(n * 0.015)
    q_garble = int(n * 0.005)
    q_badtime = int(n * 0.002)
    q_badqty = int(n * 0.002)
    q_nosub = int(n * 0.001)
    q_badseg_groups = int(n * 0.002)       # 每组 2~3 段
    q_split_groups = int(n * 0.03)         # 正常拆段组
    q_special = q_garble + q_badtime + q_badqty + q_nosub
    n_special_records = q_special + q_badseg_groups * 2 + q_split_groups * 2
    n_valid_base = n - q_dup - n_special_records

    seq = 0

    def next_id() -> str:
        nonlocal seq
        seq += 1
        return record_id(seq, rng)

    valid: list[dict] = []
    garbled: list[dict] = []
    badtime: list[dict] = []
    badqty: list[dict] = []
    nosub: list[dict] = []

    # 1) 正常话单（含跨边界话单）
    cross_n = int(n_valid_base * 0.01)
    for i in range(n_valid_base):
        if i < cross_n:
            # 起点在边界前 5~90 分钟，时长跨过边界（voice/data）
            b = rng.choice(BOUNDARIES)
            start = b - timedelta(minutes=rng.randint(5, 90))
            c = random_cdr(rng, 0, users, subs,
                           force_service=rng.choice(("voice", "data")), force_start=start)
            if c["service"] == "voice":
                c["quantity"] = rng.randint(600, 7200)
                c["end_time"] = c["start_time"] + timedelta(seconds=c["quantity"])
        else:
            c = random_cdr(rng, 0, users, subs)
        c["record_id"] = next_id()
        valid.append(c)

    # 2) 正常拆段通话（E5 校验通过、需合并）
    for g in range(q_split_groups):
        total_seg = rng.choice((2, 2, 3))
        user = rng.choice(users)
        start = pick_start(rng)
        ref = "CR%014d" % (g + 1)  # 全局唯一，杜绝跨用户碰撞（16 字符列宽内）
        total_secs = rng.randint(120, 3600)
        cuts = sorted(rng.sample(range(1, total_secs), total_seg - 1))
        edges = [0] + cuts + [total_secs]
        for k in range(total_seg):
            qty = edges[k + 1] - edges[k]
            seg = {
                "record_id": next_id(), "user_id": user, "service": "voice",
                "start_time": start + timedelta(seconds=edges[k]),
                "end_time": start + timedelta(seconds=edges[k + 1]),
                "quantity": qty, "roaming": rng.random() < 0.18,
                "call_ref": ref, "seg_no": k + 1, "seg_total": total_seg,
            }
            valid.append(seg)

    # 3) 坏拆段组（缺段 / 重复段号），引用号与正常组区间不重叠，保证全局唯一
    for g in range(q_badseg_groups):
        total_seg = rng.choice((2, 3))
        user = rng.choice(users)
        start = pick_start(rng)
        ref = "CR%014d" % (q_split_groups + g + 1)
        if rng.random() < 0.5:
            # 缺最后一段：只写 total_seg-1 条但 seg_total=total_seg
            nos = list(range(1, total_seg))
        else:
            # 重复段号
            nos = [1] + [1] + list(range(3, total_seg + 1))
            nos = nos[:total_seg]
        for k, no in enumerate(nos):
            seg = {
                "record_id": next_id(), "user_id": user, "service": "voice",
                "start_time": start + timedelta(seconds=k * 60),
                "end_time": start + timedelta(seconds=(k + 1) * 60),
                "quantity": 60, "roaming": False,
                "call_ref": ref, "seg_no": no, "seg_total": total_seg,
            }
            badtime.append(seg)  # 复用暂存桶，写出时按 E5 语义处理（见下）

    # 4) 坏时间（end < start）
    for _ in range(q_badtime):
        c = random_cdr(rng, 0, users, subs)
        c["record_id"] = next_id()
        c["start_time"], c["end_time"] = c["end_time"], c["start_time"]
        badtime.append(c)

    # 5) 坏用量
    for _ in range(q_badqty):
        c = random_cdr(rng, 0, users, subs)
        c["record_id"] = next_id()
        c["quantity"] = -rng.randint(1, 100)
        badqty.append(c)

    # 6) 无订阅：使用不存在的用户号
    for _ in range(q_nosub):
        c = random_cdr(rng, 0, users, subs)
        c["record_id"] = next_id()
        c["user_id"] = "166%08d" % (n_users + rng.randint(1, 9999))
        nosub.append(c)

    # 7) 乱码行（不构造完整 cdr，直接生成原始字节载荷）
    for _ in range(q_garble):
        kind = rng.choice(("nul", "badutf8", "short", "delim"))
        garbled.append({"_garble": kind})

    # 8) 重复：从 valid 中随机整对象复制（含拆段段记录）
    dupes = [dict(rng.choice(valid)) for _ in range(q_dup)]

    # 标记来源类型并按格式分桶（valid/dupes 去 fixed/delim/asn1/lpbin；异常桶各自对应格式）
    buckets = {"fixed": [], "delimited": [], "asn1": [], "lpbin": []}
    specials = {"fixed": [], "delimited": [], "asn1": [], "lpbin": []}

    def assign(rec: dict, target: dict) -> None:
        fmt = rng.choices(("fixed", "delimited", "asn1", "lpbin"),
                          weights=(35, 35, 20, 10))[0]
        target[fmt].append(rec)

    for c in valid:
        assign(c, buckets)
    for c in dupes:
        assign(c, buckets)
    # 异常记录（坏拆段与坏时间共用桶，写出阶段由字段特征区分；这里统一打标）
    for c in badtime:
        if "call_ref" in c and c["call_ref"] and c["seg_total"] > 0 and c["seg_no"] != 0:
            c["_err"] = "E5"
        else:
            c["_err"] = "E2"
        assign(c, specials)
    for c in badqty:
        c["_err"] = "E3"
        assign(c, specials)
    for c in nosub:
        c["_err"] = "E4"
        assign(c, specials)
    for g in garbled:
        assign(g, specials)

    # 各桶内部打乱（重复行与其原行可能落到不同文件，仍须被全局去重）
    for b in buckets.values():
        rng.shuffle(b)
    for b in specials.values():
        rng.shuffle(b)

    counts = {"valid_unique": len(valid), "duplicates": len(dupes),
              "garbled": q_garble, "bad_time": q_badtime, "bad_quantity": q_badqty,
              "no_subscription": q_nosub, "bad_segment_records": q_badseg_groups * 2,
              "split_groups": q_split_groups, "cross_boundary": cross_n}

    write_fixed(os.path.join(inbox, "cdr-001.fixed"), buckets["fixed"], specials["fixed"], rng)
    write_delim(os.path.join(inbox, "cdr-002.delim"), buckets["delimited"], specials["delimited"], rng)
    write_asn1(os.path.join(inbox, "cdr-003.asn1"), buckets["asn1"], specials["asn1"], rng)
    write_lpbin(os.path.join(inbox, "cdr-004.lpbin"), buckets["lpbin"], specials["lpbin"], rng)

    manifest_out = {
        "generator": "gen_cdrs.py", "seed": args.seed, "requested_count": n,
        "shards": args.shards, "users": n_users,
        "bill_months": sorted({bill_month(WIN_S), bill_month(NOV1)}),
        "counts": counts,
        "files": ["cdr-001.fixed", "cdr-002.delim", "cdr-003.asn1", "cdr-004.lpbin"],
    }
    with open(os.path.join(inbox, "gen_manifest.json"), "w") as fp:
        json.dump(manifest_out, fp, ensure_ascii=False, indent=2)
    total_written = sum(len(v) for v in buckets.values()) + sum(len(v) for v in specials.values())
    print(f"[gen_cdrs] requested={n} written_records={total_written}")
    print(json.dumps(counts, indent=2))


# ---------------- 各格式写出 ----------------

def _garble_payload(kind: str, rng: random.Random) -> bytes:
    if kind == "nul":
        line = encode_fixed({
            "record_id": "REC%010d-%06x" % (0, rng.getrandbits(24)),
            "user_id": "16600000000", "service": "voice",
            "start_time": WIN_S, "end_time": WIN_S + timedelta(seconds=60),
            "quantity": 60, "roaming": False, "call_ref": "", "seg_no": 0, "seg_total": 0,
        })
        pos = rng.randrange(20, 100)
        return (line[:pos] + "\x00\x00" + line[pos + 2:]).encode("utf-8", "replace")
    if kind == "badutf8":
        return bytes([0x52] * 40 + [0xFF, 0xFE, 0x80] + [0x45] * 77)
    if kind == "short":
        return b"REC_SHORT_BAD_LINE\n"
    # delim：字段不足
    return b"REC0000000000-abcdef|16600000001|voice\n"


def write_fixed(path: str, good: list[dict], bad: list[dict], rng: random.Random) -> None:
    with open(path, "w", encoding="utf-8", newline="\n") as fp:
        for c in good:
            fp.write(encode_fixed({**c, "source_type": "fixed"}) + "\n")
        for c in bad:
            if "_garble" in c:
                fp.write(_garble_payload(c["_garble"], rng).decode("utf-8", "replace") + "\n")
            else:
                fp.write(encode_fixed({**c, "source_type": "fixed"}) + "\n")


def write_delim(path: str, good: list[dict], bad: list[dict], rng: random.Random) -> None:
    with open(path, "w", encoding="utf-8", newline="\n") as fp:
        for c in good:
            fp.write(encode_delimited(c) + "\n")
        for c in bad:
            if "_garble" in c:
                fp.write(_garble_payload(c["_garble"], rng).decode("utf-8", "replace") + "\n")
            else:
                fp.write(encode_delimited(c) + "\n")


def write_asn1(path: str, good: list[dict], bad: list[dict], rng: random.Random) -> None:
    # DER 流无法在坏记录后重新同步，乱码记录一律排到文件末尾：
    # 可解码的异常记录（E2/E3/E4/E5，本身是合法 DER）在前，乱码在后，
    # 保证 Go 引擎与 Python oracle 在"首错即停"规则下消费集合一致。
    decodable_bad = [c for c in bad if "_garble" not in c]
    garble_bad = [c for c in bad if "_garble" in c]
    with open(path, "wb") as fp:
        for c in good:
            fp.write(encode_cdr(c))
        for c in decodable_bad:
            fp.write(encode_cdr(c))
        for c in garble_bad:
            payload = _garble_payload(c["_garble"], rng)
            if c["_garble"] == "short":
                fp.write(b"\x30\x05" + payload[:5])  # 声明长度 5 的残缺 SEQUENCE
            else:
                # 外层 SEQUENCE 长度故意与内容不符
                fp.write(b"\x30\x7f" + payload[:40])


def write_lpbin(path: str, good: list[dict], bad: list[dict], rng: random.Random) -> None:
    with open(path, "wb") as fp:
        def emit(payload: bytes) -> None:
            fp.write(struct.pack(">I", len(payload)))
            fp.write(payload)
        for c in good:
            emit(encode_fixed({**c, "source_type": "fixed"}).encode("utf-8"))
        for c in bad:
            if "_garble" in c:
                payload = _garble_payload(c["_garble"], rng)
                if len(payload) < FIXED_WIDTH:
                    emit(payload)  # 长度不足，定长解析失败
                else:
                    emit(payload[:FIXED_WIDTH])
            else:
                emit(encode_fixed({**c, "source_type": "fixed"}).encode("utf-8"))


if __name__ == "__main__":
    main()
