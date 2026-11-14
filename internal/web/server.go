// Package web 提供运营页面与 REST API（静态资源 embed，单二进制部署）。
package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"sort"
	"strings"
	"time"

	"ratingengine/internal/model"
	"ratingengine/internal/reconcile"
	"ratingengine/internal/store"
	"ratingengine/internal/tariff"
	"ratingengine/internal/task"
)

//go:embed static
var staticFS embed.FS

// Server 运营页面服务。
type Server struct {
	store *store.Store
	mux   *http.ServeMux
}

// Serve 启动 HTTP 服务（阻塞）。
func Serve(st *store.Store, addr string) error {
	s := &Server{store: st, mux: http.NewServeMux()}
	s.routes()
	fmt.Printf("运营页面监听 http://localhost%s （数据目录 %s）\n", addr, st.Root)
	return http.ListenAndServe(addr, s.mux)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func writeErr(w http.ResponseWriter, code int, format string, a ...any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf(format, a...)})
}

func (s *Server) routes() {
	m := s.mux
	m.HandleFunc("GET /api/overview", s.handleOverview)
	m.HandleFunc("GET /api/tariffs", s.handleTariffs)
	m.HandleFunc("GET /api/tariffs/{plan}", s.handleTariffDetail)
	m.HandleFunc("GET /api/subscriptions", s.handleSubscriptions)
	m.HandleFunc("GET /api/inbox", s.handleInbox)
	m.HandleFunc("GET /api/tasks", s.handleTasks)
	m.HandleFunc("GET /api/tasks/{id}", s.handleTaskDetail)
	m.HandleFunc("GET /api/tasks/{id}/shards", s.handleShards)
	m.HandleFunc("GET /api/tasks/{id}/exceptions", s.handleExceptions)
	m.HandleFunc("GET /api/tasks/{id}/deltas", s.handleDeltas)
	m.HandleFunc("GET /api/tasks/{id}/reconcile", s.handleReconcile)
	m.HandleFunc("POST /api/tasks", s.handleCreateTask)
	m.HandleFunc("POST /api/tasks/{id}/retry", s.handleRetry)
	m.HandleFunc("POST /api/recompute", s.handleRecompute)
	m.HandleFunc("POST /api/reconcile/{id}", s.handleRunReconcile)

	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	m.Handle("GET /", http.FileServer(http.FS(sub)))
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	tasks, _ := task.NewRunner(s.store, 0, 0).ListTasks()
	exByReason := map[string]int64{}
	var totalEx, totalDup, totalRated int64
	for _, t := range tasks {
		if t.Kind != "rating" {
			continue
		}
		for _, sh := range t.Shards {
			totalDup += sh.DupCount
			totalEx += sh.ExceptionCount
			totalRated += sh.RatedCount
		}
		// 异常原因明细取最新 rating 任务
	}
	// 取最新任务的异常原因
	if len(tasks) > 0 {
		latest := tasks[0]
		for sh := 0; sh < latest.ShardCount; sh++ {
			_ = store.ReadJSONLInto(s.store.ExceptionPath(latest.TaskID, sh), func(e *model.ExceptionCDR) error {
				exByReason[e.Reason]++
				return nil
			})
		}
	}
	writeJSON(w, map[string]any{
		"task_count":          len(tasks),
		"latest_rated_cdrs":   totalRated,
		"duplicate_cdrs":      totalDup,
		"exception_cdrs":      totalEx,
		"exception_by_reason": exByReason,
		"server_time":         time.Now().UTC().Format(model.TimeLayout),
	})
}

func (s *Server) handleTariffs(w http.ResponseWriter, r *http.Request) {
	cat, err := loadCatalog(s.store)
	if err != nil {
		writeErr(w, 500, "%v", err)
		return
	}
	type brief struct {
		PlanID         string   `json:"plan_id"`
		Version        string   `json:"version"`
		EffectiveStart string   `json:"effective_start"`
		EffectiveEnd   string   `json:"effective_end"`
		Services       []string `json:"services"`
	}
	var out []brief
	for _, t := range cat.AllTariffs() {
		svcs := make([]string, 0, len(t.Services))
		for k := range t.Services {
			svcs = append(svcs, k)
		}
		sort.Strings(svcs)
		out = append(out, brief{
			t.PlanID, t.Version,
			t.EffectiveStart.Format(model.TimeLayout), t.EffectiveEnd.Format(model.TimeLayout), svcs,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].PlanID != out[j].PlanID {
			return out[i].PlanID < out[j].PlanID
		}
		return out[i].Version < out[j].Version
	})
	writeJSON(w, out)
}

