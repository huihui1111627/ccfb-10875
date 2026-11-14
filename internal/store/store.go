// Package store 管理基于文件的 JSONL 存储、分片目录与原子 checkpoint。
package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Store 数据根目录。
type Store struct {
	Root string
}

func New(root string) *Store { return &Store{Root: root} }

// 目录布局
func (s *Store) InboxDir() string         { return filepath.Join(s.Root, "inbox") }
func (s *Store) TariffDir() string        { return filepath.Join(s.Root, "tariffs") }
func (s *Store) SubFile() string          { return filepath.Join(s.Root, "subscriptions.jsonl") }
func (s *Store) OracleDir() string        { return filepath.Join(s.Root, "oracle") }
func (s *Store) TaskDir(id string) string { return filepath.Join(s.Root, "tasks", id) }
func (s *Store) TasksIndexDir() string    { return filepath.Join(s.Root, "tasks") }

// EnsureLayout 初始化目录。
func (s *Store) EnsureLayout() error {
	for _, d := range []string{s.InboxDir(), s.TariffDir(), s.OracleDir(), s.TasksIndexDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// ShardDir 任务下某分片目录。
func (s *Store) ShardDir(taskID string, shard int) string {
	return filepath.Join(s.TaskDir(taskID), fmt.Sprintf("shard-%04d", shard))
}

func (s *Store) RawPath(taskID string, shard int) string {
	return filepath.Join(s.ShardDir(taskID, shard), "raw.jsonl")
}
func (s *Store) RatedPath(taskID string, shard int) string {
	return filepath.Join(s.ShardDir(taskID, shard), "rated.jsonl")
}
func (s *Store) ExceptionPath(taskID string, shard int) string {
	return filepath.Join(s.ShardDir(taskID, shard), "exceptions.jsonl")
}
func (s *Store) IngestExceptionPath(taskID string, shard int) string {
	return filepath.Join(s.ShardDir(taskID, shard), "ingest_exceptions.jsonl")
}
func (s *Store) SummaryPath(taskID string, shard int) string {
	return filepath.Join(s.ShardDir(taskID, shard), "summary.jsonl")
}
func (s *Store) MergePath(taskID string, shard int) string {
	return filepath.Join(s.ShardDir(taskID, shard), "merges.jsonl")
}
func (s *Store) CheckpointPath(taskID string, shard int) string {
	return filepath.Join(s.ShardDir(taskID, shard), "checkpoint.json")
}

func (s *Store) TaskMetaPath(taskID string) string {
	return filepath.Join(s.TaskDir(taskID), "task.json")
}
func (s *Store) GlobalSummaryPath(taskID string) string {
	return filepath.Join(s.TaskDir(taskID), "global_summary.jsonl")
}
func (s *Store) ReconcilePath(taskID string) string {
	return filepath.Join(s.TaskDir(taskID), "reconcile.json")
}
func (s *Store) DeltaPath(taskID string) string {
	return filepath.Join(s.TaskDir(taskID), "deltas.jsonl")
}
func (s *Store) StageLogPath(taskID string) string {
	return filepath.Join(s.TaskDir(taskID), "stages.jsonl")
}

// JSONLWriter 流式 JSONL 写出。
type JSONLWriter struct {
	f   *os.File
	bw  *bufio.Writer
	enc *json.Encoder
}

func CreateJSONL(path string) (*JSONLWriter, error) {
	return openJSONL(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY)
}

// AppendJSONL 以追加模式打开（接入阶段多个输入文件写同一分片文件时必须使用）。
func AppendJSONL(path string) (*JSONLWriter, error) {
	return openJSONL(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY)
}

func openJSONL(path string, flag int) (*JSONLWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, flag, 0o644)
	if err != nil {
		return nil, err
	}
	bw := bufio.NewWriterSize(f, 1<<16)
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)
	return &JSONLWriter{f: f, bw: bw, enc: enc}, nil
}

func (w *JSONLWriter) Write(v any) error { return w.enc.Encode(v) }

func (w *JSONLWriter) Close() error {
	if err := w.bw.Flush(); err != nil {
		_ = w.f.Close()
		return err
	}
	return w.f.Close()
}

// JSONLReader 流式 JSONL 读取，回调处理每条记录。
func ReadJSONL(path string, fn func(line []byte) error) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 1<<16)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			// 去掉末尾换行
			for len(line) > 0 && (line[len(line)-1] == '\n' || line[len(line)-1] == '\r') {
				line = line[:len(line)-1]
			}
			if len(line) > 0 {
				if e := fn(line); e != nil {
					return e
				}
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// ReadJSON 读取 JSONL 并反序列化到 v。
func ReadJSONLInto[T any](path string, fn func(*T) error) error {
	return ReadJSONL(path, func(line []byte) error {
		var v T
		if err := json.Unmarshal(line, &v); err != nil {
			return fmt.Errorf("unmarshal %s: %w", path, err)
		}
		return fn(&v)
	})
}

// WriteJSONAtomic 原子写 JSON 文件（tmp + rename）。
func WriteJSONAtomic(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// ReadJSON 读取 JSON 文件。
func ReadJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// ListTaskIDs 返回所有任务 ID（按目录名排序）。
func (s *Store) ListTaskIDs() ([]string, error) {
	entries, err := os.ReadDir(s.TasksIndexDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() {
			ids = append(ids, e.Name())
		}
	}
	return ids, nil
}
