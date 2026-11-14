"""Python 侧域模型与文本话单编解码（与 docs/SPEC.md §2 对齐，独立于 Go 实现）。"""
from __future__ import annotations

from datetime import datetime, timezone

TIME_FMT = "%Y-%m-%dT%H:%M:%SZ"
FIXED_WIDTH = 120
DELIM = "|"
SERVICES = ("voice", "sms", "data")
SOURCES = ("fixed", "delimited", "asn1", "lpbin")
SOURCE_CODE = {"fixed": "00", "delimited": "01", "asn1": "02", "lpbin": "03"}
CODE_SOURCE = {v: k for k, v in SOURCE_CODE.items()}

# 异常原因
E1 = "E1_BAD_FORMAT"
E2 = "E2_BAD_TIME"
E3 = "E3_BAD_QUANTITY"
E4 = "E4_NO_SUBSCRIPTION"
E5 = "E5_BAD_SEGMENT"

_FIXED_OFFSETS = [(0, 20), (20, 32), (32, 38), (38, 58), (58, 78),
                  (78, 92), (92, 94), (94, 110), (110, 114), (114, 118), (118, 120)]
_FIELD_ORDER = ("record_id", "user_id", "service", "start_time", "end_time",
                "quantity", "roaming", "call_ref", "seg_no", "seg_total", "source_type")


def parse_time(s: str) -> datetime:
    return datetime.strptime(s, TIME_FMT).replace(tzinfo=timezone.utc)


def fmt_time(dt: datetime) -> str:
    return dt.astimezone(timezone.utc).strftime(TIME_FMT)


def _pad(s: str, width: int, right: bool = False) -> str:
    s = str(s)
    if len(s) >= width:
        return s[:width]
    return (" " * (width - len(s)) + s) if right else (s + " " * (width - len(s)))


def cdr_to_fields(cdr: dict) -> list[str]:
    return [
        cdr["record_id"], cdr["user_id"], cdr["service"],
        fmt_time(cdr["start_time"]), fmt_time(cdr["end_time"]),
        str(int(cdr["quantity"])), "1" if cdr["roaming"] else "0",
        cdr.get("call_ref", ""), str(int(cdr.get("seg_no", 0))),
        str(int(cdr.get("seg_total", 0))),
        SOURCE_CODE[cdr.get("source_type", "fixed")],
    ]


def encode_fixed(cdr: dict) -> str:
    f = cdr_to_fields(cdr)
    out = []
    for i, (a, b) in enumerate(_FIXED_OFFSETS):
        width = b - a
        out.append(_pad(f[i], width, right=(i == 5 or i in (8, 9))))
    line = "".join(out)
    assert len(line) == FIXED_WIDTH, len(line)
    return line


def parse_fixed(line: str) -> dict:
    if len(line) != FIXED_WIDTH:
        raise ValueError("fixed width %d != %d" % (len(line), FIXED_WIDTH))
    fields = [line[a:b].strip() for a, b in _FIXED_OFFSETS]
    return _assemble(fields, "fixed")


def encode_delimited(cdr: dict) -> str:
    return DELIM.join(cdr_to_fields({**cdr, "source_type": "delimited"}))


def parse_delimited(line: str) -> dict:
    fields = line.split(DELIM)
    if len(fields) != 11:
        raise ValueError("delimited field count %d != 11" % len(fields))
    return _assemble(fields, "delimited")


def _assemble(f: list[str], default_source: str) -> dict:
    if not f[0] or not f[1]:
        raise ValueError("empty record_id/user_id")
    roaming = f[6]
    if roaming not in ("0", "1"):
        raise ValueError("bad roaming flag %r" % roaming)
    src_type = CODE_SOURCE.get(f[10])
    if src_type is None:
        raise ValueError("unknown source type code %r" % f[10])
    return {
        "record_id": f[0],
        "user_id": f[1],
        "service": f[2],
        "start_time": parse_time(f[3]),
        "end_time": parse_time(f[4]),
        "quantity": int(f[5]),
        "roaming": roaming == "1",
        "call_ref": f[7],
        "seg_no": int(f[8]),
        "seg_total": int(f[9]),
        "source_type": src_type,
    }


def fnv32a(text: str) -> int:
    """标准 FNV-1a 32 位，与 Go hash/fnv New32a 一致。"""
    h = 0x811C9DC5
    for b in text.encode("utf-8"):
        h ^= b
        h = (h * 0x01000193) & 0xFFFFFFFF
    return h


def shard_of(user_id: str, shard_count: int) -> int:
    return fnv32a(user_id) % shard_count


def bill_month(dt: datetime) -> str:
    u = dt.astimezone(timezone.utc)
    return "%04d-%02d" % (u.year, u.month)