func (s *Server) handleTariffDetail(w http.ResponseWriter, r *http.Request) {
	plan := r.PathValue("plan")
	cat, err := loadCatalog(s.store)
	if err != nil {
		writeErr(w, 500, "%v", err)
		return
	}
	var versions []*model.Tariff
	for _, t := range cat.AllTariffs() {
		if t.PlanID == plan {
			versions = append(versions, t)
		}
	}
	if len(versions) == 0 {
		writeErr(w, 404, "plan %s not found", plan)
		return
	}
	writeJSON(w, versions)
}

func (s *Server) handleSubscriptions(w http.ResponseWriter, r *http.Request) {
	var subs []*model.Subscription
	if err := store.ReadJSONLInto(s.store.SubFile(), func(sub *model.Subscription) error {
		subs = append(subs, sub)
		return nil
	}); err != nil {
		writeErr(w, 500, "%v", err)
		return
	}
	userFilter := r.URL.Query().Get("user")
	var out []*model.Subscription
	for _, sub := range subs {
		if userFilter != "" && sub.UserID != userFilter {
			continue
		}
		out = append(out, sub)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].UserID != out[j].UserID {
			return out[i].UserID < out[j].UserID
		}
		if out[i].Service != out[j].Service {
			return out[i].Service < out[j].Service
		}
		return out[i].EffectiveStart.Before(out[j].EffectiveStart)
	})
	writeJSON(w, out)
}

func (s *Server) handleInbox(w http.ResponseWriter, r *http.Request) {
	type fileInfo struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	}
	entries, err := readDirNames(s.store.InboxDir())
	if err != nil {
		writeErr(w, 500, "%v", err)
		return
	}
	var out []fileInfo
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, fileInfo{Name: e.Name(), Size: info.Size()})
	}
	writeJSON(w, out)
}

func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	tasks, err := task.NewRunner(s.store, 0, 0).ListTasks()
	if err != nil {
		writeErr(w, 500, "%v", err)
		return
	}
	writeJSON(w, tasks)
}

func (s *Server) handleTaskDetail(w http.ResponseWriter, r *http.Request) {
	meta, ok := s.loadTask(w, r)
	if !ok {
		return
	}
	// 附带对平信息
	rec := map[string]any{}
	_ = store.ReadJSON(s.store.ReconcilePath(meta.TaskID), &rec)
	writeJSON(w, map[string]any{"task": meta, "reconcile": rec})
}

func (s *Server) handleShards(w http.ResponseWriter, r *http.Request) {
	meta, ok := s.loadTask(w, r)
	if !ok {
		return
	}
	writeJSON(w, meta.Shards)
}

func (s *Server) handleExceptions(w http.ResponseWriter, r *http.Request) {
	meta, ok := s.loadTask(w, r)
	if !ok {
		return
	}
	reason := r.URL.Query().Get("reason")
	limit := 200
	var out []*model.ExceptionCDR
	for sh := 0; sh < meta.ShardCount && len(out) < limit; sh++ {
		_ = store.ReadJSONLInto(s.store.ExceptionPath(meta.TaskID, sh), func(e *model.ExceptionCDR) error {
			if len(out) >= limit {
				return nil
			}
			if reason != "" && e.Reason != reason {
				return nil
			}
			out = append(out, e)
			return nil
		})
	}
	writeJSON(w, out)
}

func (s *Server) handleDeltas(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var out []reconcile.Delta
	if err := store.ReadJSONLInto(s.store.DeltaPath(id), func(d *reconcile.Delta) error {
		out = append(out, *d)
		return nil
	}); err != nil {
		writeErr(w, 500, "%v", err)
		return
	}
	writeJSON(w, out)
}

func (s *Server) handleReconcile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rep := &reconcile.Report{}
	if err := store.ReadJSON(s.store.Root+"/tasks/"+id+"/oracle_reconcile.json", rep); err != nil {
		writeErr(w, 404, "尚未执行 oracle 对账: %v", err)
		return
	}
	writeJSON(w, rep)
}

type createReq struct {
	Name     string `json:"name"`
	Shards   int    `json:"shards"`
	Parallel int    `json:"parallel"`
}

