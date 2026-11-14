package codec

import (
	"encoding/asn1"
	"fmt"
	"time"

	"ratingengine/internal/model"
)

// asn1CDR 与 Python 生成器的 DER SEQUENCE 严格对应（按字段顺序）：
// RecordID, UserID, Service, StartTime, EndTime, Quantity, Roaming,
// CallRef, SegNo, SegTotal。
type asn1CDR struct {
	RecordID  string
	UserID    string
	Service   string
	StartTime time.Time
	EndTime   time.Time
	Quantity  int64
	Roaming   bool
	CallRef   string
	SegNo     int
	SegTotal  int
}

// ParseASN1 解析一条 DER 编码话单。
func ParseASN1(data []byte) (*model.RawCDR, error) {
	var v asn1CDR
	rest, err := asn1.Unmarshal(data, &v)
	if err != nil {
		return nil, fmt.Errorf("asn1 unmarshal: %w", err)
	}
	if len(rest) != 0 {
		return nil, fmt.Errorf("asn1 trailing %d bytes", len(rest))
	}
	return &model.RawCDR{
		RecordID:   v.RecordID,
		SourceType: model.SourceASN1,
		UserID:     v.UserID,
		Service:    v.Service,
		StartTime:  v.StartTime.UTC(),
		EndTime:    v.EndTime.UTC(),
		Quantity:   v.Quantity,
		Roaming:    v.Roaming,
		CallRef:    v.CallRef,
		SegNo:      v.SegNo,
		SegTotal:   v.SegTotal,
	}, nil
}

// EncodeASN1 将话单编码为 DER。
func EncodeASN1(c *model.RawCDR) ([]byte, error) {
	v := asn1CDR{
		RecordID:  c.RecordID,
		UserID:    c.UserID,
		Service:   c.Service,
		StartTime: c.StartTime.UTC(),
		EndTime:   c.EndTime.UTC(),
		Quantity:  c.Quantity,
		Roaming:   c.Roaming,
		CallRef:   c.CallRef,
		SegNo:     c.SegNo,
		SegTotal:  c.SegTotal,
	}
	data, err := asn1.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("asn1 marshal: %w", err)
	}
	return data, nil
}

// ASN1SeqLen 返回 buf 中第一条 DER TLV 的总字节数（tag+length+value）。
// 仅解析外层长度，不校验内容。
func ASN1SeqLen(buf []byte) (int, error) {
	if len(buf) < 2 {
		return 0, fmt.Errorf("asn1: buffer too short")
	}
	// tag（本项目均为单字节 tag）
	i := 1
	l := int(buf[i])
	i++
	if l&0x80 == 0 {
		// 短形式长度
		total := i + l
		if total > len(buf) {
			return 0, fmt.Errorf("asn1: truncated value")
		}
		return total, nil
	}
	// 长形式：低 7 位为长度字节数
	n := l & 0x7f
	if n == 0 || n > 4 || i+n > len(buf) {
		return 0, fmt.Errorf("asn1: bad long-form length")
	}
	vlen := 0
	for k := 0; k < n; k++ {
		vlen = vlen<<8 | int(buf[i+k])
	}
	total := i + n + vlen
	if total > len(buf) {
		return 0, fmt.Errorf("asn1: truncated value")
	}
	return total, nil
}
