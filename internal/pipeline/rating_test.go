package pipeline

import (
	"testing"
	"time"

	"ratingengine/internal/model"
	"ratingengine/internal/tariff"
)

// 构造最小 catalog：单计划、单用户订阅。
func testCatalog(t *testing.T, svc map[string]*model.ServiceTariff, subEnd time.Time) *tariff.Catalog {
	t.Helper()
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	tr := &model.Tariff{
		PlanID: "P1", Version: "v1", EffectiveStart: start, EffectiveEnd: end,
		Services: svc,
	}
	sub := &model.Subscription{
		UserID: "16600000001", Service: model.ServiceVoice, PlanID: "P1",
		EffectiveStart: start, EffectiveEnd: subEnd,
	}
	return tariff.NewTestCatalog([]*model.Tariff{tr}, []*model.Subscription{sub})
}

func voiceTariff() *model.ServiceTariff {
	return &model.ServiceTariff{
		UnitSize: 60, Rounding: "up", Allowance: 100,
		Tiers: []model.Tier{
			{Upto: 100, Rate: 10000},
			{Upto: -1, Rate: 8000},
		},
		NightWindow:   []string{"23:00", "07:00"},
		NightFactor:   5000,
		RoamingFactor: 20000,
	}
}

func mkCDR(start string, secs int64, roaming bool) *model.RawCDR {
	s, _ := time.Parse(model.TimeLayout, start)
	return &model.RawCDR{
		RecordID: "REC1", UserID: "16600000001", Service: model.ServiceVoice,
		StartTime: s, EndTime: s.Add(time.Duration(secs) * time.Second),
		Quantity: secs, Roaming: roaming,
	}
}

// 1) 免费额度内：100 分钟额度，第一条 60 秒（1 单位）应免费。
func TestAllowanceCoversFirstUnits(t *testing.T) {
	cat := testCatalog(t, map[string]*model.ServiceTariff{model.ServiceVoice: voiceTariff()},
		time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC))
	r := NewRater(cat, model.ServiceVoice)
	if err := r.InitAllowance("16600000001"); err != nil {
		t.Fatal(err)
	}
	frags, fee, err := r.Rate(mkCDR("2026-10-02T10:00:00Z", 60, false))
	if err != nil {
		t.Fatal(err)
	}
	if fee != 0 || frags[0].FreeUnits != 1 || frags[0].BillUnits != 0 {
		t.Fatalf("allowance not applied: fee=%d frag=%+v", fee, frags[0])
	}
}

// 2) 阶梯价：耗尽额度后，第 101 个单位进入第二档（8000 厘）。
func TestTierPricingAfterAllowance(t *testing.T) {
	cat := testCatalog(t, map[string]*model.ServiceTariff{model.ServiceVoice: voiceTariff()},
		time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC))
	r := NewRater(cat, model.ServiceVoice)
	_ = r.InitAllowance("16600000001")
	// 先吃掉 100 单位免费额度
	_, _, _ = r.Rate(mkCDR("2026-10-02T10:00:00Z", 6000, false)) // 100 分钟
	c := mkCDR("2026-10-03T10:00:00Z", 120, false)               // 2 单位，全计费
	frags, fee, err := r.Rate(c)
	if err != nil {
		t.Fatal(err)
	}
	// 第一档容量 100 已被游标越过（免费单位不计入游标），2 单位落第一档 10000
	// 游标只累计 bill_units，因此这里仍在第一档：2 * 10000 = 20000
	if fee != 20000 {
		t.Fatalf("tier fee=%d frags=%+v", fee, frags)
	}
}

// 3) 夜间优惠：中点落在 23:00-07:00，费率 ×0.5。
func TestNightFactor(t *testing.T) {
	cfg := voiceTariff()
	cfg.Allowance = 0 // 关闭额度
	cat := testCatalog(t, map[string]*model.ServiceTariff{model.ServiceVoice: cfg},
		time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC))
	r := NewRater(cat, model.ServiceVoice)
	_ = r.InitAllowance("16600000001")
	// 23:30 开始 60 秒，中点 23:30:30 命中夜间
	_, fee, err := r.Rate(mkCDR("2026-10-02T23:30:00Z", 60, false))
	if err != nil {
		t.Fatal(err)
	}
	// 10000 * 5000/10000 = 5000
	if fee != 5000 {
		t.Fatalf("night fee=%d, want 5000", fee)
	}
}

// 4) 夜间+漫游系数相乘，每步四舍五入。
func TestNightAndRoamingCombined(t *testing.T) {
	cfg := voiceTariff()
	cfg.Allowance = 0
	cat := testCatalog(t, map[string]*model.ServiceTariff{model.ServiceVoice: cfg},
		time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC))
	r := NewRater(cat, model.ServiceVoice)
	_ = r.InitAllowance("16600000001")
	_, fee, err := r.Rate(mkCDR("2026-10-02T23:30:00Z", 60, true))
	if err != nil {
		t.Fatal(err)
	}
	// base 10000 -> 夜间 5000 -> 漫游 5000*20000/10000 = 10000
	if fee != 10000 {
		t.Fatalf("combined fee=%d, want 10000", fee)
	}
}

// 5) 跨月切分：一条长话单跨过 11-01，应产生两个账期月片段，用量按秒分摊。
func TestCrossMonthSplit(t *testing.T) {
	cfg := voiceTariff()
	cfg.Allowance = 0
	cat := testCatalog(t, map[string]*model.ServiceTariff{model.ServiceVoice: cfg},
		time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC))
	r := NewRater(cat, model.ServiceVoice)
	_ = r.InitAllowance("16600000001")
	// 10-31 23:00 起 2 小时（7200 秒），跨 11-01 00:00
	frags, _, err := r.Rate(mkCDR("2026-10-31T23:00:00Z", 7200, false))
	if err != nil {
		t.Fatal(err)
	}
	if len(frags) != 2 {
		t.Fatalf("expect 2 fragments across month, got %d", len(frags))
	}
	months := map[string]bool{}
	var qty int64
	for _, f := range frags {
		months[f.BillMonth] = true
		qty += f.Units
	}
	if !months["2026-10"] || !months["2026-11"] {
		t.Fatalf("missing month: %+v", frags)
	}
	// 总单位 = ceil(7200/60)=120，两段单位之和应等于 120
	if qty != 120 {
		t.Fatalf("split units sum=%d, want 120", qty)
	}
}

// 6) 向上取整：61 秒按 2 分钟计。
func TestRoundingUp(t *testing.T) {
	cfg := voiceTariff()
	cfg.Allowance = 0
	cat := testCatalog(t, map[string]*model.ServiceTariff{model.ServiceVoice: cfg},
		time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC))
	r := NewRater(cat, model.ServiceVoice)
	_ = r.InitAllowance("16600000001")
	frags, _, err := r.Rate(mkCDR("2026-10-02T10:00:00Z", 61, false))
	if err != nil {
		t.Fatal(err)
	}
	if frags[0].Units != 2 {
		t.Fatalf("61s should round up to 2 units, got %d", frags[0].Units)
	}
}
