// Package tariff 加载资费版本与用户订阅，提供按时刻解析资费的目录查询。
package tariff

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"ratingengine/internal/model"
	"ratingengine/internal/store"
)

// Catalog 资费 + 订阅的只读目录。
type Catalog struct {
	tariffs      []*model.Tariff                             // 按 plan_id, version 索引
	tariffByPlan map[string][]*model.Tariff                  // plan_id -> 版本（按生效排序）
	subs         map[string]map[string][]*model.Subscription // user -> service -> 区间（按生效排序）
}

// Load 从 store 目录加载全部资费 JSON 与订阅 JSONL。
func Load(st *store.Store) (*Catalog, error) {
	c := &Catalog{
		tariffByPlan: map[string][]*model.Tariff{},
		subs:         map[string]map[string][]*model.Subscription{},
	}

	tariffFiles, err := filepath.Glob(filepath.Join(st.TariffDir(), "*.json"))
	if err != nil {
		return nil, err
	}
	for _, p := range tariffFiles {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var t model.Tariff
		if err := json.Unmarshal(data, &t); err != nil {
			return nil, fmt.Errorf("load tariff %s: %w", p, err)
		}
		c.tariffs = append(c.tariffs, &t)
		c.tariffByPlan[t.PlanID] = append(c.tariffByPlan[t.PlanID], &t)
	}
	for plan := range c.tariffByPlan {
		sort.Slice(c.tariffByPlan[plan], func(i, j int) bool {
			return c.tariffByPlan[plan][i].EffectiveStart.Before(c.tariffByPlan[plan][j].EffectiveStart)
		})
	}

	if err := store.ReadJSONLInto(st.SubFile(), func(sub *model.Subscription) error {
		if c.subs[sub.UserID] == nil {
			c.subs[sub.UserID] = map[string][]*model.Subscription{}
		}
		c.subs[sub.UserID][sub.Service] = append(c.subs[sub.UserID][sub.Service], sub)
		return nil
	}); err != nil {
		return nil, err
	}
	for u := range c.subs {
		for svc := range c.subs[u] {
			sort.Slice(c.subs[u][svc], func(i, j int) bool {
				return c.subs[u][svc][i].EffectiveStart.Before(c.subs[u][svc][j].EffectiveStart)
			})
		}
	}
	return c, nil
}

// SubscriptionAt 返回用户某业务在 t 时刻生效的订阅；无匹配返回 nil。
func (c *Catalog) SubscriptionAt(user, service string, t time.Time) *model.Subscription {
	for _, sub := range c.subs[user][service] {
		if !t.Before(sub.EffectiveStart) && t.Before(sub.EffectiveEnd) {
			return sub
		}
	}
	return nil
}

// Subscriptions 返回用户某业务全部订阅（已按生效排序）。
func (c *Catalog) Subscriptions(user, service string) []*model.Subscription {
	return c.subs[user][service]
}

// TariffAt 返回计划在 t 时刻生效的资费版本；无匹配返回错误。
func (c *Catalog) TariffAt(planID string, t time.Time) (*model.Tariff, error) {
	versions := c.tariffByPlan[planID]
	var hit *model.Tariff
	for _, v := range versions {
		if !t.Before(v.EffectiveStart) && t.Before(v.EffectiveEnd) {
			hit = v
			break
		}
	}
	if hit == nil {
		return nil, fmt.Errorf("no tariff version of %s effective at %s", planID, t.Format(model.TimeLayout))
	}
	return hit, nil
}

// TariffByVersion 按 plan_id + version 精确取资费。
func (c *Catalog) TariffByVersion(planID, version string) (*model.Tariff, error) {
	for _, t := range c.tariffByPlan[planID] {
		if t.Version == version {
			return t, nil
		}
	}
	return nil, fmt.Errorf("tariff %s/%s not found", planID, version)
}

// AllTariffs 暴露全部资费（页面/导出用）。
func (c *Catalog) AllTariffs() []*model.Tariff { return c.tariffs }

// SplitBoundaries 返回落在 (s, e) 内部的全部切分时刻：
// 月界、订阅变更、资费版本生效边界（去重升序）。
func (c *Catalog) SplitBoundaries(user, service string, s, e time.Time) []time.Time {
	set := map[time.Time]struct{}{}

	// 月界
	first := time.Date(s.Year(), s.Month()+1, 1, 0, 0, 0, 0, time.UTC)
	for b := first; b.Before(e); b = b.AddDate(0, 1, 0) {
		if b.After(s) && b.Before(e) {
			set[b] = struct{}{}
		}
	}

	// 订阅边界
	for _, sub := range c.subs[user][service] {
		if sub.EffectiveStart.After(s) && sub.EffectiveStart.Before(e) {
			set[sub.EffectiveStart] = struct{}{}
		}
		if sub.EffectiveEnd.After(s) && sub.EffectiveEnd.Before(e) {
			set[sub.EffectiveEnd] = struct{}{}
		}
	}

	// 资费版本边界（覆盖该用户此业务引用过的全部 plan）
	seenPlan := map[string]bool{}
	for _, sub := range c.subs[user][service] {
		if seenPlan[sub.PlanID] {
			continue
		}
		seenPlan[sub.PlanID] = true
		for _, v := range c.tariffByPlan[sub.PlanID] {
			if v.EffectiveStart.After(s) && v.EffectiveStart.Before(e) {
				set[v.EffectiveStart] = struct{}{}
			}
			if v.EffectiveEnd.After(s) && v.EffectiveEnd.Before(e) {
				set[v.EffectiveEnd] = struct{}{}
			}
		}
	}

	out := make([]time.Time, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out
}

// NightWindow 解析夜间窗口 ["23:00","07:00"] 为分钟数（UTC）。
type NightWindow struct {
	StartMin int // 含
	EndMin   int // 不含；EndMin <= StartMin 表示跨零点
}

func ParseNightWindow(w []string) (*NightWindow, error) {
	if len(w) != 2 {
		return nil, fmt.Errorf("night window must have 2 entries")
	}
	parse := func(x string) (int, error) {
		parts := strings.Split(x, ":")
		if len(parts) != 2 {
			return 0, fmt.Errorf("bad hh:mm %q", x)
		}
		var h, m int
		if _, err := fmt.Sscanf(x, "%d:%d", &h, &m); err != nil {
			return 0, err
		}
		if h < 0 || h > 23 || m < 0 || m > 59 {
			return 0, fmt.Errorf("time out of range %q", x)
		}
		_ = parts
		return h*60 + m, nil
	}
	a, err := parse(w[0])
	if err != nil {
		return nil, err
	}
	b, err := parse(w[1])
	if err != nil {
		return nil, err
	}
	return &NightWindow{StartMin: a, EndMin: b}, nil
}

// Contains 判断 UTC 时刻是否落在夜间窗口。
func (w *NightWindow) Contains(t time.Time) bool {
	cur := t.UTC().Hour()*60 + t.UTC().Minute()
	if w.StartMin <= w.EndMin {
		return cur >= w.StartMin && cur < w.EndMin
	}
	// 跨零点，如 23:00–07:00
	return cur >= w.StartMin || cur < w.EndMin
}
