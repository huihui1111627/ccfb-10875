package codec

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"ratingengine/internal/model"
)

// 定长文本字段偏移（见 docs/SPEC.md §2.1），总长 120。
const FixedLineWidth = 120

var fixedOffsets = [][2]int{
	{0, 20},    // record_id
	{20, 32},   // user_id
	{32, 38},   // service
	{38, 58},   // start
	{58, 78},   // end
	{78, 92},   // quantity
	{92, 94},   // roaming
	{94, 110},  // call_ref
	{110, 114}, // seg_no
	{114, 118}, // seg_total
	{118, 120}, // type
}

// ParseFixed 解析一条定长文本话单。
func ParseFixed(line string) (*model.RawCDR, error) {
	if len(line) != FixedLineWidth {
		return nil, fmt.Errorf("fixed line width %d != %d", len(line), FixedLineWidth)
	}
	fields := make([]string, 11)
	for i, off := range fixedOffsets {
		fields[i] = strings.TrimSpace(line[off[0]:off[1]])
	}
	return assemble(fields, model.SourceFixed)
}

// sourceCode 格式名 <-> 2 位数字格式码（SPEC §2.1）。
var sourceCode = map[string]string{
	model.SourceFixed: "00", model.SourceDelimited: "01",
	model.SourceASN1: "02", model.SourceLPBin: "03",
}
var codeSource = map[string]string{
	"00": model.SourceFixed, "01": model.SourceDelimited,
	"02": model.SourceASN1, "03": model.SourceLPBin,
}

// EncodeFixed 将话单编码为 120 字符定长行。
func EncodeFixed(c *model.RawCDR) string {
	var b strings.Builder
	b.WriteString(pad(c.RecordID, 20, false))
	b.WriteString(pad(c.UserID, 12, false))
	b.WriteString(pad(c.Service, 6, false))
	b.WriteString(pad(c.StartTime.UTC().Format(model.TimeLayout), 20, false))
	b.WriteString(pad(c.EndTime.UTC().Format(model.TimeLayout), 20, false))
	b.WriteString(pad(strconv.FormatInt(c.Quantity, 10), 14, true))
	roam := "0"
	if c.Roaming {
		roam = "1"
	}
	b.WriteString(pad(roam, 2, false))
	b.WriteString(pad(c.CallRef, 16, false))
	b.WriteString(pad(strconv.Itoa(c.SegNo), 4, true))
	b.WriteString(pad(strconv.Itoa(c.SegTotal), 4, true))
	code := sourceCode[c.SourceType]
	if code == "" {
		code = sourceCode[model.SourceFixed]
	}
	b.WriteString(code)
	return b.String()
}

func pad(s string, width int, rightAlign bool) string {
	if len(s) >= width {
		return s[:width]
	}
	gap := width - len(s)
	if rightAlign {
		return strings.Repeat(" ", gap) + s
	}
	return s + strings.Repeat(" ", gap)
}

// assemble 字段顺序：record_id,user_id,service,start,end,quantity,roaming,
// call_ref,seg_no,seg_total,type
func assemble(f []string, defaultType string) (*model.RawCDR, error) {
	_ = defaultType
	if len(f) != 11 {
		return nil, fmt.Errorf("field count %d != 11", len(f))
	}
	if f[0] == "" || f[1] == "" {
		return nil, errors.New("empty record_id or user_id")
	}
	st, err := time.Parse(model.TimeLayout, f[3])
	if err != nil {
		return nil, fmt.Errorf("bad start_time %q: %w", f[3], err)
	}
	et, err := time.Parse(model.TimeLayout, f[4])
	if err != nil {
		return nil, fmt.Errorf("bad end_time %q: %w", f[4], err)
	}
	qty, err := strconv.ParseInt(f[5], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("bad quantity %q: %w", f[5], err)
	}
	if f[6] != "0" && f[6] != "1" {
		return nil, fmt.Errorf("bad roaming flag %q", f[6])
	}
	segNo, err := strconv.Atoi(f[8])
	if err != nil {
		return nil, fmt.Errorf("bad seg_no %q: %w", f[8], err)
	}
	segTotal, err := strconv.Atoi(f[9])
	if err != nil {
		return nil, fmt.Errorf("bad seg_total %q: %w", f[9], err)
	}
	srcType := codeSource[f[10]]
	if srcType == "" {
		return nil, fmt.Errorf("unknown source type code %q", f[10])
	}
	return &model.RawCDR{
		RecordID:   f[0],
		SourceType: srcType,
		UserID:     f[1],
		Service:    f[2],
		StartTime:  st.UTC(),
		EndTime:    et.UTC(),
		Quantity:   qty,
		Roaming:    f[6] == "1",
		CallRef:    f[7],
		SegNo:      segNo,
		SegTotal:   segTotal,
	}, nil
}
