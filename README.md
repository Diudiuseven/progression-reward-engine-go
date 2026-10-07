# 玩赚成长进度与奖励结算引擎（Go）

一套仅依赖**进程内状态 + 本地持久化**的成长进度与奖励结算能力，把
**事件摄入 → 进度结算（高频递减 / 每日·赛季封顶）→ 等级推进 → 奖励领取 →
赛季切换与结转 → 崩溃恢复** 串成一条可复现、可解释的链路。

* 无外部数据库 / 中间件依赖：配置 `Dir` 即用本地预写日志（WAL）+ 快照持久化；`Dir` 为空则纯内存。
* 玩家按哈希分片，单玩家串行、多玩家并发不互相阻塞。
* 每条状态迁移先落 WAL（fsync）再对外可见；快照用临时文件 + 原子 rename。

## 快速开始

```bash
go test ./...            # 全部自动化测试（含 -race 场景）
go test -race ./...      # 竞态检测
go run ./cmd/demo        # 端到端演示，并在 ./demo-data 生成 WAL/快照
```

## 1. 核心模型

### 事件与两种进度

```go
Event{ID, PlayerID, Type, Amount, OccurredAt}
```

* `OccurredAt` 是**事件发生时间**，是赛季归属、每日分桶、高频窗口的唯一时间依据；
  与事件到达引擎的处理时间无关，因此乱序 / 迟到事件的结论可解释、可复现。
* 每个玩家每个赛季维护两本进度：
  * `RawProgress`：原始累计进度，只观测；
  * `EffectiveProgress`：有效累计进度，经过「高频递减 → 每日封顶 → 赛季封顶」后入账，决定等级。

### 等级

* `LevelThresholds []int64` 升序配置：`thresholds[i] <= eff` 时等级至少为 `i+1`；
* 等级**单调递增**；迟到事件补入有效进度，但已对外固化的等级奖励不被回退。

### 收益结算（纯函数，可解释可复现）

单条事件的有效进度在 `gain(ev, ledger, seasonCfg)` 中计算，返回完整明细
`GainBreakdown`：

```
gross ──高频递减──▶ afterDecay ──每日封顶──▶ …… ──赛季封顶──▶ granted
```

1. **高频边际递减**：以 `OccurredAt` 为右边界的滑动窗口 `Decay.Window`，
   窗口内此前每多一条已结算事件，因子乘一次 `Gamma`，最低 `MinFactor`：
   `factor = max(MinFactor, Gamma^priorInWindow)`，
   `afterDecay = floor(gross * factor)`。
   高频刷进度的收益因此平滑衰减；明细里同时给出 `PriorInWindow` 与 `FrequencyFactor`。
2. **每日上限 `DailyCap`**：按 `OccurredAt` 的 UTC 日期分桶（`DailyGranted[day]`），
   超出部分计入 `DailyCut`，不发放。跨天自然重置。
3. **赛季上限 `SeasonCap`**：本赛季累计有效进度封顶，超出计入 `SeasonCut`。

> 语义说明：高频递减定义在**事件到达流**上（在线流控的常规语义）；相同到达顺序
> 产出逐字段一致、可精确复现的 `GainBreakdown`（见 `TestDecayDeterminismNoCap`）。
> 乱序 / 迟到事件依然遵守「去重、正确赛季归属、原始累计不丢不重」的不变量。

## 2. 事件可靠性：乱序、重复、迟到

* **去重**：以 `(赛季, 玩家, 事件ID)` 为键记录已结算事件；重复事件返回
  `Accepted=false, Reason=duplicate`，永不重复累计。该判定先于赛季冻结判定，
  同一条已结算事件无论何时重投都稳定返回 `duplicate`。
* **乱序**：按 `OccurredAt` 插入有序的事件时间序列，高频窗口与每日分桶不依赖到达顺序。
* **迟到**：
  * 目标赛季仍开放 → 正常补入（可能抬高当前等级，但不回退已发奖励）；
  * 目标赛季已结算冻结 → 拒绝，`Reason=season_settled`，
    **已经对外可见的等级与奖励不被迟到事件改写**。
* **玩家隔离**：玩家 FNV 哈希固定分片，账本与去重集合均按玩家独立。

## 3. 奖励规则版本化

```go
RuleSet{Version, EffectiveFrom, Rewards: map[level][]Reward}
```

