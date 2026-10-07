package engine

import (
	"encoding/json"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// 持久化布局（DataDir 下）：
//   season.json        —— 赛季元信息（活跃赛季、已结算集合），原子写
//   players/<id>.json  —— 单个玩家状态，写穿 + 原子替换；
//                         单玩家操作（进度/领取）= 单次原子写，崩溃不会残留半份记录
//   switch.log         —— 赛季切换两阶段提交日志（begin/commit，每行 "crc32hex <json>"）
//
// 恢复语义：switch.log 中存在无配对 commit 的 begin 时，按 begin 载荷重做切换
// （重做是幂等的）；CRC 校验失败的尾部半行视为未提交，直接截断。

const (
	switchBegin  = "begin"
	switchCommit = "commit"
)

// SeasonMeta 赛季元信息。
type SeasonMeta struct {
	ActiveSeason string   `json:"active_season"`
	Settled      []string `json:"settled"`
}

// SwitchRecord 赛季切换日志记录。
type SwitchRecord struct {
	Kind     string   `json:"kind"` // begin / commit
	SwitchID string   `json:"switch_id"`
	To       string   `json:"to"`
	Settled  []string `json:"settled"`
}

// Store 本地持久化。
type Store struct {
	mu         sync.Mutex
	dir        string
	playersDir string
	seasonPath string
	switchPath string
	sf         *os.File
}

func OpenStore(dir string) (*Store, error) {
	s := &Store{
		dir:        dir,
		playersDir: filepath.Join(dir, "players"),
		seasonPath: filepath.Join(dir, "season.json"),
		switchPath: filepath.Join(dir, "switch.log"),
	}
	if err := os.MkdirAll(s.playersDir, 0o755); err != nil {
		return nil, err
	}
	sf, err := os.OpenFile(s.switchPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	s.sf = sf
	return s, nil
}

// writeFileAtomic 写临时文件 + fsync + rename，保证要么完整生效要么不存在。
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadSeasonMeta 读取赛季元信息；不存在返回 (nil, nil)。
func (s *Store) LoadSeasonMeta() (*SeasonMeta, error) {
	data, err := os.ReadFile(s.seasonPath)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m SeasonMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("corrupt season meta: %w", err)
	}
	return &m, nil
}

// SaveSeasonMeta 原子写赛季元信息。
func (s *Store) SaveSeasonMeta(m *SeasonMeta) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return writeFileAtomic(s.seasonPath, data)
}

// LoadPlayer 读取玩家状态；不存在返回 (nil, nil)。
func (s *Store) LoadPlayer(id string) (*PlayerState, error) {
	data, err := os.ReadFile(s.playerPath(id))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st PlayerState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("corrupt player state %q: %w", id, err)
	}
	return &st, nil
}

// SavePlayer 原子写穿玩家状态。
func (s *Store) SavePlayer(st *PlayerState) error {
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return writeFileAtomic(s.playerPath(st.PlayerID), data)
}

func (s *Store) playerPath(id string) string {
	// id 只取安全字符，避免路径穿越
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		}
		return '_'
	}, id)
	return filepath.Join(s.playersDir, safe+".json")
}

// AppendSwitch 追加一条切换日志并 fsync；一条完整行即视为已提交。
func (s *Store) AppendSwitch(rec SwitchRecord) error {
	payload, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	line := fmt.Sprintf("%08x %s\n", crc32.ChecksumIEEE(payload), payload)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.sf.WriteString(line); err != nil {
		return err
	}
	return s.sf.Sync()
}

// PendingSwitch 返回最后一个未配对的 begin（崩溃残留），无则返回 nil。
// 尾部 CRC 失败的半行视为未提交并截断。
func (s *Store) PendingSwitch() (*SwitchRecord, error) {
	data, err := os.ReadFile(s.switchPath)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var pending *SwitchRecord
	valid := int64(0)
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		sp := strings.IndexByte(line, ' ')
		if sp <= 0 {
			break
		}
		want, perr := strconv.ParseUint(line[:sp], 16, 32)
		payload := line[sp+1:]
		if perr != nil || crc32.ChecksumIEEE([]byte(payload)) != uint32(want) {
			break // 未提交的半行
		}
		var rec SwitchRecord
		if jerr := json.Unmarshal([]byte(payload), &rec); jerr != nil {
			break
		}
		valid += int64(len(line)) + 1
		switch rec.Kind {
		case switchBegin:
			r := rec
			pending = &r
		case switchCommit:
			if pending != nil && pending.SwitchID == rec.SwitchID {
				pending = nil
			}
		}
	}
	if valid < int64(len(data)) {
		if terr := os.Truncate(s.switchPath, valid); terr != nil {
			return nil, terr
		}
	}
	return pending, nil
}

// Close 关闭日志文件。
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sf != nil {
		err := s.sf.Close()
		s.sf = nil
		return err
	}
	return nil
}
