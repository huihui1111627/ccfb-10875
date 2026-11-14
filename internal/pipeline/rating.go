// Package pipeline 实现话单接入、去重合并、切分批价与汇总的完整流水线。
package pipeline

import (
	"fmt"
	"sort"
	"time"

	"ratingengine/internal/model"
	"ratingengine/internal/tariff"
)

const basis = 10000 // 系数基点：10000 = 原价

// poolKey 免费额度池标识。
type poolKey struct {
	billMonth string
	planID    string
	version   string
}

// tierCursor 同一资费版本内已计费单位数（跨池累计，SPEC §4.3）。
type tierCursor struct {
	planID  string
	version string
	used    int64
}

// Rater 单分片单业务的批价状态机（非并发安全，每个分片 goroutine 独立持有）。
type Rater struct {
	cat       *tariff.Catalog
	service   string
	allowance map[poolKey]int64 // 剩余免费单位
	cursors   map[string]*tierCursor
	nwCache   map[string]*tariff.NightWindow
}

func NewRater(cat *tariff.Catalog, service string) *Rater {
	return &Rater{
		cat:       cat,
		service:   service,
		allowance: map[poolKey]int64{},
		cursors:   map[string]*tierCursor{},
		nwCache:   map[string]*tariff.NightWindow{},
	}
}

// InitAllowance 根据该用户此业务的全部订阅预建额度池。
func (r *Rater) InitAllowance(userID string) error {
	subs := r.cat.Subscriptions(userID, r.service)
	for _, sub := range subs {
		t, err := r.cat.TariffAt(sub.PlanID, sub.EffectiveStart)
		if err != nil {
			return err
		}
		allow := int64(0)
		if svc, ok := t.Services[r.service]; ok {
			allow = svc.Allowance
		}
		if allow <= 0 {
			continue
		}
		if err := r.buildPools(sub, t, allow); err != nil {
			return err
		}
	}
	return nil
}

// buildPools 按 账期月 -> 资费版本 两级秒数占比切分额度，最后池兜底余数。
// 全程整数乘除，与 Python oracle 的 // 运算逐位一致。
func (r *Rater) buildPools(sub *model.Subscription, firstTariff *model.Tariff, total int64) error {
	// 第一级：账期月切分
	months := splitByMonth(sub.EffectiveStart, sub.EffectiveEnd)
	subSecs := int64(sub.EffectiveEnd.Sub(sub.EffectiveStart) / time.Second)
	if subSecs <= 0 {
		return fmt.Errorf("zero-length subscription %s/%s", sub.UserID, sub.Service)
	}
	monthAllow := make([]int64, len(months))
	assigned := int64(0)
	for i, seg := range months {
		secs := int64(seg.e.Sub(seg.s) / time.Second)
		if i == len(months)-1 {
			monthAllow[i] = total - assigned // 兜底
		} else {
			monthAllow[i] = total * secs / subSecs
			assigned += monthAllow[i]
		}
	}

	// 第二级：每月内按资费版本边界切分
	for i, seg := range months {
		// 收集与该月交集的资费版本
		vers, err := r.tariffVersionsBetween(sub.PlanID, seg.s, seg.e)
		if err != nil {
			return err
		}
		if len(vers) == 0 {
			continue
		}
		mTotal := monthAllow[i]
		mSecs := int64(seg.e.Sub(seg.s) / time.Second)
		mAssigned := int64(0)
		for j, v := range vers {
			vs := maxTime(seg.s, v.EffectiveStart)
			ve := minTime(seg.e, v.EffectiveEnd)
			if !vs.Before(ve) {
				continue
			}
			key := poolKey{billMonth: billMonth(seg.s), planID: sub.PlanID, version: v.Version}
			if j == len(vers)-1 {
				r.allowance[key] += mTotal - mAssigned
			} else {
				vSecs := int64(ve.Sub(vs) / time.Second)
				part := mTotal * vSecs / mSecs
				r.allowance[key] += part
				mAssigned += part
			}
		}
	}
	return nil
}

