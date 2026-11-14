package codec

import (
	"encoding/binary"
	"fmt"

	"ratingengine/internal/model"
)

// LPBin 记录格式：4 字节大端长度 + 定长文本负载（不含换行）。

// ParseLPBin 解析一条长度前缀记录的负载部分。
func ParseLPBin(payload []byte) (*model.RawCDR, error) {
	c, err := ParseFixed(string(payload))
	if err != nil {
		return nil, err
	}
	c.SourceType = model.SourceLPBin
	return c, nil
}

// EncodeLPBin 编码为 4 字节大端长度 + 定长负载。
func EncodeLPBin(c *model.RawCDR) ([]byte, error) {
	line := EncodeFixed(c)
	payload := []byte(line)
	if len(payload) != FixedLineWidth {
		return nil, fmt.Errorf("lpbin payload width %d != %d", len(payload), FixedLineWidth)
	}
	out := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(out, uint32(len(payload)))
	copy(out[4:], payload)
	return out, nil
}
