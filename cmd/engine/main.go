// 批价引擎命令行入口。
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"ratingengine/internal/model"
	"ratingengine/internal/reconcile"
	"ratingengine/internal/store"
	"ratingengine/internal/task"
	"ratingengine/internal/web"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	var err error
	switch cmd {
	case "run":
		err = cmdRun(args)
	case "retry":
		err = cmdRetry(args)
	case "recompute":
		err = cmdRecompute(args)
	case "list":
		err = cmdList(args)
	case "show":
		err = cmdShow(args)
	case "reconcile":
		err = cmdReconcile(args)
	case "exceptions":
		err = cmdExceptions(args)
	case "serve":
		err = cmdServe(args)
	case "version":
		fmt.Println("ratingengine 1.0.0")
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `ratingengine — 虚拟运营商离线批价引擎

用法:
  engine run --data DIR --shards N --parallel M [--name ID]
        执行批价任务（接入 + 分片并行批价 + 全局汇总对平）
  engine retry --data DIR --task ID
        从失败分片续跑（DONE 分片跳过）
  engine recompute --data DIR --old ID --start RFC3339 --end RFC3339 [--name ID]
        按当前资费重算指定时间窗，输出用户差额
  engine list --data DIR
        列出全部任务及状态
  engine show --data DIR --task ID
        查看任务详情、分片状态与阶段耗时
  engine reconcile --data DIR --task ID
        与 oracle 独立应收逐用户对账
  engine exceptions --data DIR --task ID [--reason E1] [--limit N]
        查看异常话单
  engine serve --data DIR [--addr :8080]
        启动运营页面
`)
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", a...)
	os.Exit(2)
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	data := fs.String("data", "", "数据根目录")
	shards := fs.Int("shards", 64, "分片数")
	parallel := fs.Int("parallel", 0, "并行度（默认 CPU 核数）")
	name := fs.String("name", "", "任务 ID（默认自动生成）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *data == "" {
		fail("--data is required")
	}
	st := store.New(*data)
	if err := st.EnsureLayout(); err != nil {
		return err
	}
	files, err := collectCDRFiles(st.InboxDir())
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("inbox 目录无话单文件: %s", st.InboxDir())
	}
	taskID := *name
	if taskID == "" {
		taskID = "rate-" + time.Now().UTC().Format("20060102T150405Z")
	}
	runner := task.NewRunner(st, *shards, *parallel)
	meta, err := runner.CreateRatingTask(taskID, "rating", files, map[string]string{
		"shards": fmt.Sprint(*shards),
	})
	if err != nil {
		return err
	}
	meta, err = runner.RunRatingTask(taskID)
	printTaskBrief(meta)
	return err
}

func cmdRetry(args []string) error {
	fs := flag.NewFlagSet("retry", flag.ExitOnError)
	data := fs.String("data", "", "数据根目录")
	taskID := fs.String("task", "", "任务 ID")
	parallel := fs.Int("parallel", 0, "并行度")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *taskID == "" {
		fail("--task is required")
	}
	st := store.New(*data)
	runner := task.NewRunner(st, 0, *parallel)
	meta, err := runner.RunRatingTask(*taskID)
	printTaskBrief(meta)
	return err
}

func cmdRecompute(args []string) error {
	fs := flag.NewFlagSet("recompute", flag.ExitOnError)
	data := fs.String("data", "", "数据根目录")
	oldID := fs.String("old", "", "原任务 ID")
	start := fs.String("start", "", "重算窗口起 RFC3339")
	end := fs.String("end", "", "重算窗口止 RFC3339")
	name := fs.String("name", "", "新任务 ID")
	shards := fs.Int("shards", 0, "分片数（默认沿用原任务）")
	parallel := fs.Int("parallel", 0, "并行度")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *data == "" || *oldID == "" || *start == "" || *end == "" {
		fail("--data --old --start --end are required")
	}
	st := store.New(*data)
	oldMeta, err := loadMeta(st, *oldID)
	if err != nil {
		return err
	}
	if *shards <= 0 {
		*shards = oldMeta.ShardCount
	}
	months, err := reconcile.MonthWindow(*start, *end)
	if err != nil {
		return err
	}
	taskID := *name
	if taskID == "" {
		taskID = "recompute-" + time.Now().UTC().Format("20060102T150405Z")
	}
	runner := task.NewRunner(st, *shards, *parallel)
	meta, err := runner.CreateRatingTask(taskID, "recompute", oldMeta.InputFiles, map[string]string{
		"old_task": *oldID, "window_start": *start, "window_end": *end,
		"shards": fmt.Sprint(*shards),
	})
	if err != nil {
		return err
	}
	meta, err = runner.RunRatingTask(taskID)
	printTaskBrief(meta)
	if err != nil {
		return err
	}
	deltas, err := reconcile.DiffTasks(st, *oldID, taskID, meta.ShardCount, months)
	if err != nil {
		return err
	}
	total, err := reconcile.SaveDeltas(st, taskID, deltas)
	if err != nil {
		return err
	}
	fmt.Printf("\n重算窗口 %s ~ %s（账期月 %d 个）\n", *start, *end, len(months))
	fmt.Printf("差额用户行: %d，差额总额: %d 厘（%.2f 元）\n", len(deltas), total, float64(total)/100000)
	return nil
}