type timeSeg struct{ s, e time.Time }

func splitByMonth(s, e time.Time) []timeSeg {
	var out []timeSeg
	cur := s
	for cur.Before(e) {
		next := time.Date(cur.Year(), cur.Month()+1, 1, 0, 0, 0, 0, time.UTC)
		if !next.Before(e) {
			out = append(out, timeSeg{cur, e})
			break
		}
		out = append(out, timeSeg{cur, next})
		cur = next
	}
	return out
}

func (r *Rater) tariffVersionsBetween(planID string, s, e time.Time) ([]*model.Tariff, error) {
	// 以 s 时刻版本为起点枚举（catalog 已按生效排序）
	first, err := r.cat.TariffAt(planID, s)
	if err != nil {
		return nil, err
	}
	all := r.cat.AllTariffs()
	var out []*model.Tariff
	started := false
	for _, t := range all {
		if t.PlanID != planID {
			continue
		}
		if t.Version == first.Version {
			started = true
		}
		if started && t.EffectiveStart.Before(e) {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EffectiveStart.Before(out[j].EffectiveStart) })
	return out, nil
}

// Rate 对一条已校验的 CDR 执行切分+批价，返回片段列表与总费用。
// cdr.StartTime 必须能匹配到订阅（E4 在调用前判定）。
func (r *Rater) Rate(cdr *model.RawCDR) ([]model.Fragment, int64, error) {
	sub := r.cat.SubscriptionAt(cdr.UserID, r.service, cdr.StartTime)
	if sub == nil {
		return nil, 0, fmt.Errorf("%w: %s %s", ErrNoSubscription, cdr.UserID, cdr.StartTime)
	}
	bounds := r.cat.SplitBoundaries(cdr.UserID, r.service, cdr.StartTime, cdr.EndTime)
	edges := append([]time.Time{cdr.StartTime}, bounds...)
	edges = append(edges, cdr.EndTime)

	// 每段秒数，用于按时间占比分摊（全程整数运算）
	type segWork struct {
		s, e time.Time
		secs int64
	}
	works := make([]segWork, 0, len(edges)-1)
	totalSecs := int64(cdr.EndTime.Sub(cdr.StartTime) / time.Second)
	for i := 0; i+1 < len(edges); i++ {
		if !edges[i].Before(edges[i+1]) {
			continue
		}
		works = append(works, segWork{edges[i], edges[i+1],
			int64(edges[i+1].Sub(edges[i]) / time.Second)})
	}

	var frags []model.Fragment
	remaining := cdr.Quantity
	for i, w := range works {
		segSub := r.cat.SubscriptionAt(cdr.UserID, r.service, w.s)
		if segSub == nil {
			return nil, 0, fmt.Errorf("%w: segment %s", ErrNoSubscription, w.s)
		}
		t, err := r.cat.TariffAt(segSub.PlanID, w.s)
		if err != nil {
			return nil, 0, err
		}
		svc, ok := t.Services[r.service]
		if !ok {
			return nil, 0, fmt.Errorf("plan %s has no %s tariff", segSub.PlanID, r.service)
		}

		// 用量分摊：最后一段兜底余数
		var rawQty int64
		if r.service == model.ServiceSMS {
			if i == 0 {
				rawQty = cdr.Quantity
			} else {
				rawQty = 0
			}
		} else {
			if i == len(works)-1 {
				rawQty = remaining
			} else {
				rawQty = cdr.Quantity * w.secs / totalSecs
				remaining -= rawQty
			}
		}
		if rawQty <= 0 {
			continue
		}

		units := chargeableUnits(r.service, svc, rawQty)
		if units <= 0 {
			continue
		}

		key := poolKey{billMonth: billMonth(w.s), planID: segSub.PlanID, version: t.Version}
		free := units
		if free > r.allowance[key] {
			free = r.allowance[key]
		}
		if free < 0 {
			free = 0
		}
		r.allowance[key] -= free
		billUnits := units - free

		night := r.isNight(svc, w.s, w.e)
		fee, err := r.charge(svc, segSub.PlanID, t.Version, billUnits, night, cdr.Roaming)
		if err != nil {
			return nil, 0, err
		}
		frags = append(frags, model.Fragment{
			StartTime: w.s, EndTime: w.e,
			BillMonth: key.billMonth, PlanID: segSub.PlanID, TariffVer: t.Version,
			Units: units, FreeUnits: free, BillUnits: billUnits,
			Night: night, Roaming: cdr.Roaming, Fee: fee,
		})
	}
	total := int64(0)
	for _, f := range frags {
		total += f.Fee
	}
	return frags, total, nil
}