func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "bad json: %v", err)
		return
	}
	if req.Shards <= 0 {
		req.Shards = 64
	}
	files, err := collectInboxFiles(s.store)
	if err != nil {
		writeErr(w, 500, "%v", err)
		return
	}
	if len(files) == 0 {
		writeErr(w, 400, "inbox 无话单文件")
		return
	}
	id := req.Name
	if id == "" {
		id = "rate-" + time.Now().UTC().Format("20060102T150405Z")
	}
	runner := task.NewRunner(s.store, req.Shards, req.Parallel)
	meta, err := runner.CreateRatingTask(id, "rating", files, map[string]string{"shards": fmt.Sprint(req.Shards)})
	if err != nil {
		writeErr(w, 500, "%v", err)
		return
	}
	go func() { _, _ = runner.RunRatingTask(id) }()
	writeJSON(w, meta)
}

func (s *Server) handleRetry(w http.ResponseWriter, r *http.Request) {
	meta, ok := s.loadTask(w, r)
	if !ok {
		return
	}
	runner := task.NewRunner(s.store, 0, 0)
	go func() { _, _ = runner.RunRatingTask(meta.TaskID) }()
	writeJSON(w, map[string]string{"status": "started", "task": meta.TaskID})
}

type recomputeReq struct {
	OldID    string `json:"old_task"`
	Start    string `json:"start"`
	End      string `json:"end"`
	Name     string `json:"name"`
	Shards   int    `json:"shards"`
	Parallel int    `json:"parallel"`
}

func (s *Server) handleRecompute(w http.ResponseWriter, r *http.Request) {
	var req recomputeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "bad json: %v", err)
		return
	}
	if _, err := reconcile.MonthWindow(req.Start, req.End); err != nil {
		writeErr(w, 400, "%v", err)
		return
	}
	oldMeta := &model.TaskMeta{}
	if err := store.ReadJSON(s.store.TaskMetaPath(req.OldID), oldMeta); err != nil {
		writeErr(w, 404, "old task not found: %v", err)
		return
	}
	id := req.Name
	if id == "" {
		id = "recompute-" + time.Now().UTC().Format("20060102T150405Z")
	}
	if req.Shards <= 0 {
		req.Shards = oldMeta.ShardCount
	}
	runner := task.NewRunner(s.store, req.Shards, req.Parallel)
	if _, err := runner.CreateRatingTask(id, "recompute", oldMeta.InputFiles, map[string]string{
		"old_task": req.OldID, "window_start": req.Start, "window_end": req.End,
	}); err != nil {
		writeErr(w, 500, "%v", err)
		return
	}
	go func() {
		meta, err := runner.RunRatingTask(id)
		if err != nil || meta == nil {
			return
		}
		months, _ := reconcile.MonthWindow(req.Start, req.End)
		deltas, err := reconcile.DiffTasks(s.store, req.OldID, id, meta.ShardCount, months)
		if err != nil {
			return
		}
		_, _ = reconcile.SaveDeltas(s.store, id, deltas)
	}()
	writeJSON(w, map[string]string{"status": "started", "task": id})
}

func (s *Server) handleRunReconcile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	meta := &model.TaskMeta{}
	if err := store.ReadJSON(s.store.TaskMetaPath(id), meta); err != nil {
		writeErr(w, 404, "task not found: %v", err)
		return
	}
	rep, err := reconcile.CompareWithOracle(s.store, id, meta.ShardCount)
	if err != nil {
		writeErr(w, 500, "%v", err)
		return
	}
	if err := store.WriteJSONAtomic(s.store.Root+"/tasks/"+id+"/oracle_reconcile.json", rep); err != nil {
		writeErr(w, 500, "%v", err)
		return
	}
	writeJSON(w, rep)
}

func (s *Server) loadTask(w http.ResponseWriter, r *http.Request) (*model.TaskMeta, bool) {
	id := r.PathValue("id")
	meta := &model.TaskMeta{}
	if err := store.ReadJSON(s.store.TaskMetaPath(id), meta); err != nil {
		writeErr(w, 404, "task %s not found", id)
		return nil, false
	}
	return meta, true
}

func loadCatalog(st *store.Store) (*tariff.Catalog, error) {
	cat, err := tariffLoad(st)
	if err != nil {
		return nil, err
	}
	return cat, nil
}

func collectInboxFiles(st *store.Store) ([]string, error) {
	entries, err := readDirNames(st.InboxDir())
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".fixed") || strings.HasSuffix(name, ".delim") ||
			strings.HasSuffix(name, ".asn1") || strings.HasSuffix(name, ".lpbin") {
			files = append(files, st.InboxDir()+"/"+name)
		}
	}
	sort.Strings(files)
	return files, nil
}