func cmdList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	data := fs.String("data", "", "数据根目录")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *data == "" {
		fail("--data is required")
	}
	st := store.New(*data)
	tasks, err := task.NewRunner(st, 0, 0).ListTasks()
	if err != nil {
		return err
	}
	fmt.Printf("%-28s %-10s %-8s %-9s %s\n", "TASK_ID", "KIND", "STATUS", "SHARDS", "CREATED")
	for _, t := range tasks {
		done := 0
		for _, s := range t.Shards {
			if s.Status == task.StatusDone {
				done++
			}
		}
		fmt.Printf("%-28s %-10s %-8s %-9s %s\n", t.TaskID, t.Kind, t.Status,
			fmt.Sprintf("%d/%d", done, t.ShardCount), t.CreatedAt)
	}
	return nil
}

func cmdShow(args []string) error {
	fs := flag.NewFlagSet("show", flag.ExitOnError)
	data := fs.String("data", "", "数据根目录")
	taskID := fs.String("task", "", "任务 ID")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *taskID == "" {
		fail("--task is required")
	}
	st := store.New(*data)
	meta, err := loadMeta(st, *taskID)
	if err != nil {
		return err
	}
	printTaskBrief(meta)
	byStatus := map[string]int{}
	for _, s := range meta.Shards {
		byStatus[s.Status]++
	}
	fmt.Println("\n分片状态:")
	for _, k := range []string{task.StatusPending, task.StatusRunning, task.StatusDone, task.StatusFailed} {
		if byStatus[k] > 0 {
			fmt.Printf("  %-8s %d\n", k, byStatus[k])
		}
	}
	fmt.Println("\n阶段耗时:")
	for _, s := range meta.Stages {
		fmt.Printf("  %-12s %8d ms   in=%-10d out=%-10d err=%d\n",
			s.Name, s.ElapsedMs, s.InputCount, s.OutputCount, s.ErrorCount)
	}
	if _, err := os.Stat(st.ReconcilePath(*taskID)); err == nil {
		fmt.Println("\n对平文件:", st.ReconcilePath(*taskID))
	}
	return nil
}

func cmdReconcile(args []string) error {
	fs := flag.NewFlagSet("reconcile", flag.ExitOnError)
	data := fs.String("data", "", "数据根目录")
	taskID := fs.String("task", "", "任务 ID")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *taskID == "" {
		fail("--task is required")
	}
	st := store.New(*data)
	meta, err := loadMeta(st, *taskID)
	if err != nil {
		return err
	}
	rep, err := reconcile.CompareWithOracle(st, *taskID, meta.ShardCount)
	if err != nil {
		return err
	}
	outPath := filepath.Join(st.TaskDir(*taskID), "oracle_reconcile.json")
	if err := store.WriteJSONAtomic(outPath, rep); err != nil {
		return err
	}
	fmt.Printf("引擎行 %d  oracle 行 %d  匹配 %d  差异 %d\n",
		rep.EngineRows, rep.OracleRows, rep.MatchedRows, rep.MismatchRows)
	fmt.Printf("引擎总额 %d  oracle 总额 %d  对平=%v\n",
		rep.EngineTotalFee, rep.OracleTotalFee, rep.Balanced)
	fmt.Println("报告:", outPath)
	if !rep.Balanced {
		limit := 20
		fmt.Printf("\n前 %d 条差异明细:\n", limit)
		for i, m := range rep.Mismatches {
			if i >= limit {
				break
			}
			fmt.Printf("  %s %s kind=%s engine=%d oracle=%d diff=%d\n",
				m.UserID, m.BillMonth, m.Kind, m.Engine, m.Oracle, m.Diff)
		}
	}
	return nil
}

func cmdExceptions(args []string) error {
	fs := flag.NewFlagSet("exceptions", flag.ExitOnError)
	data := fs.String("data", "", "数据根目录")
	taskID := fs.String("task", "", "任务 ID")
	reason := fs.String("reason", "", "只看某类异常")
	limit := fs.Int("limit", 50, "最多输出条数")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *taskID == "" {
		fail("--task is required")
	}
	st := store.New(*data)
	meta, err := loadMeta(st, *taskID)
	if err != nil {
		return err
	}
	printed := 0
	for sh := 0; sh < meta.ShardCount && printed < *limit; sh++ {
		_ = store.ReadJSONLInto(st.ExceptionPath(*taskID, sh), func(e *model.ExceptionCDR) error {
			if printed >= *limit {
				return nil
			}
			if *reason != "" && e.Reason != *reason {
				return nil
			}
			fmt.Printf("[shard%02d] %-18s %-12s %s detail=%s\n",
				e.Shard, e.Reason, e.UserID, e.RecordID, e.Detail)
			printed++
			return nil
		})
	}
	fmt.Printf("\n共输出 %d 条（--limit 控制上限）\n", printed)
	return nil
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	data := fs.String("data", "", "数据根目录")
	addr := fs.String("addr", ":8080", "监听地址")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *data == "" {
		fail("--data is required")
	}
	st := store.New(*data)
	if err := st.EnsureLayout(); err != nil {
		return err
	}
	return web.Serve(st, *addr)
}

// ---- 辅助 ----

func collectCDRFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if ext == ".fixed" || ext == ".delim" || ext == ".asn1" || ext == ".lpbin" {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(files)
	return files, nil
}

func loadMeta(st *store.Store, id string) (*model.TaskMeta, error) {
	m := &model.TaskMeta{}
	if err := store.ReadJSON(st.TaskMetaPath(id), m); err != nil {
		return nil, fmt.Errorf("load task %s: %w", id, err)
	}
	return m, nil
}

func printTaskBrief(m *model.TaskMeta) {
	if m == nil {
		return
	}
	fmt.Printf("任务 %s (%s) 状态=%s 分片=%d\n", m.TaskID, m.Kind, m.Status, m.ShardCount)
}
