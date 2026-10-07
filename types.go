package engine

import "time"

// Event 是玩法产生的进度事件。EventTime 决定赛季归属，而非到达时间。
type Event struct {
	ID        string    `json:"id"`
	PlayerID  string    `json:"player_id"`
	Type      string    `json:"type"`
	Points    int64     `json:"points"`
	EventTime time.Time `json:"event_time"`
}

// LevelThreshold 等级阈值：累计进度达到 Required 即升至 Level。
type LevelThreshold struct {
	Level    int   `json:"level"`
	Required int64 `json:"required"`
}

// Reward 某等级可领取的奖励。
type Reward struct {
	Level    int    `json:"level"`
	RewardID string `json:"reward_id"`
	Amount   int64  `json:"amount"`
}

// RuleVersion 一版奖励/收益规则，带生效时间。
type RuleVersion struct {
	Version       string           `json:"version"`
	EffectiveFrom time.Time        `json:"effective_from"`
	Levels        []LevelThreshold `json:"levels"`
	Rewards       []Reward         `json:"rewards"`
	DayCap        int64            `json:"day_cap"`    // 每日有效进度软上限
	DayKnee       int64            `json:"day_knee"`   // 递减拐点，越小衰减越快
	SeasonCap     int64            `json:"season_cap"` // 赛季有效进度硬上限
}

// SeasonConfig 赛季配置。
type SeasonConfig struct {
	SeasonID    string    `json:"season_id"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	CarryRatio  float64   `json:"carry_ratio"` // 切换时结转到下一赛季的比例，其余重置
	RuleVersion string    `json:"rule_version"`
}

// GainRecord 单次事件的可解释结算记录。
type GainRecord struct {
	EventID          string `json:"event_id"`
	SeasonID         string `json:"season_id"`
	Raw              int64   `json:"raw"`
	Effective        float64 `json:"effective"`
	Reason           string  `json:"reason"` // ok / duplicate / late / day_cap / season_cap / decayed
	DayTotalAfter    float64 `json:"day_total_after"`
	SeasonTotalAfter float64 `json:"season_total_after"`
}

// PlayerState 单个玩家在当前赛季的进度状态。
type PlayerState struct {
	PlayerID       string           `json:"player_id"`
	SeasonID       string           `json:"season_id"`
	SeasonProgress float64            `json:"season_progress"`
	DayProgress    map[string]float64 `json:"day_progress"` // key: 事件日(UTC)，支持跨日乱序；只保留最近若干天
	SeenEvents     map[string]bool  `json:"seen_events"`  // 本赛季已处理事件，用于去重；赛季结转时清空
	Claimed        map[string]bool  `json:"claimed"`      // key: seasonID/rewardID，领取幂等
	ClaimedRewards []Reward         `json:"claimed_rewards"` // 已发放明细，用于重复领取时还原首次口径
	LastGains      []GainRecord     `json:"last_gains"`   // 环形保留最近若干条，便于解释
}

// ClaimResult 领取结果，幂等：重复领取返回首次结果。
type ClaimResult struct {
	PlayerID string   `json:"player_id"`
	SeasonID string   `json:"season_id"`
	Level    int      `json:"level"`
	Granted  []Reward `json:"granted"` // 本次新发放
	Already  []Reward `json:"already"` // 此前已发放
}

// PlayerView 对外读取的一致性快照。
type PlayerView struct {
	PlayerID       string  `json:"player_id"`
	SeasonID       string  `json:"season_id"`
	SeasonProgress float64 `json:"season_progress"`
	DayProgress    float64 `json:"day_progress"`
	Level          int     `json:"level"`
	RuleVersion    string  `json:"rule_version"`
}
