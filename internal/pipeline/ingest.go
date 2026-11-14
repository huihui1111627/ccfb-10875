package pipeline

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"ratingengine/internal/codec"
	"ratingengine/internal/model"
	"ratingengine/internal/store"
)

// IngestStats 接入统计。
type IngestStats struct {
	Files       int
	Lines       int64
	Decoded     int64
	Garbled     int64
	ShardCounts map[int]int64
}

// IngestFile 解码单个话单文件并按 user_id 分片写入任务 raw.jsonl。
// 解码/基础校验失败的记录直接写该分片（无法定分片时写 0 号分片）异常文件。
func IngestFile(st *store.Store, taskID string, shardCount int, path string, stats *IngestStats) error {
	ext := strings.ToLower(filepath.Ext(path))
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	writers := make(map[int]*store.JSONLWriter)
	exWriters := make(map[int]*store.JSONLWriter)
	closeAll := func() {
		for _, w := range writers {
			_ = w.Close()
		}
		for _, w := range exWriters {
			_ = w.Close()
		}
	}
	getWriter := func(sh int) (*store.JSONLWriter, error) {
		if w, ok := writers[sh]; ok {
			return w, nil
		}
		// 多输入文件共享分片文件，必须追加而非截断
		w, err := store.AppendJSONL(st.RawPath(taskID, sh))
		if err != nil {
			return nil, err
		}
		writers[sh] = w
		return w, nil
	}
	getEx := func(sh int) (*store.JSONLWriter, error) {
		if w, ok := exWriters[sh]; ok {
			return w, nil
		}
		// 接入异常独立文件，避免与批价阶段异常在续跑时互相覆盖/重复
		w, err := store.AppendJSONL(st.IngestExceptionPath(taskID, sh))
		if err != nil {
			return nil, err
		}
		exWriters[sh] = w
		return w, nil
	}

	emitException := func(sh int, reason, detail, snippet string) {
		stats.Garbled++
		w, e := getEx(sh)
		if e != nil {
			err = e
			return
		}
		_ = w.Write(&model.ExceptionCDR{
			Shard: sh, Reason: reason, Detail: detail,
			RawSnippet: snippet, Stage: "ingest",
			OccurredAt: time.Now().UTC().Format(model.TimeLayout),
		})
	}

	br := bufio.NewReaderSize(f, 1<<20)

	switch ext {
	case ".fixed", ".delim":
		for {
			line, readErr := br.ReadString('\n')
			raw := strings.TrimRight(line, "\r\n")
			if len(raw) > 0 {
				stats.Lines++
				cdr, perr := decodeText(ext, raw)
				if perr != nil {
					emitException(0, model.ErrBadFormat, perr.Error(), snippet(raw))
				} else {
					sh := codec.ShardOf(cdr.UserID, shardCount)
					stats.ShardCounts[sh]++
					stats.Decoded++
					cdr.RawLine = raw
					w, e := getWriter(sh)
					if e != nil {
						closeAll()
						return e
					}
					if e := w.Write(cdr); e != nil {
						closeAll()
						return e
					}
				}
			}
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				closeAll()
				return readErr
			}
		}
	case ".asn1":
		// 文件为多条 DER SEQUENCE 串联，逐条 Unmarshal
		data, readErr := io.ReadAll(f)
		if readErr != nil {
			return readErr
		}
		rest := data
		for len(rest) > 0 {
			stats.Lines++
			cdr, perr := decodeASN1Seq(&rest)
			if perr != nil {
				emitException(0, model.ErrBadFormat, perr.Error(), snippetBytes(rest))
				break // DER 错位后无法继续定位，后续记录在重跑前需修复文件
			}
			sh := codec.ShardOf(cdr.UserID, shardCount)
			stats.ShardCounts[sh]++
			stats.Decoded++
			w, e := getWriter(sh)
			if e != nil {
				closeAll()
				return e
			}
			if e := w.Write(cdr); e != nil {
				closeAll()
				return e
			}
		}
	case ".lpbin":
		for {
			var lenBuf [4]byte
			_, readErr := io.ReadFull(f, lenBuf[:])
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				closeAll()
				return readErr
			}
			n := binary.BigEndian.Uint32(lenBuf[:])
			buf := make([]byte, n)
			if _, e := io.ReadFull(f, buf); e != nil {
				closeAll()
				return e
			}
			stats.Lines++
			// 与文本路径一致：先做 NUL/UTF-8 合法性预检，再解定长负载
			if bytes.IndexByte(buf, 0) >= 0 || !utf8.Valid(buf) {
				emitException(0, model.ErrBadFormat, "garbled record: invalid utf8 or NUL byte", snippetBytes(buf))
				continue
			}
			cdr, perr := codec.ParseLPBin(buf)
			if perr != nil {
				emitException(0, model.ErrBadFormat, perr.Error(), snippetBytes(buf))
				continue
			}
			sh := codec.ShardOf(cdr.UserID, shardCount)
			stats.ShardCounts[sh]++
			stats.Decoded++
			cdr.RawLine = string(buf)
			w, e := getWriter(sh)
			if e != nil {
				closeAll()
				return e
			}
			if e := w.Write(cdr); e != nil {
				closeAll()
				return e
			}
		}
	default:
		closeAll()
		return fmt.Errorf("unsupported cdr file extension: %s", ext)
	}

	closeAll()
	return err
}

func decodeText(ext, raw string) (*model.RawCDR, error) {
	if !utf8.ValidString(raw) || strings.ContainsRune(raw, 0) {
		return nil, fmt.Errorf("garbled line: invalid utf8 or NUL byte")
	}
	if ext == ".fixed" {
		return codec.ParseFixed(raw)
	}
	return codec.ParseDelimited(raw)
}

func decodeASN1Seq(rest *[]byte) (*model.RawCDR, error) {
	// 先按外层 TLV 长度切片，再解码单条（ParseASN1 拒绝尾部多余字节）
	consumed, err := codec.ASN1SeqLen(*rest)
	if err != nil {
		return nil, err
	}
	cdr, err := codec.ParseASN1((*rest)[:consumed])
	if err != nil {
		return nil, err
	}
	*rest = (*rest)[consumed:]
	return cdr, nil
}

func snippet(s string) string {
	if len(s) > 200 {
		return s[:200]
	}
	return s
}

func snippetBytes(b []byte) string {
	if len(b) > 200 {
		return string(b[:200])
	}
	return string(b)
}
