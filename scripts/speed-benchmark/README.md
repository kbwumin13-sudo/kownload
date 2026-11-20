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

报告保留每个任务的状态、各次试验与失败率。成功耗时仅在所有对照组完整成功时用于报告时间变化。混合结果可使用全样本的截止时间计分，失败或超时的样本按该场景的截止秒数计入。fixture 统计物理连接数、请求数、字节数和请求占用秒数，包含已取消及失败请求。短暂缓冲和 TCP 中仍在途的字节也可能已计入服务端发出的字节数。

在 macOS 上可克隆真实 App 验证真实应用的持久化与交接。下面命令将克隆包设为独立标识并装入对应的计时库。App 试验使用独立 HOME、Unix socket 与 Bolt 数据库，测量真实 App 进程内的 `resolve → create` 或批量 API，并重启核对原任务 ID 及完成状态。

```sh
python3 -I scripts/speed-benchmark/prepare-app.py \
  /Applications/kownload.app /tmp/core.dylib /tmp/kownload-speedbench.app
python3 -I scripts/speed-benchmark/bench.py --scenario app-small \
  --fixture-bin /tmp/speed-fixture --variant app=/tmp/kownload-speedbench.app \
  --output /tmp/app-small.json
```

`KOWNLOAD_SPEEDBENCH_TRACE` 由测速脚本为每次试验设置。探针只在 `speedbench` 构建中启用。CPU 和峰值 RSS 指标来自装载内核的测试进程。独立 App 的结果只包含端到端时间、请求和状态。
