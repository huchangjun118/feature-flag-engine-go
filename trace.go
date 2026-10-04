package flagengine

import (
	"bufio"
	"encoding/json"
	"os"
	"sync"
)

// FileAuditSink 以 JSON Lines 追加方式将审计事件写入文件，
// 进程重启后历史事件仍可追溯。写入互斥，单条写失败被忽略但不影响评估主路径。
type FileAuditSink struct {
	mu sync.Mutex
	f  *os.File
	w  *bufio.Writer
}

// NewFileAuditSink 打开（不存在则创建）一个追加模式的审计文件。
func NewFileAuditSink(path string) (*FileAuditSink, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &FileAuditSink{f: f, w: bufio.NewWriterSize(f, 4096)}, nil
}

// Write 追加一行 JSON 审计事件并立即刷盘，保证崩溃前事件尽量不丢。
func (s *FileAuditSink) Write(event []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.w.Write(event); err != nil {
		return err
	}
	return s.w.Flush()
}

// Close 刷盘并关闭底层文件。
func (s *FileAuditSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.w.Flush(); err != nil {
		_ = s.f.Close()
		return err
	}
	return s.f.Close()
}

// recordTrace 将一条评估记录写入固定容量的环形缓冲。
// 容量上限使追溯内存占用有界，且不阻塞评估（仅极短的锁）。
func (e *Engine) recordTrace(t Trace) {
	e.tracesMu.Lock()
	c := cap(e.traces)
	if len(e.traces) < c {
		e.traces = append(e.traces, t)
	} else {
		e.traces[e.tracePos] = t
	}
	e.tracePos = (e.tracePos + 1) % c
	e.tracesMu.Unlock()
}

// TraceToJSON 将一条追溯记录序列化为 JSON，便于导出。
func TraceToJSON(t Trace) ([]byte, error) {
	return json.Marshal(t)
}
