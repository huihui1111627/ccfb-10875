"""独立的 DER (ITU-T X.690) 编解码，不依赖任何第三方 ASN.1 库。

话单 DER 结构（与 Go encoding/asn1 的 asn1CDR 字段顺序严格对应）：
  SEQUENCE {
    record_id  UTF8String,      -- tag 0x0C
    user_id    UTF8String,
    service    UTF8String,
    start_time UTCTime,         -- tag 0x17, "YYMMDDHHMMSSZ"
    end_time   UTCTime,
    quantity   INTEGER,         -- tag 0x02
    roaming    BOOLEAN,         -- tag 0x01, FF/00
    call_ref   UTF8String,
    seg_no     INTEGER,
    seg_total  INTEGER
  }
"""
from __future__ import annotations

from datetime import datetime, timezone

TAG_BOOLEAN = 0x01
TAG_INTEGER = 0x02
TAG_UTCTIME = 0x17
TAG_UTF8 = 0x0C
TAG_SEQUENCE = 0x30


def _encode_length(n: int) -> bytes:
    if n < 0x80:
        return bytes([n])
    body = n.to_bytes((n.bit_length() + 7) // 8, "big")
    return bytes([0x80 | len(body)]) + body


def _tlv(tag: int, content: bytes) -> bytes:
    return bytes([tag]) + _encode_length(len(content)) + content


def encode_integer(value: int) -> bytes:
    if value == 0:
        body = b"\x00"
    else:
        body = value.to_bytes((value.bit_length() + 8) // 8, "big", signed=True)
    return _tlv(TAG_INTEGER, body)


def encode_boolean(flag: bool) -> bytes:
    return _tlv(TAG_BOOLEAN, b"\xff" if flag else b"\x00")


def encode_utf8(text: str) -> bytes:
    return _tlv(TAG_UTF8, text.encode("utf-8"))


def encode_utctime(dt: datetime) -> bytes:
    dt = dt.astimezone(timezone.utc)
    text = dt.strftime("%y%m%d%H%M%SZ")
    return _tlv(TAG_UTCTIME, text.encode("ascii"))


def encode_cdr(cdr: dict) -> bytes:
    """把规范化话单 dict 编码为一条 DER SEQUENCE。"""
    parts = [
        encode_utf8(cdr["record_id"]),
        encode_utf8(cdr["user_id"]),
        encode_utf8(cdr["service"]),
        encode_utctime(cdr["start_time"]),
        encode_utctime(cdr["end_time"]),
        encode_integer(int(cdr["quantity"])),
        encode_boolean(bool(cdr["roaming"])),
        encode_utf8(cdr.get("call_ref", "")),
        encode_integer(int(cdr["seg_no"])),
        encode_integer(int(cdr["seg_total"])),
    ]
    return _tlv(TAG_SEQUENCE, b"".join(parts))


class DERError(ValueError):
    pass


def _read_length(buf: bytes, pos: int) -> tuple[int, int]:
    if pos >= len(buf):
        raise DERError("truncated length")
    first = buf[pos]
    pos += 1
    if first < 0x80:
        return first, pos
    n = first & 0x7F
    if n == 0 or n > 4 or pos + n > len(buf):
        raise DERError("bad long-form length")
    length = int.from_bytes(buf[pos:pos + n], "big")
    return length, pos + n


def _read_tlv(buf: bytes, pos: int) -> tuple[int, bytes, int]:
    if pos >= len(buf):
        raise DERError("truncated tag")
    tag = buf[pos]
    length, pos = _read_length(buf, pos + 1)
    if pos + length > len(buf):
        raise DERError("truncated value")
    return tag, buf[pos:pos + length], pos + length


def decode_integer(content: bytes) -> int:
    return int.from_bytes(content, "big", signed=True)


def decode_utctime(content: bytes) -> datetime:
    text = content.decode("ascii")
    if not text.endswith("Z") or len(text) != 13:
        raise DERError("unsupported UTCTime format: %r" % text)
    dt = datetime.strptime(text, "%y%m%d%H%M%SZ")
    return dt.replace(tzinfo=timezone.utc)


def decode_cdr(buf: bytes, pos: int = 0) -> tuple[dict, int]:
    """解码一条 DER SEQUENCE 话单，返回 (cdr_dict, 下一位置)。"""
    tag, content, next_pos = _read_tlv(buf, pos)
    if tag != TAG_SEQUENCE:
        raise DERError("expected SEQUENCE, got tag 0x%02x" % tag)
    fields = []
    p = 0
    expected = [TAG_UTF8, TAG_UTF8, TAG_UTF8, TAG_UTCTIME, TAG_UTCTIME,
                TAG_INTEGER, TAG_BOOLEAN, TAG_UTF8, TAG_INTEGER, TAG_INTEGER]
    for want in expected:
        t, v, p = _read_tlv(content, p)
        if t != want:
            raise DERError("expected tag 0x%02x, got 0x%02x" % (want, t))
        fields.append(v)
    if p != len(content):
        raise DERError("trailing bytes inside SEQUENCE")
    return {
        "record_id": fields[0].decode("utf-8"),
        "user_id": fields[1].decode("utf-8"),
        "service": fields[2].decode("utf-8"),
        "start_time": decode_utctime(fields[3]),
        "end_time": decode_utctime(fields[4]),
        "quantity": decode_integer(fields[5]),
        "roaming": fields[6] == b"\xff",
        "call_ref": fields[7].decode("utf-8"),
        "seg_no": decode_integer(fields[8]),
        "seg_total": decode_integer(fields[9]),
    }, next_pos
