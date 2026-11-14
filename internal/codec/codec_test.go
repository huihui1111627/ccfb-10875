package codec

import (
	"testing"
	"time"

	"ratingengine/internal/model"
)

func sampleCDR() *model.RawCDR {
	st, _ := time.Parse(model.TimeLayout, "2026-10-05T23:10:00Z")
	et, _ := time.Parse(model.TimeLayout, "2026-10-05T23:15:30Z")
	return &model.RawCDR{
		RecordID:   "REC000000001-ab12cd3",
		SourceType: model.SourceFixed,
		UserID:     "16600001234",
		Service:    model.ServiceVoice,
		StartTime:  st,
		EndTime:    et,
		Quantity:   330,
		Roaming:    true,
		CallRef:    "CR9001",
		SegNo:      1,
		SegTotal:   2,
	}
}

func TestFixedRoundTrip(t *testing.T) {
	c := sampleCDR()
	line := EncodeFixed(c)
	if len(line) != FixedLineWidth {
		t.Fatalf("width = %d, want %d", len(line), FixedLineWidth)
	}
	got, err := ParseFixed(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.RecordID != c.RecordID || got.UserID != c.UserID || got.Quantity != c.Quantity ||
		!got.StartTime.Equal(c.StartTime) || !got.EndTime.Equal(c.EndTime) ||
		got.Roaming != c.Roaming || got.CallRef != c.CallRef || got.SegNo != 1 || got.SegTotal != 2 {
		t.Fatalf("mismatch: %+v", got)
	}
}

func TestDelimitedRoundTrip(t *testing.T) {
	c := sampleCDR()
	got, err := ParseDelimited(EncodeDelimited(c))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.RecordID != c.RecordID || got.Service != c.Service || got.SegTotal != 2 {
		t.Fatalf("mismatch: %+v", got)
	}
}

func TestASN1RoundTrip(t *testing.T) {
	c := sampleCDR()
	data, err := EncodeASN1(c)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := ParseASN1(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.RecordID != c.RecordID || !got.StartTime.Equal(c.StartTime) ||
		!got.EndTime.Equal(c.EndTime) || got.Quantity != c.Quantity ||
		got.Roaming != c.Roaming || got.CallRef != c.CallRef {
		t.Fatalf("mismatch: %+v", got)
	}
}

func TestLPBinRoundTrip(t *testing.T) {
	c := sampleCDR()
	data, err := EncodeLPBin(c)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(data) != 4+FixedLineWidth {
		t.Fatalf("lpbin len = %d", len(data))
	}
	got, err := ParseLPBin(data[4:])
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.RecordID != c.RecordID || got.SourceType != model.SourceLPBin {
		t.Fatalf("mismatch: %+v", got)
	}
}

func TestShardStable(t *testing.T) {
	a := ShardOf("16600001234", 64)
	b := ShardOf("16600001234", 64)
	if a != b || a < 0 || a >= 64 {
		t.Fatalf("unstable shard %d/%d", a, b)
	}
}
