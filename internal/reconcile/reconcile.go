// Package reconcile 实现引擎内部对平、重算差额与 oracle 独立对账。
package reconcile

import (
	"fmt"
	"sort"
	"time"

	"ratingengine/internal/model"
	"ratingengine/internal/store"
)

// UserFeeRow 用户×月×业务的费用行（从 rated 明细聚合）。
type UserFeeRow struct {
	UserID     string
	BillMonth  string
	ServiceFee map[string]int64
	TotalFee   int64
}

// AggregateRated 扫描任务全部分片 rated.jsonl，按 用户×月 聚合业务级费用。
func AggregateRated(st *store.Store, taskID string, shardCount int) (map[string]*UserFeeRow, error) {
	out := map[string]*UserFeeRow{}
	for sh := 0; sh < shardCount; sh++ {
		err := store.ReadJSONLInto(st.RatedPath(taskID, sh), func(c *model.RatedCDR) error {
			months := map[string]map[string]int64{} // month -> service -> fee
			for _, fg := range c.Fragments {
				if months[fg.BillMonth] == nil {
					months[fg.BillMonth] = map[string]int64{}
				}
				months[fg.BillMonth][c.Service] += fg.Fee
			}
			for month, svcFee := range months {
				key := c.UserID + "|" + month
				row := out[key]
				if row == nil {
					row = &UserFeeRow{UserID: c.UserID, BillMonth: month, ServiceFee: map[string]int64{}}
					out[key] = row
				}
				for svc, fee := range svcFee {
					row.ServiceFee[svc] += fee
					row.TotalFee += fee
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Mismatch 对账差异明细。
type Mismatch struct {
	UserID    string           `json:"user_id"`
	BillMonth string           `json:"bill_month"`
	Engine    int64            `json:"engine_fee"`
	Oracle    int64            `json:"oracle_fee"`
	Diff      int64            `json:"diff"`
	Service   map[string]int64 `json:"engine_service_fee,omitempty"`
	OracleSvc map[string]int64 `json:"oracle_service_fee,omitempty"`
	Kind      string           `json:"kind"` // FEE_MISMATCH / MISSING_IN_ENGINE / MISSING_IN_ORACLE
}

// Report 对账报告。
type Report struct {
	TaskID         string     `json:"task_id"`
	OracleRows     int        `json:"oracle_rows"`
	EngineRows     int        `json:"engine_rows"`
	MatchedRows    int        `json:"matched_rows"`
	MismatchRows   int        `json:"mismatch_rows"`
	EngineTotalFee int64      `json:"engine_total_fee"`
	OracleTotalFee int64      `json:"oracle_total_fee"`
	Balanced       bool       `json:"balanced"`
	Mismatches     []Mismatch `json:"mismatches"`
}

// OracleUserRow oracle 应收文件的用户汇总行。
type OracleUserRow struct {
	UserID       string           `json:"user_id"`
	BillMonth    string           `json:"bill_month"`
	ServiceFee   map[string]int64 `json:"service_fee"`
	ServiceUnits map[string]int64 `json:"service_units"`
	TotalFee     int64            `json:"total_fee"`
	ValidCount   int64            `json:"valid_count"`
}

// CompareWithOracle 引擎结果 vs Python 独立应收，逐用户×月核对。
func CompareWithOracle(st *store.Store, taskID string, shardCount int) (*Report, error) {
	engine, err := AggregateRated(st, taskID, shardCount)
	if err != nil {
		return nil, err
	}
	oracle := map[string]*OracleUserRow{}
	oraclePath := st.Root + "/oracle/user_summary.jsonl"
	if err := store.ReadJSONLInto(oraclePath, func(o *OracleUserRow) error {
		oracle[o.UserID+"|"+o.BillMonth] = o
		return nil
	}); err != nil {
		return nil, err
	}

	rep := &Report{TaskID: taskID, EngineRows: len(engine), OracleRows: len(oracle)}
	keys := map[string]bool{}
	for k := range engine {
		keys[k] = true
	}
	for k := range oracle {
		keys[k] = true
	}
	ordered := make([]string, 0, len(keys))
	for k := range keys {
		ordered = append(ordered, k)
	}
	sort.Strings(ordered)

	for _, k := range ordered {
		e := engine[k]
		o := oracle[k]
		switch {
		case e == nil:
			rep.Mismatches = append(rep.Mismatches, Mismatch{
				UserID: o.UserID, BillMonth: o.BillMonth, Oracle: o.TotalFee,
				Diff: -o.TotalFee, OracleSvc: o.ServiceFee, Kind: "MISSING_IN_ENGINE",
			})
		case o == nil:
			rep.Mismatches = append(rep.Mismatches, Mismatch{
				UserID: e.UserID, BillMonth: e.BillMonth, Engine: e.TotalFee,
				Diff: e.TotalFee, Service: e.ServiceFee, Kind: "MISSING_IN_ORACLE",
			})
		default:
			rep.EngineTotalFee += e.TotalFee
			rep.OracleTotalFee += o.TotalFee
			if e.TotalFee == o.TotalFee {
				rep.MatchedRows++
				continue
			}
			rep.Mismatches = append(rep.Mismatches, Mismatch{
				UserID: e.UserID, BillMonth: e.BillMonth,
				Engine: e.TotalFee, Oracle: o.TotalFee, Diff: e.TotalFee - o.TotalFee,
				Service: e.ServiceFee, OracleSvc: o.ServiceFee, Kind: "FEE_MISMATCH",
			})
		}
	}
	rep.MismatchRows = len(rep.Mismatches)
	rep.Balanced = rep.MismatchRows == 0 && rep.EngineTotalFee == rep.OracleTotalFee
	return rep, nil
}

// Delta 重算差额行。
type Delta struct {
	UserID       string           `json:"user_id"`
	BillMonth    string           `json:"bill_month"`
	OldFee       int64            `json:"old_fee"`
	NewFee       int64            `json:"new_fee"`
	Delta        int64            `json:"delta"`
	ServiceDelta map[string]int64 `json:"service_delta"`
}

// DiffTasks 对比新旧两个任务的用户×月费用，输出差额（delta = new - old）。
// monthFilter 非空时只输出与窗口相交账期月。
func DiffTasks(st *store.Store, oldTaskID, newTaskID string, shardCount int, monthFilter map[string]bool) ([]Delta, error) {
	oldRows, err := AggregateRated(st, oldTaskID, shardCount)
	if err != nil {
		return nil, err
	}
	newRows, err := AggregateRated(st, newTaskID, shardCount)
	if err != nil {
		return nil, err
	}
	keys := map[string]bool{}
	for k := range oldRows {
		keys[k] = true
	}
	for k := range newRows {
		keys[k] = true
	}
	ordered := make([]string, 0, len(keys))
	for k := range keys {
		ordered = append(ordered, k)
	}
	sort.Strings(ordered)

	var out []Delta
	for _, k := range ordered {
		var user, month string
		for i := 0; i < len(k); i++ {
			if k[i] == '|' {
				user, month = k[:i], k[i+1:]
				break
			}
		}
		if len(monthFilter) > 0 && !monthFilter[month] {
			continue
		}
		o := oldRows[k]
		n := newRows[k]
		var oldFee, newFee int64
		svcDelta := map[string]int64{}
		if o != nil {
			oldFee = o.TotalFee
			for svc, fee := range o.ServiceFee {
				svcDelta[svc] -= fee
			}
		}
		if n != nil {
			newFee = n.TotalFee
			for svc, fee := range n.ServiceFee {
				svcDelta[svc] += fee
			}
		}
		d := newFee - oldFee
		// 清理零差额业务
		for svc, v := range svcDelta {
			if v == 0 {
				delete(svcDelta, svc)
			}
		}
		if d == 0 && len(svcDelta) == 0 {
			continue // 无任何差额的用户×月不输出
		}
		out = append(out, Delta{
			UserID: user, BillMonth: month,
			OldFee: oldFee, NewFee: newFee, Delta: d, ServiceDelta: svcDelta,
		})
	}
	return out, nil
}

// SaveDeltas 写差额 JSONL 并返回总额。
func SaveDeltas(st *store.Store, taskID string, deltas []Delta) (int64, error) {
	w, err := store.CreateJSONL(st.DeltaPath(taskID))
	if err != nil {
		return 0, err
	}
	var total int64
	for _, d := range deltas {
		if err := w.Write(d); err != nil {
			_ = w.Close()
			return 0, err
		}
		total += d.Delta
	}
	if err := w.Close(); err != nil {
		return 0, err
	}
	return total, nil
}

// MonthWindow 把 [start,end) 时间窗转为覆盖的账期月集合（YYYY-MM）。
func MonthWindow(start, end string) (map[string]bool, error) {
	const layout = "2006-01-02T15:04:05Z"
	s, err := time.Parse(layout, start)
	if err != nil {
		return nil, fmt.Errorf("bad window start: %w", err)
	}
	e, err := time.Parse(layout, end)
	if err != nil {
		return nil, fmt.Errorf("bad window end: %w", err)
	}
	if !e.After(s) {
		return nil, fmt.Errorf("window end must be after start")
	}
	out := map[string]bool{}
	cur := s
	for cur.Before(e) {
		out[fmt.Sprintf("%04d-%02d", cur.Year(), cur.Month())] = true
		cur = cur.AddDate(0, 1, 0)
	}
	return out, nil
}
