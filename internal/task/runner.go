// Package task 编排批价/重算任务：阶段统计、分片并行与断点续跑。
package task

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	"ratingengine/internal/model"
	"ratingengine/internal/pipeline"
	"ratingengine/internal/store"
	"ratingengine/internal/tariff"
)

const (
	StatusPending = "PENDING"
	StatusRunning = "RUNNING"
	StatusDone    = "DONE"
	StatusFailed  = "FAILED"
)

// Runner 任务执行器。
type Runner struct {
	store    *store.Store
	shards   int
	parallel int
}

func NewRunner(st *store.Store, shards, parallel int) *Runner {
	if parallel <= 0 {
		parallel = runtime.NumCPU()
	}
	if shards <= 0 {
		shards = 64
	}
	return &Runner{store: st, shards: shards, parallel: parallel}
}

// CreateRatingTask 建立任务骨架（目录 + 元数据 + 分片 checkpoint）。
func (r *Runner) CreateRatingTask(taskID, kind string, inputFiles []string, params map[string]string) (*model.TaskMeta, error) {
	if err := os.MkdirAll(r.store.TaskDir(taskID), 0o755); err != nil {
		return nil, err
	}
	now := time.Now().UTC().Format(model.TimeLayout)
	meta := &model.TaskMeta{
		TaskID: taskID, Kind: kind, ShardCount: r.shards,
		Status: StatusPending, CreatedAt: now, UpdatedAt: now,
		Params: params, InputFiles: inputFiles,
		Shards: make([]model.ShardCheckpoint, r.shards),
	}
	for i := range meta.Shards {
		meta.Shards[i] = model.ShardCheckpoint{Shard: i, Status: StatusPending}
		if err := os.MkdirAll(r.store.ShardDir(taskID, i), 0o755); err != nil {
			return nil, err
		}
		if err := store.WriteJSONAtomic(r.store.CheckpointPath(taskID, i), &meta.Shards[i]); err != nil {
			return nil, err
		}
	}
	if err := store.WriteJSONAtomic(r.store.TaskMetaPath(taskID), meta); err != nil {
		return nil, err
	}
	return meta, nil
}

