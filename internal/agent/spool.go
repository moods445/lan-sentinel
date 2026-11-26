package agent

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
)

// Spool 断网环形补传：最多 200 批 / 5MB，按序补传，服务端按 host+ts 幂等
type Spool struct {
	path     string
	maxLines int
	maxBytes int64
}

func NewSpool(path string) *Spool {
	return &Spool{path: path, maxLines: 200, maxBytes: 5 << 20}
}

func (s *Spool) Load() ([][]byte, error) {
	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out [][]byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		b := sc.Bytes()
		if len(bytes.TrimSpace(b)) == 0 {
			continue
		}
		line := make([]byte, len(b))
		copy(line, b)
		out = append(out, line)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) > s.maxLines {
		out = out[len(out)-s.maxLines:]
	}
	return out, nil
}

// Replace 用当前待发批次重写 spool（原子替换，崩溃后不产生半行）
func (s *Spool) Replace(lines [][]byte) error {
	if len(lines) == 0 {
		return s.Clear()
	}
	var n int64
	start := 0
	for i := len(lines) - 1; i >= 0; i-- { // 只保留最新的 maxBytes，旧的丢弃
		n += int64(len(lines[i])) + 1
		if n > s.maxBytes {
			start = i + 1
			break
		}
	}
	var buf bytes.Buffer
	for i := start; i < len(lines); i++ { // 旧 → 新
		buf.Write(lines[i])
		buf.WriteByte('\n')
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *Spool) Clear() error {
	err := os.Remove(s.path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
