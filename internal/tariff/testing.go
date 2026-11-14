package tariff

import (
	"sort"

	"ratingengine/internal/model"
)

// NewTestCatalog 用给定资费与订阅构造内存 Catalog，供其他包测试使用。
func NewTestCatalog(tariffs []*model.Tariff, subs []*model.Subscription) *Catalog {
	c := &Catalog{
		tariffByPlan: map[string][]*model.Tariff{},
		subs:         map[string]map[string][]*model.Subscription{},
	}
	for _, t := range tariffs {
		c.tariffs = append(c.tariffs, t)
		c.tariffByPlan[t.PlanID] = append(c.tariffByPlan[t.PlanID], t)
	}
	for plan := range c.tariffByPlan {
		sort.Slice(c.tariffByPlan[plan], func(i, j int) bool {
			return c.tariffByPlan[plan][i].EffectiveStart.Before(c.tariffByPlan[plan][j].EffectiveStart)
		})
	}
	for _, s := range subs {
		if c.subs[s.UserID] == nil {
			c.subs[s.UserID] = map[string][]*model.Subscription{}
		}
		c.subs[s.UserID][s.Service] = append(c.subs[s.UserID][s.Service], s)
	}
	for u := range c.subs {
		for svc := range c.subs[u] {
			sort.Slice(c.subs[u][svc], func(i, j int) bool {
				return c.subs[u][svc][i].EffectiveStart.Before(c.subs[u][svc][j].EffectiveStart)
			})
		}
	}
	return c
}
