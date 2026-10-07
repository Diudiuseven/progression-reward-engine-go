# progression-reward-engine-go

玩赚（Play-and-Earn）模式下的成长进度与奖励结算引擎。纯进程内状态 + 本地文件持久化，
不依赖外部存储，所有结论可在本地复现与验证。

## 链路总览

```
玩法事件 Event ──Ingest──> 赛季归属 → 去重 → 递减/封顶结算 → 等级推进
                                                              │
                              奖励领取 Claim <── 版本化规则 <──┘
赛季推进 SwitchSeason ──> 两阶段提交 + 惰性结转（重置比例 / 跨赛季结转）
```

## 成长模型

- **事件摄入**：每个事件携带全局唯一 `ID`、`PlayerID`、`Points` 与 `EventTime`。
  赛季归属只由 `EventTime` 落在哪个赛季的 `[Start, End)` 区间决定，与到达时间无关，
  因此切换过程中到达的事件归属确定，不会摇摆。
- **去重**：`SeenEvents` 按玩家按赛季记录已处理事件 ID，重复事件返回 `reason=duplicate`，
  不重复累计；重启后去重集合依然有效。
- **迟到事件**：赛季一旦结算（被切换出去），属于该赛季的迟到事件返回 `reason=late`，
  不改写任何状态——已对外可见的等级与奖励结论不被追溯。
- **收益递减与封顶**（见 `decay.go`）：
  - 日内累计采用平滑递减曲线 `D(x) = DayCap * (1 - exp(-x / DayKnee))`，
    单次事件有效进度 = `D(当日已累计 + raw) - D(当日已累计)`，
    高频获得时边际收益平滑衰减并渐近封顶于 `DayCap`；`DayKnee` 越小衰减越快。
  - 赛季维度为硬上限 `SeasonCap`，超出部分直接截断（`reason=season_cap`）。
  - 账本使用 float64（IEEE754 确定性运算，同输入同输出），每次结算落一条
    `GainRecord{Raw, Effective, Reason, DayTotalAfter, SeasonTotalAfter}`，可解释、可复现。
- **等级**：`LevelThreshold` 按累计赛季进度推进，等级由进度纯函数导出，不单独落账，
  因此任何时刻读取到的等级与进度必然自洽。

## 奖励规则版本化

- 规则以 `RuleVersion` 为单位注册，携带 `EffectiveFrom`，注册后不可变。
- 每个赛季通过 `SeasonConfig.RuleVersion` 绑定固定规则版本：同一赛季内同一份进度
  产生的可领取奖励口径恒定，不受后续规则更新影响；新赛季可绑定新版本。
- `RuleSet.EffectiveAt(t)` 支持按指定时刻查询生效版本（用于运营排期）。
- 已发放的奖励记录在 `Claimed` / `ClaimedRewards` 中，规则更新不会追溯改写历史发放。

## 奖励领取的幂等与并发

- `Claim(playerID)` 发放当前等级全部未领奖励；已发放的记录在 `Claimed` 中，
  重复领取返回 `Granted=[]`、`Already=<首次发放口径>`，绝不重复发放。
- 每个玩家的所有变更由分片锁（64 条 stripe）串行化；领取与结算并发时，
  任一时刻读取到的都是某个自洽完整版本（视图返回拷贝快照）。
- 单玩家操作 = 单次原子文件写（tmp + rename），崩溃不会残留半份领取记录。

## 赛季重置语义

- `SwitchSeason(now)` 结算当前赛季并激活覆盖 `now` 的赛季（中间跳过的赛季一并结算）。
- **结转**：玩家下次被访问时惰性结转——从停留赛季起，逐赛季乘上被退出赛季的
  `CarryRatio`（`CarryRatio=0.3` 表示保留 30%，其余重置），跨多个赛季也只会逐季
  连乘一次，不会重复结转或丢失。
- 结转时清空当赛季的去重集合、日桶与结算记录；`Claimed` 的 key 含赛季 ID，
  历史赛季已领奖励永久保留。
- 切换采用两阶段提交（`switch.log` 的 begin/commit + 原子 `season.json`）：
  崩溃在切换中途时，恢复流程按 begin 载荷幂等重做，归属不摇摆。

## 失败恢复

- 玩家状态写穿到 `players/<id>.json`（原子替换）；赛季元信息在 `season.json`；
  切换日志 `switch.log` 每行带 CRC32 校验。
- 恢复时：CRC 校验失败的尾部半行视为未提交并截断；存在无配对 commit 的 begin
  时按载荷重做切换；玩家状态从各自的已提交文件继续，进度不跳变、奖励不重复。

## 并发与内存

- 赛季读取走 `RWMutex` 读锁，切换只更新元信息（O(1)），不阻塞其他玩家摄入。
- 玩家状态按需加载 + LRU 缓存（`MaxCachedPlayers`），逐出不丢数据（已写穿）；
  日桶只保留最近 `DayBucketsKept` 天、结算记录只保留最近 `MaxGainRecords` 条，
  长时间运行内存不随事件量增长。

## 配置项（`Config`）

| 字段 | 说明 | 默认 |
|---|---|---|
| `DataDir` | 本地持久化目录 | 必填 |
| `Rules` | 规则版本列表（`DayCap`/`DayKnee`/`SeasonCap`/等级阈值/奖励） | 必填 |
| `Seasons` | 赛季列表（区间、`CarryRatio`、绑定规则版本） | 必填 |
| `MaxCachedPlayers` | 玩家状态内存缓存上限（LRU） | 1024 |
| `MaxGainRecords` | 每玩家保留的结算解释记录条数 | 32 |
| `DayBucketsKept` | 每玩家保留的日桶数量 | 7 |
| `Now` | 时钟注入（测试用） | `time.Now` |

## 验证

```bash
go test ./...          # 全部场景测试
go test -race ./...    # 竞态检测
```

测试覆盖：事件乱序/重复/迟到、收益封顶与递减、赛季切换与结转、规则版本切换、
领取幂等与并发、重启恢复、赛季切换崩溃重做、多玩家并发与缓存有界。