* 规则只追加、不改写；新版本通过新赛季（或指定 `EffectiveFrom`）生效。
* **赛季在开启时绑定固定 `RuleVersion`**：整个赛季内，同一份进度产生的可领取奖励口径一致。
* 领取时把奖励数组**深拷贝冻结**进领取记录；之后再注册新规则版本，
  旧赛季的待领 / 已领奖励都按旧版本计算，**已发奖励绝不被追溯改写**。

## 4. 奖励领取：幂等、两阶段、并发安全

`ClaimLevel(ctx, player, season, level, claimID)`（`season` 传空串表示由引擎选择
未结算赛季）：

1. 同一 `(玩家, 赛季, 等级)` 已成功领取 → 后续任何 `claimID` 都只返回**首次结果**
   （`AlreadyClaimed=true`）。
2. 未达等级 → `ErrLevelNotReached`，不产生记录。
3. 两阶段提交：
   * **prepared**：按赛季绑定的规则版本冻结奖励快照，写 WAL；
   * **committed**：等级写入 `ClaimedLevels`、清空半成品，写 WAL。
   两阶段都在该玩家分片写锁内完成；视图读取持读锁，因此任一时刻读到的
   等级与账目都是某个自洽的完整版本。
4. 提交 WAL 失败则内存回滚到 prepared，可安全重试，不会重复发放。
5. **崩溃恢复**：只有 prepared 没有 committed 的领取，重启时一律补写
   `aborted` 并从内存清除——不存在半份领取残留；同一 `claimID` 可重新领取。

## 5. 赛季推进与重置语义

`SwitchSeason(ctx, nextCfg)`：

1. 校验新赛季区间不与既有赛季重叠、规则版本存在；
2. **先把新赛季配置持久化并加入赛季表**：这样切换窗口内到达的、
   `OccurredAt` 已属于新赛季的事件立刻能正确路由到新赛季；
3. 逐分片（不做全局停顿）把上一赛季每个玩家的账本原子结转：
   * 旧账本标记 `Settled`（冻结）；
   * `carriedOver = floor(旧赛季 EffectiveProgress * CarryRatio)` 作为新赛季种子，
     计入新赛季有效进度 / 赛季已发基数并据此重算初始等级；
   * 每个玩家一条 `player_settled` WAL 记录，同时承载「旧冻结 + 新种子」，
     该记录**幂等**（`Settled` 账本不重复结转）。
4. 切换期间事件归属只看 `OccurredAt`：
   * 属于新赛季 → 新赛季入账（即使结转尚未跑完，种子稍后补齐，最终值确定）；
   * 属于旧赛季 → 其账本一旦冻结即返回 `season_settled`。
   不存在归属摇摆、进度丢失或重复计入。
5. 切换在中途失败 / 崩溃：已落盘的新赛季配置与已提交的结转不回滚；
   下次 `SwitchSeason` 或引擎重启时自动续做剩余玩家（见
   `TestSeasonSwitchResumedAfterCrash`）。

**重置语义汇总**

| 项目 | 跨赛季处理 |
|---|---|
| 原始/有效进度 | 旧账本冻结保留（领取/审计用）；新账本按 `CarryRatio` 结转种子 |
| 等级 | 按结转后的有效进度 + 新赛季阈值重新计算 |
| 每日已发额度 | 按 UTC 日期天然分桶，新赛季继续按天累计 |
| 赛季已发额度/封顶 | 新赛季从结转种子开始重新累计 |
| 已领取奖励 | 固化在旧账本，不受新规则 / 新进度影响 |
| 未领等级奖励 | 结算汇总 `UnclaimedLevels` 给出；旧账本保留期间仍可按旧规则领取 |
| 事件去重 | 以赛季为维度，新赛季是全新去重空间 |

## 6. 持久化与重启恢复

目录布局（`Dir` 非空时）：

```
Dir/
  meta/meta.log, meta.snap          # 规则注册、赛季配置
  shard-000/shard-000.log, .snap    # 每个分片独立 WAL 与快照
  ...
```

* 每条摄入 / prepared / committed / aborted / 结转记录一行 JSON，append 后 `fsync`。
* 快照为完整状态（玩家账本 + 去重集合 / 规则 + 赛季），
  写临时文件后原子 rename，随后截断 WAL；恢复时「快照 + 快照之后的日志」重放。
* 恢复后从最近一次已提交状态继续：进度不跳变，奖励不重发，
  半成品领取被明确终结为 aborted（**可区分已提交与未完成**）。
