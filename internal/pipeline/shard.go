package pipeline

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"ratingengine/internal/model"
	"ratingengine/internal/store"
	"ratingengine/internal/tariff"
)

// ShardResult 分片处理结果计数。
type ShardResult struct {
	Input             int64
	Rated             int64
	Dup               int64
	Exception         int64
	ExceptionByReason map[string]int64
}

// workItem 待批价的话单及其合并上下文。
type workItem struct {
	cdr       *model.RawCDR
	merged    bool
	mergedIDs []string
}

// ProcessShard 执行单个分片的完整处理：去重 -> 合并 -> 校验 -> 批价 -> 汇总。
// 输出整文件覆盖，重复执行幂等，支持断点续跑（由 task 层决定是否调用）。
func ProcessShard(st *store.Store, cat *tariff.Catalog, taskID string, shard, shardCount int) (*ShardResult, error) {
	res := &ShardResult{ExceptionByReason: map[string]int64{}}

	// 1. 读入分片原始话单
	var raw []*model.RawCDR
	if err := store.ReadJSONLInto(st.RawPath(taskID, shard), func(c *model.RawCDR) error {
		res.Input++
		raw = append(raw, c)
		return nil
	}); err != nil {
		return nil, err
	}

	ratedW, err := store.CreateJSONL(st.RatedPath(taskID, shard))
	if err != nil {
		return nil, err
	}
	exW, err := store.CreateJSONL(st.ExceptionPath(taskID, shard))
	if err != nil {
		_ = ratedW.Close()
		return nil, err
	}
	mergeW, err := store.CreateJSONL(st.MergePath(taskID, shard))
	if err != nil {
		_ = ratedW.Close()
		_ = exW.Close()
		return nil, err
	}
	sumW, err := store.CreateJSONL(st.SummaryPath(taskID, shard))
	if err != nil {
		_ = ratedW.Close()
		_ = exW.Close()
		_ = mergeW.Close()
		return nil, err
	}
	rollback := func(e error) error {
		_ = ratedW.Close()
		_ = exW.Close()
		_ = mergeW.Close()
		_ = sumW.Close()
		return e
	}

	// 1.1 先把接入阶段异常并入统一异常文件（整文件覆盖，幂等）
	if err := store.ReadJSONLInto(st.IngestExceptionPath(taskID, shard), func(e *model.ExceptionCDR) error {
		res.Exception++
		res.ExceptionByReason[e.Reason]++
		return exW.Write(e)
	}); err != nil {
		return nil, rollback(err)
	}

	emitEx := func(c *model.RawCDR, reason, detail string) error {
		res.Exception++
		res.ExceptionByReason[reason]++
		snippet := c.RawLine
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return exW.Write(&model.ExceptionCDR{
			RecordID: c.RecordID, UserID: c.UserID, Shard: shard,
			Reason: reason, Detail: detail, RawSnippet: snippet,
			Stage: "rate", OccurredAt: time.Now().UTC().Format(model.TimeLayout),
		})
	}

	// 2. 去重（record_id 保留首条）
	seen := map[string]struct{}{}
	uniq := make([]*model.RawCDR, 0, len(raw))
	for _, c := range raw {
		if _, ok := seen[c.RecordID]; ok {
			res.Dup++
			continue
		}
		seen[c.RecordID] = struct{}{}
		uniq = append(uniq, c)
	}

	// 3. 拆段合并：按 (user_id, call_ref) 分组，防止引用号跨用户碰撞导致误合并
	groups := map[string][]*model.RawCDR{}
	var groupOrder []string
	var standalone []*model.RawCDR
	groupKey := func(c *model.RawCDR) string { return c.UserID + "|" + c.CallRef }
	for _, c := range uniq {
		if c.CallRef == "" {
			standalone = append(standalone, c)
			continue
		}
		key := groupKey(c)
		if _, ok := groups[key]; !ok {
			groupOrder = append(groupOrder, key)
		}
		groups[key] = append(groups[key], c)
	}

	var work []workItem
	work = append(work, workItemsOf(standalone)...)

	for _, ref := range groupOrder {
		segs := groups[ref]
		sort.Slice(segs, func(i, j int) bool {
			if segs[i].SegNo != segs[j].SegNo {
				return segs[i].SegNo < segs[j].SegNo
			}
			return segs[i].RecordID < segs[j].RecordID
		})
		// E5 校验：段号集合必须恰好为 1..seg_total
		bad := false
		var detail string
		segTotal := segs[0].SegTotal
		if segTotal != len(segs) {
			bad = true
			detail = fmt.Sprintf("seg count %d != seg_total %d", len(segs), segTotal)
		}
		nos := make(map[int]bool, len(segs))
		for _, sg := range segs {
			if sg.SegTotal != segTotal {
				bad = true
				detail = fmt.Sprintf("inconsistent seg_total: %d vs %d", sg.SegTotal, segTotal)
			}
			if nos[sg.SegNo] || sg.SegNo < 1 || sg.SegNo > segTotal {
				bad = true
				detail = fmt.Sprintf("duplicate or out-of-range seg_no %d", sg.SegNo)
			}
			nos[sg.SegNo] = true
		}
		if !bad {
			for i := 1; i <= segTotal; i++ {
				if !nos[i] {
					bad = true
					detail = fmt.Sprintf("missing seg_no %d", i)
					break
				}
			}
		}
		if bad {
			for _, sg := range segs {
				if e := emitEx(sg, model.ErrBadSegment, detail); e != nil {
					return nil, rollback(e)
				}
			}
			continue
		}

		// 合并
		head := segs[0]
		merged := &model.RawCDR{
			RecordID: head.RecordID, SourceType: head.SourceType,
			UserID: head.UserID, Service: head.Service,
			StartTime: segs[0].StartTime, EndTime: segs[0].EndTime,
			Roaming: head.Roaming, CallRef: head.CallRef,
			SegNo: 1, SegTotal: segTotal, RawLine: head.RawLine,
		}
		ids := make([]string, 0, len(segs))
		for _, sg := range segs {
			ids = append(ids, sg.RecordID)
			merged.Quantity += sg.Quantity
			if sg.StartTime.Before(merged.StartTime) {
				merged.StartTime = sg.StartTime
			}
			if sg.EndTime.After(merged.EndTime) {
				merged.EndTime = sg.EndTime
			}
		}
		if err := mergeW.Write(&model.MergeRecord{
			CallRef: head.CallRef, UserID: head.UserID, KeptID: head.RecordID,
			MergedIDs: ids, SegTotal: segTotal,
		}); err != nil {
			return nil, rollback(err)
		}
		work = append(work, workItem{cdr: merged, merged: true, mergedIDs: ids})
	}

	// 4. 按用户、业务分组批价
	byUserSvc := map[string][]*workItem{}
	var keyOrder []string
	for i := range work {
		item := &work[i]
		key := item.cdr.UserID + "|" + item.cdr.Service
		if _, ok := byUserSvc[key]; !ok {
			keyOrder = append(keyOrder, key)
		}
		byUserSvc[key] = append(byUserSvc[key], item)
	}
	sort.Strings(keyOrder)

	// 汇总累加器
	type sumKey struct {
		user, month string
	}
	sums := map[sumKey]*model.UserSummary{}

	for _, key := range keyOrder {
		items := byUserSvc[key]
		userId, service := splitKey(key)

		// E2/E3 校验并排序（start_time, record_id 确定性顺序）
		valid := make([]*workItem, 0, len(items))
		for _, item := range items {
			c := item.cdr
			if c.EndTime.Before(c.StartTime) || c.EndTime.Equal(c.StartTime) && service != model.ServiceSMS {
				if e := emitEx(c, model.ErrBadTime, "end_time <= start_time"); e != nil {
					return nil, rollback(e)
				}
				continue
			}
			if c.Quantity <= 0 {
				if e := emitEx(c, model.ErrBadQuantity, "quantity <= 0"); e != nil {
					return nil, rollback(e)
				}
				continue
			}
			if cat.SubscriptionAt(userId, service, c.StartTime) == nil {
				if e := emitEx(c, model.ErrNoSubscription,
					fmt.Sprintf("no %s subscription at %s", service, c.StartTime.Format(model.TimeLayout))); e != nil {
					return nil, rollback(e)
				}
				continue
			}
			valid = append(valid, item)
		}
		sort.Slice(valid, func(i, j int) bool {
			a, b := valid[i].cdr, valid[j].cdr
			if !a.StartTime.Equal(b.StartTime) {
				return a.StartTime.Before(b.StartTime)
			}
			return a.RecordID < b.RecordID
		})

		rater := NewRater(cat, service)
		if err := rater.InitAllowance(userId); err != nil {
			return nil, rollback(err)
		}

		for _, item := range valid {
			c := item.cdr
			frags, fee, err := rater.Rate(c)
			if err != nil {
				if errors.Is(err, ErrNoSubscription) {
					if e := emitEx(c, model.ErrNoSubscription, err.Error()); e != nil {
						return nil, rollback(e)
					}
					continue
				}
				return nil, rollback(err)
			}
			rated := &model.RatedCDR{
				RecordID: c.RecordID, UserID: c.UserID, Service: service, Shard: shard,
				StartTime: c.StartTime, EndTime: c.EndTime, Quantity: c.Quantity,
				Roaming: c.Roaming, CallRef: c.CallRef,
				Merged: item.merged, MergedIDs: item.mergedIDs,
				Fragments: frags, TotalFee: fee,
			}
			if err := ratedW.Write(rated); err != nil {
				return nil, rollback(err)
			}
			res.Rated++

			// 汇总：片段可能跨月
			monthsCounted := map[string]bool{}
			for _, fg := range frags {
				sk := sumKey{userId, fg.BillMonth}
				sum := sums[sk]
				if sum == nil {
					sum = &model.UserSummary{
						UserID: userId, BillMonth: fg.BillMonth, Shard: shard,
						ServiceUnits: map[string]int64{},
					}
					sums[sk] = sum
				}
				sum.ServiceUnits[service] += fg.Units
				sum.TotalFee += fg.Fee
				if !monthsCounted[fg.BillMonth] {
					sum.ValidCount++
					monthsCounted[fg.BillMonth] = true
				}
			}
		}
	}

	// 5. 写用户汇总（按 user, month 排序）
	sumKeys := make([]sumKey, 0, len(sums))
	for k := range sums {
		sumKeys = append(sumKeys, k)
	}
	sort.Slice(sumKeys, func(i, j int) bool {
		if sumKeys[i].user != sumKeys[j].user {
			return sumKeys[i].user < sumKeys[j].user
		}
		return sumKeys[i].month < sumKeys[j].month
	})
	for _, k := range sumKeys {
		if err := sumW.Write(sums[k]); err != nil {
			return nil, rollback(err)
		}
	}

	if err := ratedW.Close(); err != nil {
		return nil, rollback(err)
	}
	if err := exW.Close(); err != nil {
		return nil, rollback(err)
	}
	if err := mergeW.Close(); err != nil {
		return nil, rollback(err)
	}
	if err := sumW.Close(); err != nil {
		return nil, rollback(err)
	}
	return res, nil
}

func workItemsOf(cdrs []*model.RawCDR) []workItem {
	out := make([]workItem, len(cdrs))
	for i, c := range cdrs {
		out[i] = workItem{cdr: c}
	}
	return out
}

func splitKey(k string) (string, string) {
	for i := 0; i < len(k); i++ {
		if k[i] == '|' {
			return k[:i], k[i+1:]
		}
	}
	return k, ""
}
