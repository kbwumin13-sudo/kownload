# HTTP 下载测速

在各候选提交上，用同一个 Go 版本和同一份计时探针构建动态库。用相同的 fixture 和场景交替运行各版本。

```sh
go build -tags 'nosqlite speedbench' -buildmode=c-shared -o /tmp/core.dylib ./bind/desktop
go build -o /tmp/speed-fixture ./scripts/speed-benchmark/fixture
python3 -I scripts/speed-benchmark/bench.py --scenario probe-delayed \
  --fixture-bin /tmp/speed-fixture --variant baseline=/tmp/baseline.dylib \
  --variant candidate=/tmp/core.dylib --output /tmp/probe-delayed.json
```

阶段以提交本地 API 为起点。首段是提交至收到首批有效数据，持续传输至收到 90% 的唯一字节范围，尾段至最后一批有效数据，交付阶段至 Core 发布已提交的完成状态。预取纳入计时。采样只用于等待完成和记录自适应状态。下载文件另行检查大小和 SHA-256。

## 持续限流与配对统计

`b2`、`b2-date`、`b2-none` 只拒绝预定次数的 Range 请求。`b2-bucket`、`b2-bucket-date`、`b2-bucket-none` 在整次试验中实施令牌桶，限流来源每秒补充 3 个令牌，突发容量为 3。三个桶场景分别返回 429 与秒数、503 与 HTTP 日期、429 且无 Retry-After。`cooldown-slots` 用一个全局任务槽验证另一来源可以继续。

```sh
python3 -I scripts/speed-benchmark/bench.py --scenario b2-bucket \
  --fixture-bin /tmp/speed-fixture --variant build50=/tmp/build50.dylib \
  --variant build51=/tmp/build51.dylib --pairs 20 --max-pairs 20 \
  --output /tmp/reliability-b2-bucket.json
```

每次试验结束会原子更新输出文件。全部完成后写入 `completed: true` 和汇总。配对顺序 AB、BA 交替。用独立文件补样时，`--pair-offset` 可指定起始配对序号，在合并相同设计的记录后重新统计。

- Runner 先读任务状态、脱敏错误并检查文件大小与 SHA-256，再停止独立 Core 或 App。fixture 排空后才记录最终计数。未排空或没有可信最终计数的结果标记为 `statsIncomplete`，保留原始记录，在汇总中排除。配对分析要求同对的两次试验都有有效证据。
- `taskSuccessRate` 以有效试验的全部预期文件为分母，成功要求任务完成、文件校验通过、完成事件匹配。`batchSuccessRate` 以有效批次为分母。App 重启恢复结果单独计入批次通过条件。结果同时给出来源分组和逐任务失败原因，试验输出使用 fixture 文件名、分类错误和数值，不转存 Core 原始日志或 HTTP 凭据。
- 任务成功率 95% 区间按批次聚类重采样，固定种子，10,000 次。批次成功比例无变化时，使用按批次数作有效样本量的 Wilson 区间，以免全成功或全失败时给出零宽区间。配对差值为后一个变体减前一个变体，区间重采样以整对为单位。配对符号检验排除零差值，差值无变化时不列 bootstrap 区间。
- 成功批次的端到端用时列出 p50 和 p95。截止时间计分对有效但失败的批次使用该场景预先设置的截止秒数，避免快速失败被误报为提速。通过状态与哈希校验的文件仍计入阶段耗时统计，即使同批另有任务失败。
- `server.requestTimeline` 保存 fixture 的请求到达、响应头决策和结束时间，均为相对 `server.epochUnixNs` 的纳秒。Core 的 `budgetTimeline` 使用绝对 UNIX 纳秒。可据此核对冷却下限、单探测与后续恢复的请求次序。计数包含物理连接、请求数、429/503 响应、Retry-After 类型及等待时间、请求占用和发送字节。重复传输字节是已写出的同一 fixture 文件的字节区间重叠，含服务端缓冲和 TCP 在途字节。
- 诊断构建在任务行记录 `retries` 和 `deferrals`。前者计 HTTP 流重试决定，包括共享恢复波次重新排队，后者计任务实际让出运行槽并入队的次数。这两个计数与服务端收到的请求数独立。旧探针缺少的计数用 null 表示。

在 macOS 上可克隆真实 App 验证真实应用的持久化与交接。下面命令将克隆包设为独立标识并装入对应的计时库。App 试验使用独立 HOME、Unix socket 与 Bolt 数据库，测量真实 App 进程内的 `resolve → create` 或批量 API，并重启核对原任务 ID 及完成状态。

```sh
python3 -I scripts/speed-benchmark/prepare-app.py \
  /Applications/kownload.app /tmp/core.dylib /tmp/kownload-speedbench.app
python3 -I scripts/speed-benchmark/bench.py --scenario app-small \
  --fixture-bin /tmp/speed-fixture --variant app=/tmp/kownload-speedbench.app \
  --output /tmp/app-small.json
```

`KOWNLOAD_SPEEDBENCH_TRACE` 由测速脚本为每次试验设置。探针只在 `speedbench` 构建中启用。CPU 和峰值 RSS 指标来自装载内核的测试进程。独立 App 的结果只包含端到端时间、请求和状态。
