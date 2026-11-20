# Next Wave 源码实施与验收记录

依据：`vestrail_next_wave_execution_plan.md`。源码：`D:\Code\artex-god-integration`。已直接修改源码，保留原工作区改动，没有发布、部署或运行真实评估任务。

## 已实现

- Phase 0：API token 与本地估算的 micro 迟滞修复；108K 软窗口、30K 增长门槛；冷压缩缓存前缀与短索引；会话数、首条输入 P50 和样本数。
- Phase 1：summary / brief 分离，超长交底原件保留并交给 Worker；提示生命周期和相关性过滤；RunScript 原子写入执行、审批及输出留档；纯 Go 意图去重；有事实时 Planner 移除 Bash；收尾关闭 Thinking；独立问答清理旧 reasoning。
- Phase 2：工作快照 Fork、零计费历史种子、同执行线目录与串行调度、四级交接和条件不满足时独立会话降级；活 Planner 会话、增量节点更新、8 轮 / 48K 等条件切 Epoch；前 25 次探测豁免、15 次重复纠偏、23 次重复限制探测但放行写回、再撞三次才结束；四次无成果降并发，成果或目标提示重置。
- 配套：归档和删除纳入工作快照及接力元数据；活动统计只记录当轮新增用量；保留模型重试前已写入的成果；修复旧测试清理顺序及 Windows 后台任务测试的固定等待。

## 必要修正

1. 现有数据库 hint 状态只允许 active / consumed。复用 consumed，在已有 JSON 保存 hint_status=resolved 和处理原因；不迁移表结构，前端保持原状态表示。
2. InputTokens 已包含缓存输入，不再叠加 cache_read / cache_write。
3. 旧系统提示固定上游“唯一意图”，会与新交底冲突。接力改用稳定的执行线背景，最新明确交底指定当前任务；AutoCompact 保留该交底，写回工具验证当前归属。
4. 原始 transcript 不保存 micro 清洗，接力使用与末尾 UUID 匹配的工作快照。快照过期、损坏或配置变化时明确降级，保留原始证据。
5. 历史 reasoning 清理会改变前缀：独立问答默认清理，执行线和规划 Epoch 保留历史 reasoning。
6. 增量使用节点版本变化与删除，包含既有意图终态、提示处理、目标和 digest 更新；超限节点明确提供 ID。
7. Fork 给下游新会话 ID。DistinctSessions 包含这些会话，不等于冷启动次数，不能用它直接证明 Worker 冷启动下降 75%。

## 开关

以下开关默认开启，设置 0 / false / off / no 关闭对应优化：

| 环境变量 | 控制项 |
| --- | --- |
| VESTRAIL_OPT_COMPACTION | 软窗口与压缩参数、冷索引 |
| VESTRAIL_OPT_DATA_CONTRACT | 短索引、提示视图 |
| VESTRAIL_OPT_RUNSCRIPT | Worker 注册 RunScript |
| VESTRAIL_OPT_INTENT_DEDUP | 入队去重 |
| VESTRAIL_OPT_PLANNER_SLIM | 有事实时移除 Planner Bash |
| VESTRAIL_OPT_REASONING_HISTORY | 独立会话历史思考清理 |
| VESTRAIL_OPT_SETTLEMENT_THINKING | 收尾关闭 Thinking |
| VESTRAIL_OPT_LANE_WORKER | 创建新的执行线接力 |
| VESTRAIL_OPT_STICKY_PLANNER | Planner 会话复用 |
| VESTRAIL_OPT_PROGRESS_GUARD | 停滞守卫、降并发 |

关闭新接力后，已共用目录的执行线继续串行。NOA 开启时不建立新的工作快照接力或 Sticky Planner。

## 已运行验证

- 主仓构建、go vet 和差异格式检查通过。
- agent / guard / db 全量测试通过；server 的同线领取、等待期间暂停、进展守卫、任务连续运行、用量接口、归档与删除定向测试通过。
- norma 的 llm / compaction / tool / harness / agentcore / transcript 全量测试通过。
- 前端 10 项 token / 导出测试、TypeScript 检查通过。合成预览覆盖 1280px 桌面和 390px 手机；表头保持单行、表格横向滚动，范围、维度、刷新及错误状态已验证。
- 浏览器中已核对导出生成的 Markdown 内容和文件名；自动化没有捕获下载完成事件，未将下载落盘宣称为已验证。
- 所有数据库验证使用本次独立启动的 wave_test 实例，无用户数据导入，无真实模型调用。

## 实际任务验收仍待运行

费用下降 84–90%、真实缓存命中 >=90%、首轮 <=15K、检出率及复杂链能力对照，需要在相同授权靶场、模型和范围下执行 A/B 后核对。此次交付源码与合成回归，不把方案预期写成实测结果。

## 本轮五处收口修复

- Planner 从 SDK `LastInputTokens()` 读取最后一次模型调用的输入；累计用量仍由 `Usage()` 保存。
- 大块态势按字段 JSON 内容的 SHA-256 判定变化；未变字段不重复发送，字段删除显式发送 null。每轮保留当前队列和必要计数。
- `add_intent` 的批量元素使用完整对象 Schema，与单条共用 summary、brief、continue_intent_id、asset_ids、parent_ids、priority，批量元素要求 summary。
- 子目标列表移出 Epoch 配置摘要；prove_goal 状态变化通过增量发送。顶层目标、工具或配置变化仍触发换班。
- 接力成功跳过 Worker 概览查询，Fork 失败时才补取；优化提示视图与旧提示查询互斥。

新增回归：`TestPlannerEpochLastInputTokNotCumulative`（单次和多次模型调用）、`TestPlannerDeltaOmitsUnchangedHeavyFields`、`TestAddIntentBatchSchemaExposesBriefAndContinue`、`TestWorkerLaneForkSkipsOverviewQuery`、`TestPlannerDeltaClearsRemovedHeavyFields`。已通过定向运行；SQL 跟踪确认成功接力无概览查询，优化路径无旧提示查询，关闭优化仍走旧查询。

本轮完整验收已通过：主仓构建、go vet、agent / guard / db 全量测试，以及 norma llm / compaction / tool / harness / agentcore 全量测试。真实模型的 Token 降幅和费用变化未执行 A/B 测量。