* `Dir` 为空时所有存储退化为 no-op，纯进程内运行，便于测试。

## 7. 并发与内存

* 玩家哈希到固定分片（默认 16），摄入 / 领取仅锁该分片；
  赛季切换逐分片处理，单个玩家或赛季的结算不会阻塞其他玩家摄入。
* 视图读取使用读锁；快照、摄入、领取、结转使用写锁。
* 内存有界：
  * 已结算赛季可用 `EvictSettled(seasonID)` 显式回收账本与去重集合；
  * 配置 `RetainSettled=N` + `EvictInterval` 后后台 janitor 自动只保留最近
    N 个已结束赛季（当前赛季不受影响）。
* 事件时间序列只保留当前赛季账本（随赛季淘汰释放）；
  每日额度是「每天一个整数」的 map，随账本一同回收。

## 8. 配置项

### `engine.Config`

| 字段 | 含义 | 默认 |
|---|---|---|
| `Dir` | WAL/快照目录；空串为纯内存 | 空 |
| `ShardCount` | 玩家分片数 | 16 |
| `Clock` | 墙钟时钟（视图选季 / 领取时间戳） | `time.Now` |
| `RetainSettled` | >0 时自动淘汰，仅保留最近 N 个已结束赛季 | 0（手动） |
| `EvictInterval` | janitor 运行间隔 | 1m |

### `SeasonConfig`

| 字段 | 含义 |
|---|---|
| `ID` | 赛季唯一标识 |
| `Start` / `End` | 半开区间 `[Start, End)`；`End` 零值表示开放式赛季 |
| `CarryRatio` | 结转到下一赛季的有效进度比例，`[0,1]`；**取值以旧赛季配置为准**（旧赛季决定放多少进度进入下一赛季） |
| `LevelThresholds` | 升序等级阈值 |
| `RuleVersion` | 本赛季绑定的奖励规则版本 |
| `Decay` | `Window` / `Gamma` / `MinFactor` 高频递减参数（缺省 60s / 0.8 / 0.1） |
| `DailyCap` | 每日有效进度上限，`<=0` 不限 |
| `SeasonCap` | 每赛季有效进度上限，`<=0` 不限 |

## 9. 测试覆盖

| 场景 | 测试 |
|---|---|
| 乱序 / 重复 / 迟到 | `TestBasicLevelProgressAndDedup`、`TestOutOfOrderEvents`、`TestEventAttributionDuringSwitch` |
| 高频递减与平滑 | `TestFrequencyDecaySmooth` |
| 每日 / 赛季封顶与明细 | `TestDailyAndSeasonCap` |
| 可复现 | `TestDecayDeterminismNoCap` |
| 多玩家隔离 | `TestPlayerIsolation`、`TestHighConcurrencyNoCrossContamination` |
| 赛季切换 / 结转 / 归属 | `TestSeasonCarryOverAndAttribution`、`TestClaimSettledSeasonStillClaimable` |
| 规则版本切换 | `TestRuleVersionIsolation` |
| 领取幂等 / 并发 | `TestClaimIdempotentAndConcurrent`、`TestConcurrentIngestAndClaim`（`-race`） |
| 重启恢复 | `TestRestartContinuesFromCommitted`、`TestSnapshotThenRestart`、`TestPreparedWithoutCommitIsAbortedOnRestart`、`TestSeasonSwitchResumedAfterCrash` |
| 内存有界 | `TestEvictSettledReleasesMemory`、`TestJanitorKeepsBoundedSeasons` |

## 10. 包结构

| 文件 | 职责 |
|---|---|
| `types.go` | 领域类型、配置、错误 |
| `engine.go` | 引擎装配与摄入 / 领取 / 切换主流程 |
| `progress.go` | 高频递减、封顶结算（纯函数）与等级推进 |
| `season.go` | 不可变赛季表与 `OccurredAt` 归属 |
| `rules.go` | 版本化奖励规则簿 |
| `claim*.go` | 两阶段领取（内联于 engine.go） |
| `shard.go` | 玩家分片与分片内多赛季账本 |
| `dedup.go` | `(赛季,玩家,事件ID)` 去重 |
| `persist.go` | 每分片 WAL + 原子快照 |
| `restore.go` | 快照加载与 WAL 重放、崩溃恢复 |
| `janitor.go` | 已结束赛季自动淘汰 |