// RunRatingTask 执行完整任务：ingest -> 分片批价（续跑）-> 全局汇总。
func (r *Runner) RunRatingTask(taskID string) (*model.TaskMeta, error) {
	meta := &model.TaskMeta{}
	if err := store.ReadJSON(r.store.TaskMetaPath(taskID), meta); err != nil {
		return nil, err
	}
	meta.Status = StatusRunning
	meta.UpdatedAt = time.Now().UTC().Format(model.TimeLayout)
	_ = store.WriteJSONAtomic(r.store.TaskMetaPath(taskID), meta)

	// 阶段 1：接入（仅当没有任何分片 raw 时执行，避免重复追加）
	if !r.ingestDone(meta) {
		stage, err := r.runIngest(meta)
		if err != nil {
			r.markFailed(meta, err)
			return meta, err
		}
		meta.Stages = append(meta.Stages, stage)
		_ = store.WriteJSONAtomic(r.store.TaskMetaPath(taskID), meta)
	}

	// 阶段 2：分片并行批价（DONE 跳过，其余重跑）
	cat, err := tariff.Load(r.store)
	if err != nil {
		r.markFailed(meta, err)
		return meta, err
	}
	stageStart := time.Now()
	stage := model.StageStat{Name: "rate_shards", StartedAt: stageStart.UTC().Format(model.TimeLayout)}

	sem := make(chan struct{}, r.parallel)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	var inputTotal, ratedTotal, dupTotal, exTotal int64

	for i := 0; i < meta.ShardCount; i++ {
		if meta.Shards[i].Status == StatusDone {
			inputTotal += meta.Shards[i].InputCount
			ratedTotal += meta.Shards[i].RatedCount
			dupTotal += meta.Shards[i].DupCount
			exTotal += meta.Shards[i].ExceptionCount
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(sh int) {
			defer wg.Done()
			defer func() { <-sem }()
			cp := meta.Shards[sh]
			cp.Status = StatusRunning
			cp.Attempts++
			cp.UpdatedAt = time.Now().UTC().Format(model.TimeLayout)
			_ = store.WriteJSONAtomic(r.store.CheckpointPath(taskID, sh), &cp)

			res, err := pipeline.ProcessShard(r.store, cat, taskID, sh, meta.ShardCount)
			if err != nil {
				cp.Status = StatusFailed
				cp.Error = err.Error()
				cp.UpdatedAt = time.Now().UTC().Format(model.TimeLayout)
				_ = store.WriteJSONAtomic(r.store.CheckpointPath(taskID, sh), &cp)
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("shard %d: %w", sh, err)
				}
				mu.Unlock()
				return
			}
			cp.Status = StatusDone
			cp.Error = ""
			cp.InputCount = res.Input
			cp.RatedCount = res.Rated
			cp.DupCount = res.Dup
			cp.ExceptionCount = res.Exception
			cp.UpdatedAt = time.Now().UTC().Format(model.TimeLayout)
			_ = store.WriteJSONAtomic(r.store.CheckpointPath(taskID, sh), &cp)
			mu.Lock()
			meta.Shards[sh] = cp
			inputTotal += res.Input
			ratedTotal += res.Rated
			dupTotal += res.Dup
			exTotal += res.Exception
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	stage.EndedAt = time.Now().UTC().Format(model.TimeLayout)
	stage.ElapsedMs = time.Since(stageStart).Milliseconds()
	stage.InputCount = inputTotal
	stage.OutputCount = ratedTotal
	stage.ErrorCount = exTotal
	meta.Stages = append(meta.Stages, stage)

	if firstErr != nil {
		r.markFailed(meta, firstErr)
		return meta, firstErr
	}

	// 阶段 3：全局汇总
	aggStage, err := r.runAggregate(meta)
	if err != nil {
		r.markFailed(meta, err)
		return meta, err
	}
	meta.Stages = append(meta.Stages, aggStage)

	meta.Status = StatusDone
	meta.UpdatedAt = time.Now().UTC().Format(model.TimeLayout)
	if err := store.WriteJSONAtomic(r.store.TaskMetaPath(taskID), meta); err != nil {
		return meta, err
	}
	return meta, nil
}

func (r *Runner) ingestDone(meta *model.TaskMeta) bool {
	for i := 0; i < meta.ShardCount; i++ {
		if _, err := os.Stat(r.store.RawPath(meta.TaskID, i)); err == nil {
			return true
		}
	}
	return false
}

func (r *Runner) runIngest(meta *model.TaskMeta) (model.StageStat, error) {
	start := time.Now()
	stage := model.StageStat{Name: "ingest", StartedAt: start.UTC().Format(model.TimeLayout)}
	stats := &pipeline.IngestStats{ShardCounts: map[int]int64{}}
	for _, p := range meta.InputFiles {
		if err := pipeline.IngestFile(r.store, meta.TaskID, meta.ShardCount, p, stats); err != nil {
			return stage, err
		}
		stats.Files++
	}
	stage.EndedAt = time.Now().UTC().Format(model.TimeLayout)
	stage.ElapsedMs = time.Since(start).Milliseconds()
	stage.InputCount = stats.Lines
	stage.OutputCount = stats.Decoded
	stage.ErrorCount = stats.Garbled
	return stage, nil
}

// runAggregate 汇总各分片 summary -> global_summary.jsonl，并做引擎内两级对平。
// 单遍扫描：每分片同时累加 rated 明细与 summary，避免重复全量 I/O。
func (r *Runner) runAggregate(meta *model.TaskMeta) (model.StageStat, error) {
	start := time.Now()
	stage := model.StageStat{Name: "aggregate", StartedAt: start.UTC().Format(model.TimeLayout)}

	gw, err := store.CreateJSONL(r.store.GlobalSummaryPath(meta.TaskID))
	if err != nil {
		return stage, err
	}
	monthTotals := map[string]int64{}
	var rows, ratedSum, ratedRows int64

	// 先写全局汇总并累计 summary 侧
	for sh := 0; sh < meta.ShardCount; sh++ {
		err := store.ReadJSONLInto(r.store.SummaryPath(meta.TaskID, sh), func(s *model.UserSummary) error {
			if e := gw.Write(s); e != nil {
				return e
			}
			monthTotals[s.BillMonth] += s.TotalFee
			rows++
			return nil
		})
		if err != nil {
			_ = gw.Close()
			return stage, err
		}
	}
	if err := gw.Close(); err != nil {
		return stage, err
	}

	// 并行对平扫描：各分片 rated 明细求和（CPU 轻、I/O 重，适合并发）
	sem := make(chan struct{}, r.parallel)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var scanErr error
	for sh := 0; sh < meta.ShardCount; sh++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(sh int) {
			defer wg.Done()
			defer func() { <-sem }()
			var sum, n int64
			if e := store.ReadJSONLInto(r.store.RatedPath(meta.TaskID, sh), func(c *model.RatedCDR) error {
				sum += c.TotalFee
				n++
				return nil
			}); e != nil {
				mu.Lock()
				if scanErr == nil {
					scanErr = e
				}
				mu.Unlock()
				return
			}
			mu.Lock()
			ratedSum += sum
			ratedRows += n
			mu.Unlock()
		}(sh)
	}
	wg.Wait()
	if scanErr != nil {
		return stage, scanErr
	}
	var summarySum int64
	months := make([]string, 0, len(monthTotals))
	for m, v := range monthTotals {
		months = append(months, m)
		summarySum += v
	}
	sort.Strings(months)
	recon := map[string]any{
		"rated_fee_sum":   ratedSum,
		"summary_fee_sum": summarySum,
		"balanced":        ratedSum == summarySum,
		"rated_rows":      ratedRows,
		"summary_rows":    rows,
		"month_totals":    monthTotals,
		"months":          months,
	}
	if err := store.WriteJSONAtomic(r.store.ReconcilePath(meta.TaskID), recon); err != nil {
		return stage, err
	}
	if ratedSum != summarySum {
		return stage, fmt.Errorf("internal reconcile failed: rated %d != summary %d", ratedSum, summarySum)
	}

	stage.EndedAt = time.Now().UTC().Format(model.TimeLayout)
	stage.ElapsedMs = time.Since(start).Milliseconds()
	stage.InputCount = ratedRows
	stage.OutputCount = rows
	return stage, nil
}

func (r *Runner) markFailed(meta *model.TaskMeta, err error) {
	meta.Status = StatusFailed
	meta.UpdatedAt = time.Now().UTC().Format(model.TimeLayout)
	_ = store.WriteJSONAtomic(r.store.TaskMetaPath(meta.TaskID), meta)
	_ = os.WriteFile(filepath.Join(r.store.TaskDir(meta.TaskID), "FAILED"), []byte(err.Error()+"\n"), 0o644)
}

// ListTasks 读取全部任务元数据（新的在前）。
func (r *Runner) ListTasks() ([]*model.TaskMeta, error) {
	ids, err := r.store.ListTaskIDs()
	if err != nil {
		return nil, err
	}
	var out []*model.TaskMeta
	for _, id := range ids {
		meta := &model.TaskMeta{}
		if err := store.ReadJSON(r.store.TaskMetaPath(id), meta); err != nil {
			continue // 半成品目录跳过
		}
		out = append(out, meta)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out, nil
}
