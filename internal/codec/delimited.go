package codec

import (
	"fmt"
	"strconv"
	"strings"

	"ratingengine/internal/model"
)

// Delim 分隔符文本的字段分隔符。
const Delim = "|"

// ParseDelimited 解析一条分隔符话单。
func ParseDelimited(line string) (*model.RawCDR, error) {
	fields := strings.Split(line, Delim)
	if len(fields) != 11 {
		return nil, fmt.Errorf("delimited field count %d != 11", len(fields))
	}
	return assemble(fields, model.SourceDelimited)
}

// EncodeDelimited 编码分隔符话单。
func EncodeDelimited(c *model.RawCDR) string {
	roam := "0"
	if c.Roaming {
		roam = "1"
	}
	return strings.Join([]string{
		c.RecordID,
		c.UserID,
		c.Service,
		c.StartTime.UTC().Format(model.TimeLayout),
		c.EndTime.UTC().Format(model.TimeLayout),
		strconv.FormatInt(c.Quantity, 10),
		roam,
		c.CallRef,
		strconv.Itoa(c.SegNo),
		strconv.Itoa(c.SegTotal),
		sourceCode[c.SourceType],
	}, Delim)
}
