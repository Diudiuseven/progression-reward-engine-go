package engine

import (
	"errors"
	"time"
)

// ---------- 基础标识 ----------

type PlayerID string
type SeasonID string
type RuleVersion string
type EventType string

// ---------- 事件摄入 ----------

// Event 是玩法产生的进度事件。OccurredAt 是事件发生时间（权威时间），
// 赛季归属、每日分桶与高频递减全部以它为准，与到达引擎的处理时间无关，
// 从而保证乱序/迟到事件的结算可解释、可复现。
type Event struct {
	ID         string
	PlayerID   PlayerID
	Type       EventType
	Amount     int64 // 原始进度值，必须 >= 0
	OccurredAt time.Time
}

// GainBreakdown 是单条事件有效进度的结算明细，逐字段解释
// “原始值 -> 高频递减 -> 每日/赛季封顶 -> 实际入账”的全过程。
type GainBreakdown struct {
	EventID         string
	SeasonID        SeasonID
	DayBucket       string  // OccurredAt 对应的 UTC 日期
	Gross           int64   // 原始进度
	PriorInWindow   int     // 高频窗口内此前已结算事件数
	FrequencyFactor float64 // 递减因子
	AfterDecay      int64   // 递减后的进度
	DailyCap        int64   // 当日上限
	SeasonCap       int64   // 赛季上限
	DailyCut        int64   // 被每日封顶截掉的部分
	SeasonCut       int64   // 被赛季封顶截掉的部分
	Granted         int64   // 最终入账有效进度
}

type IngestResult struct {
	EventID        string
	PlayerID       PlayerID
	Accepted       bool
	Reason         string // duplicate / season_settled / season_not_found / invalid
	SeasonID       SeasonID
	LevelBefore    int
	LevelAfter     int
	ProgressBefore int64
	ProgressAfter  int64
	Breakdown      *GainBreakdown
}

// ---------- 奖励规则（版本化） ----------

type Reward struct {
	Code   string
	Amount int64
}

// RuleSet 是某一版本的奖励规则，按等级配置一次性里程碑奖励。
// 赛季在开启时绑定一个固定版本，赛季内同一份进度产生的可领取奖励口径恒定。
type RuleSet struct {
	Version       RuleVersion
	EffectiveFrom time.Time
	Rewards       map[int][]Reward
}

// ---------- 递减与封顶配置 ----------

// DecayConfig 高频边际递减：以 OccurredAt 为准的滑动窗口，
// 窗口内每多一条已结算事件，因子乘一次 Gamma，最低到 MinFactor。
type DecayConfig struct {
	Window    time.Duration // 默认 60s
	Gamma     float64       // 默认 0.8
	MinFactor float64       // 默认 0.1
}

// SeasonConfig 赛季配置。End 为零值表示开放式赛季。
type SeasonConfig struct {
	ID              SeasonID
	Start           time.Time
	End             time.Time
	CarryRatio      float64 // 赛季结算时结转到下一赛季的有效进度比例 [0,1]
	LevelThresholds []int64 // 升序，第 i 项表示达到等级 i+1 所需累计有效进度
	RuleVersion     RuleVersion
	Decay           DecayConfig
	DailyCap        int64 // <=0 表示不限
	SeasonCap       int64 // <=0 表示不限
}

// ---------- 领取 ----------

const (
	ClaimPrepared  = "prepared"
	ClaimCommitted = "committed"
	ClaimAborted   = "aborted"
)

type ClaimRecord struct {
	ClaimID     string
	PlayerID    PlayerID
	SeasonID    SeasonID
	Level       int
	RuleVersion RuleVersion
	Rewards     []Reward
	Status      string
	CommittedAt time.Time
}

type ClaimResult struct {
	ClaimID        string
	PlayerID       PlayerID
	SeasonID       SeasonID
	Level          int
	AlreadyClaimed bool
	Record         *ClaimRecord
}

// ---------- 对外视图 ----------

type PlayerView struct {
	PlayerID          PlayerID
	SeasonID          SeasonID
	Status            string // active / settled
	RawProgress       int64
	EffectiveProgress int64
	Level             int
	SeasonGranted     int64
	DailyGranted      map[string]int64
	ClaimedLevels     []int
}

type SeasonSettlement struct {
	SeasonID        SeasonID
	PlayerCount     int
	PlayerSummaries []SettledPlayer
	SettledAt       time.Time
}

type SettledPlayer struct {
	PlayerID          PlayerID
	FinalLevel        int
	EffectiveProgress int64
	CarriedOver       int64
	UnclaimedLevels   []int
}

// ---------- 错误 ----------

var (
	ErrEventInvalid       = errors.New("event invalid")
	ErrSeasonNotFound     = errors.New("season not found for event time")
	ErrNoActiveSeason     = errors.New("no active season")
	ErrRuleNotFound       = errors.New("rule version not found")
	ErrLevelNotReached    = errors.New("level not reached")
	ErrSeasonNotClaimable = errors.New("season not claimable")
	ErrClaimIncomplete    = errors.New("previous claim attempt did not commit")
	ErrEngineClosed       = errors.New("engine closed")
)

// Config 引擎配置：Dir 为空时纯内存运行；否则在 Dir 下按分片写 WAL/快照。
type Config struct {
	Dir           string
	ShardCount    int
	Clock         func() time.Time
	RetainSettled int // >0 时后台自动淘汰，仅保留最近 N 个已结束赛季的账本；0=手动淘汰
	EvictInterval time.Duration
}
