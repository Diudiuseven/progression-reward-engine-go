package engine

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// WAL 记录种类。
const (
	recEventAccepted  = "event_accepted"
	recClaimPrepared  = "claim_prepared"
	recClaimCommitted = "claim_committed"
	recClaimAborted   = "claim_aborted"
	recPlayerSettled  = "player_settled"
	recCarryInit      = "carry_init"
	recRuleRegistered = "rule_registered"
	recSeasonAdded    = "season_added"
)

// walRecord 是预写日志的统一记录体。只放可序列化字段；
// 同一条记录描述一个不可分割的状态迁移，恢复时按序重放即可。
type walRecord struct {
	Kind string `json:"kind"`

	// 事件结算
	PlayerID   PlayerID       `json:"player_id,omitempty"`
	SeasonID   SeasonID       `json:"season_id,omitempty"`
	OccurredAt time.Time      `json:"occurred_at,omitempty"`
	LevelAfter int            `json:"level_after,omitempty"`
	Breakdown  *GainBreakdown `json:"breakdown,omitempty"`

	// 领取
	Claim *ClaimRecord `json:"claim,omitempty"`

	// 赛季结算 / 结转
	FinalLevel      int         `json:"final_level,omitempty"`
	Effective       int64       `json:"effective,omitempty"`
	CarriedOver     int64       `json:"carried_over,omitempty"`
	UnclaimedLevels []int       `json:"unclaimed_levels,omitempty"`
	FromSeason      SeasonID    `json:"from_season,omitempty"`
	NextSeason      SeasonID    `json:"next_season,omitempty"`
	RuleVersion     RuleVersion `json:"rule_version,omitempty"`

	// 元数据
	Rule   *RuleSet      `json:"rule,omitempty"`
	Season *SeasonConfig `json:"season_cfg,omitempty"`
}

// ---------- 快照载荷 ----------

type dedupEntry struct {
	SeasonID SeasonID `json:"season_id"`
	PlayerID PlayerID `json:"player_id"`
	EventIDs []string `json:"event_ids"`
}

type shardSnapshot struct {
	ShardID uint32            `json:"shard_id"`
	Dedup   []dedupEntry      `json:"dedup"`
	Players []*playerProgress `json:"players"`
}

type metaSnapshot struct {
	Rules   []RuleSet      `json:"rules"`
	Seasons []SeasonConfig `json:"seasons"`
}

// store 负责单个分片（或元数据分片）的预写日志与快照。
// 每次 commit 同步落盘；快照写入采用 “临时文件 + rename” 原子替换，
// 崩溃后要么看到旧快照、要么看到新快照，不会读到半份文件。
type store struct {
	dir     string
	name    string
	inMem   bool
	mu      sync.Mutex
	f       *os.File
	w       *bufio.Writer
	enc     *json.Encoder
	snapSeq int
}

func openStore(dir string, name string) (*store, error) {
	s := &store{dir: dir, name: name}
	if dir == "" {
		s.inMem = true
		return s, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	logPath := s.logPath()
	if err := repairTrailingRecord(logPath); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	s.f = f
	s.w = bufio.NewWriter(f)
	s.enc = json.NewEncoder(s.w)
	return s, nil
}

func (s *store) logPath() string  { return filepath.Join(s.dir, s.name+".log") }
func (s *store) snapPath() string { return filepath.Join(s.dir, s.name+".snap") }
func (s *store) tmpPath() string  { return filepath.Join(s.dir, s.name+".snap.tmp") }

// append 追加一条日志并 fsync；内存模式下为 no-op。
func (s *store) append(rec walRecord) error {
	if s.inMem {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.enc.Encode(rec); err != nil {
		return err
	}
	if err := s.w.Flush(); err != nil {
		return err
	}
	return s.f.Sync()
}

func loadSnapshot[T any](path string) (*T, bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var v T
	if len(data) == 0 {
		return nil, false, nil
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, false, err
	}
	return &v, true, nil
}

// saveSnapshot 原子写入快照并截断日志。调用方必须保证 state 是完整版本。
func (s *store) saveSnapshot(state any) error {
	if s.inMem {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.tmpPath()
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.snapPath()); err != nil {
		return err
	}
	// 截断并重开日志文件。
	if s.f != nil {
		_ = s.f.Close()
	}
	f, err := os.OpenFile(s.logPath(), os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	s.f = f
	s.w = bufio.NewWriter(f)
	s.enc = json.NewEncoder(s.w)
	s.snapSeq++
	return f.Sync()
}

func (s *store) close() error {
	if s.inMem {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f != nil {
		if err := s.w.Flush(); err != nil {
			return err
		}
		return s.f.Close()
	}
	return nil
}

// repairTrailingRecord 处理崩溃留下的“撕裂尾部”：
// 每条完整记录都是以 '\n' 结尾的一行 JSON；若文件末字符不是换行，
// 说明最后一行从未写完（也从未完成提交语义），将其截断到最后一个换行，
// 避免后续 append 把新记录拼到残行上。
func repairTrailingRecord(path string) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(data) == 0 || data[len(data)-1] == '\n' {
		return nil
	}
	cut := len(data)
	for i := len(data) - 1; i >= 0; i-- {
		if data[i] == '\n' {
			cut = i + 1
			break
		}
		if i == 0 {
			cut = 0
		}
	}
	return os.WriteFile(path, data[:cut], 0o644)
}
