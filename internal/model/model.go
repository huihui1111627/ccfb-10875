// Package model 定义批价引擎的领域对象与存储结构。
package model

import "time"

// 业务类型
const (
	ServiceVoice = "voice"
	ServiceSMS   = "sms"
	ServiceData  = "data"
)

// 话单来源格式
const (
	SourceFixed     = "fixed"
	SourceDelimited = "delimited"
	SourceASN1      = "asn1"
	SourceLPBin     = "lpbin"
)

// 异常原因
const (
	ErrBadFormat      = "E1_BAD_FORMAT"
	ErrBadTime        = "E2_BAD_TIME"
	ErrBadQuantity    = "E3_BAD_QUANTITY"
	ErrNoSubscription = "E4_NO_SUBSCRIPTION"
	ErrBadSegment     = "E5_BAD_SEGMENT"
)

// TimeLayout 全系统统一 UTC RFC3339 秒精度。
const TimeLayout = time.RFC3339

// RawCDR 解码后的原始话单（去重、合并前）。
type RawCDR struct {
	RecordID   string    `json:"record_id"`
	SourceType string    `json:"source_type"`
	UserID     string    `json:"user_id"`
	Service    string    `json:"service"`
	StartTime  time.Time `json:"start_time"`
	EndTime    time.Time `json:"end_time"`
	Quantity   int64     `json:"quantity"`
	Roaming    bool      `json:"roaming"`
	CallRef    string    `json:"call_ref"`
	SegNo      int       `json:"seg_no"`
	SegTotal   int       `json:"seg_total"`
	// RawLine 原始行摘要，异常留痕用
	RawLine string `json:"raw_line,omitempty"`
}

// ExceptionCDR 异常话单。
type ExceptionCDR struct {
	RecordID   string `json:"record_id,omitempty"`
	UserID     string `json:"user_id,omitempty"`
	Shard      int    `json:"shard"`
	Reason     string `json:"reason"`
	Detail     string `json:"detail"`
	RawSnippet string `json:"raw_snippet"`
	Stage      string `json:"stage"`
	OccurredAt string `json:"occurred_at"`
}

// Fragment 一条话单按时间切分后的批价片段。
type Fragment struct {
	StartTime time.Time `json:"start_time"`
	EndTime   time.Time `json:"end_time"`
	BillMonth string    `json:"bill_month"` // YYYY-MM（UTC）
	PlanID    string    `json:"plan_id"`
	TariffVer string    `json:"tariff_version"`
	Units     int64     `json:"units"`      // 批价单位数
	FreeUnits int64     `json:"free_units"` // 免费额度抵扣
	BillUnits int64     `json:"bill_units"` // 计费单位数
	Night     bool      `json:"night"`
	Roaming   bool      `json:"roaming"`
	Fee       int64     `json:"fee"` // 厘
}

// RatedCDR 批价结果话单。
type RatedCDR struct {
	RecordID  string     `json:"record_id"`
	UserID    string     `json:"user_id"`
	Service   string     `json:"service"`
	Shard     int        `json:"shard"`
	StartTime time.Time  `json:"start_time"`
	EndTime   time.Time  `json:"end_time"`
	Quantity  int64      `json:"quantity"`
	Roaming   bool       `json:"roaming"`
	CallRef   string     `json:"call_ref,omitempty"`
	Merged    bool       `json:"merged"`
	MergedIDs []string   `json:"merged_ids,omitempty"`
	Fragments []Fragment `json:"fragments"`
	TotalFee  int64      `json:"total_fee"`
}

// UserSummary 用户×账期月汇总。
type UserSummary struct {
	UserID       string           `json:"user_id"`
	BillMonth    string           `json:"bill_month"`
	Shard        int              `json:"shard"`
	ValidCount   int64            `json:"valid_count"`
	ServiceUnits map[string]int64 `json:"service_units"`
	TotalFee     int64            `json:"total_fee"`
}

// MergeRecord 拆段合并明细。
type MergeRecord struct {
	CallRef   string   `json:"call_ref"`
	UserID    string   `json:"user_id"`
	KeptID    string   `json:"kept_id"`
	MergedIDs []string `json:"merged_ids"`
	SegTotal  int      `json:"seg_total"`
}

// StageStat 阶段耗时与流量统计。
type StageStat struct {
	Name        string `json:"name"`
	StartedAt   string `json:"started_at"`
	EndedAt     string `json:"ended_at"`
	ElapsedMs   int64  `json:"elapsed_ms"`
	InputCount  int64  `json:"input_count"`
	OutputCount int64  `json:"output_count"`
	ErrorCount  int64  `json:"error_count"`
}

// ShardCheckpoint 分片断点。
type ShardCheckpoint struct {
	Shard          int    `json:"shard"`
	Status         string `json:"status"` // PENDING/RUNNING/DONE/FAILED
	Attempts       int    `json:"attempts"`
	UpdatedAt      string `json:"updated_at"`
	Error          string `json:"error,omitempty"`
	InputCount     int64  `json:"input_count"`
	RatedCount     int64  `json:"rated_count"`
	DupCount       int64  `json:"dup_count"`
	ExceptionCount int64  `json:"exception_count"`
}

// TaskMeta 任务元数据。
type TaskMeta struct {
	TaskID     string            `json:"task_id"`
	Kind       string            `json:"kind"` // rating / recompute
	ShardCount int               `json:"shard_count"`
	Status     string            `json:"status"`
	CreatedAt  string            `json:"created_at"`
	UpdatedAt  string            `json:"updated_at"`
	Params     map[string]string `json:"params"`
	Stages     []StageStat       `json:"stages"`
	Shards     []ShardCheckpoint `json:"shards"`
	InputFiles []string          `json:"input_files"`
}

// Tier 阶梯。Upto=-1 表示无穷档；Rate 单位为厘/批价单位。
type Tier struct {
	Upto int64 `json:"upto"`
	Rate int64 `json:"rate"`
}

// ServiceTariff 单业务资费。
type ServiceTariff struct {
	UnitSize      int64    `json:"unit_size"`
	Rounding      string   `json:"rounding"` // up
	Allowance     int64    `json:"allowance_units"`
	Tiers         []Tier   `json:"tiers"`
	NightWindow   []string `json:"night_window,omitempty"` // ["23:00","07:00"]
	NightFactor   int64    `json:"night_factor,omitempty"` // 基点 10000
	RoamingFactor int64    `json:"roaming_factor,omitempty"`
}

// Tariff 资费计划版本。
type Tariff struct {
	PlanID         string                    `json:"plan_id"`
	Version        string                    `json:"version"`
	EffectiveStart time.Time                 `json:"effective_start"`
	EffectiveEnd   time.Time                 `json:"effective_end"`
	Services       map[string]*ServiceTariff `json:"services"`
}

// Subscription 用户订阅区间。
type Subscription struct {
	UserID         string    `json:"user_id"`
	Service        string    `json:"service"`
	PlanID         string    `json:"plan_id"`
	EffectiveStart time.Time `json:"effective_start"`
	EffectiveEnd   time.Time `json:"effective_end"`
}

// RecomputeDelta 重算差额（用户×月）。
type RecomputeDelta struct {
	UserID       string           `json:"user_id"`
	BillMonth    string           `json:"bill_month"`
	OldFee       int64            `json:"old_fee"`
	NewFee       int64            `json:"new_fee"`
	Delta        int64            `json:"delta"`
	ServiceDelta map[string]int64 `json:"service_delta"`
}