// charge 阶梯价 + 夜间/漫游系数，游标跨池累计。
func (r *Rater) charge(svc *model.ServiceTariff, planID, version string, units int64, night, roaming bool) (int64, error) {
	if units <= 0 {
		return 0, nil
	}
	curKey := planID + "/" + version
	cur := r.cursors[curKey]
	if cur == nil {
		cur = &tierCursor{planID: planID, version: version}
		r.cursors[curKey] = cur
	}
	if len(svc.Tiers) == 0 {
		return 0, fmt.Errorf("empty tiers for %s", curKey)
	}

	base := int64(0)
	left := units
	usedBefore := cur.used
	for _, tier := range svc.Tiers {
		if left == 0 {
			break
		}
		var capLeft int64
		if tier.Upto < 0 {
			capLeft = left
		} else {
			if usedBefore >= tier.Upto {
				continue
			}
			capLeft = tier.Upto - usedBefore
			if capLeft > left {
				capLeft = left
			}
		}
		base += capLeft * tier.Rate
		usedBefore += capLeft
		left -= capLeft
	}
	if left > 0 {
		return 0, fmt.Errorf("tiers exhausted for %s", curKey)
	}
	cur.used = usedBefore

	fee := base
	if night && svc.NightFactor > 0 {
		fee = roundHalfUp(fee*svc.NightFactor, basis)
	}
	if roaming && svc.RoamingFactor > 0 {
		fee = roundHalfUp(fee*svc.RoamingFactor, basis)
	}
	return fee, nil
}

func (r *Rater) isNight(svc *model.ServiceTariff, s, e time.Time) bool {
	if len(svc.NightWindow) != 2 {
		return false
	}
	nw := r.nwCache[svc.NightWindow[0]+svc.NightWindow[1]]
	if nw == nil {
		parsed, err := tariff.ParseNightWindow(svc.NightWindow)
		if err != nil {
			return false
		}
		nw = parsed
		r.nwCache[svc.NightWindow[0]+svc.NightWindow[1]] = nw
	}
	// 中点 = s + (e-s)/2，整数秒向下取整
	mid := s.Add((e.Sub(s) / 2))
	return nw.Contains(mid)
}

// chargeableUnits 原始用量 -> 批价单位（向上取整）。
func chargeableUnits(service string, svc *model.ServiceTariff, qty int64) int64 {
	switch service {
	case model.ServiceSMS:
		return qty
	default:
		if svc.UnitSize <= 1 {
			return qty
		}
		return (qty + svc.UnitSize - 1) / svc.UnitSize
	}
}

// roundHalfUp 整数四舍五入到厘（half away from zero，输入非负）。
func roundHalfUp(num, den int64) int64 {
	return (num + den/2) / den
}

func billMonth(t time.Time) string {
	u := t.UTC()
	return fmt.Sprintf("%04d-%02d", u.Year(), u.Month())
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// ErrNoSubscription 供上层识别 E4。
var ErrNoSubscription = fmt.Errorf("no subscription")
