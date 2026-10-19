# 看板与告警

本目录是 `msvcservice` 的可观测性配置，compose 与集群共用同一份文件：

| 文件 | 用途 |
|---|---|
| `dashboards/msvc-red.json` | 看板：RED、消息、任务、熔断、Go 运行时、数据库连接池、积压时长、按错误码的失败，最后一段是指标速查 |
| `alerts.yml` | 告警规则，Prometheus 规则格式，每条带 `service` / `alertgroup` / `severity` 三个路由标签和写了 LogQL、SQL 的 runbook |
| `import-dashboard.sh` | 把看板导入（覆盖）到集群的 Grafana |
| `import-alerts.sh` | 把规则导入成 Grafana-managed、页面可编辑的告警 |

本地 `make up` 之后 Prometheus 直接加载 `alerts.yml`，Grafana 通过 provisioning 加载 `dashboards/`，不用导入。

## 导入到集群

```bash
# 凭证：Grafana → Administration → Service accounts → 建一个 Editor 的 token
export GRAFANA_TOKEN=...
# 直连，或让脚本自己 port-forward
export GRAFANA_URL=https://grafana.example.com
# export GRAFANA_PORT_FORWARD="-n monitoring svc/grafana 3000:80"

deploy/grafana/import-dashboard.sh
deploy/grafana/import-alerts.sh --yes            # 默认 folder 是服务名，数据源 uid 是 prometheus
```

脚本刻意不从集群的 Secret 里读管理员密码：chart 播种的密码在有人从页面改过之后就是陈旧的，拿到一个"看起来对、实际 401"的凭证比直接要求手输更难排查。

### 导入告警后必做

每条规则 → Edit → **Configure no data and error handling** → No data = **Normal**。
计数类指标在第一次发生前根本没有序列，查询为空会被判成 NoData，而手建规则的默认 NoData 行为是 Alerting，系统完全健康时反而一直报警。`import-alerts.sh` 导入后会回读规则列表，改完逐条核对。

### 通知路由

规则上的三个标签给通知策略用：`service` 把整个服务路由到一个群，`alertgroup`（http / messaging / tasks / database / infra）把业务失败与依赖故障分开，`severity` 分紧急度。Grafana 没有原生飞书集成，用 Webhook contact point 加自定义 payload。

## 改了之后怎么回写

导入的看板和告警存在 Grafana 的数据库里，和仓库没有任何自动同步。页面上调好之后要回写，否则下次导入会覆盖：

- 看板：Export → Export as JSON，覆盖 `dashboards/msvc-red.json`。不要勾"export for sharing externally"，否则数据源会变成模板变量。
- 告警：改阈值这类小改动直接改 `alerts.yml` 的 `expr` / `for` 再导入；页面导出的是 Grafana 自己的三段式格式，和这份文件不是一回事。

## 写查询的三个坑

1. **队列和 outbox 的 gauge 用 `max by`，不要 `sum`。** 每个副本上报的都是同一个全局值（fixer 和投递器各自查一次库），两副本 `sum` 直接翻倍。
2. **选择器用 `job`，而且每条 expr 都要带 `{job=~"msvcservice-.*"}`。** 它是 ServiceMonitor 打的 target label（不设 `jobLabel`，取 Service 名：`<release>-api` / `<release>-worker`；compose 里是 `msvcservice-api` / `msvcservice-worker`，所以 release 名要等于服务名）。集群的 Prometheus 抓所有服务，没有这个选择器，导入的规则会对别的服务报警（`up == 0` 更是对整个集群）。服务自己不暴露 `service` 标签，避免和 target label 撞名后被改成 `exported_service` 静默失配。
3. **死信按 `reason` 过滤。** `exhausted`（重试用尽，要告警）和 `non_retryable`（handler 主动放弃，日常噪声）量级差很多；任务的 `msvc_tasks_dead_total{reason}` 同理。

## 自检

```bash
# 规则语法（用本地已有的 Prometheus 镜像）
docker run --rm --entrypoint promtool -v "$PWD/deploy/grafana/alerts.yml:/alerts.yml:ro" prom/prometheus:v3.0.1 check rules /alerts.yml
# 看板 JSON 合法且面板不重叠
python3 - <<'PY'
import json
d = json.load(open('deploy/grafana/dashboards/msvc-red.json'))
cells, bad = {}, 0
for p in d['panels']:
    g = p['gridPos']
    for y in range(g['y'], g['y'] + g['h']):
        for x in range(g['x'], g['x'] + g['w']):
            if (x, y) in cells: print('overlap:', cells[(x, y)], '<->', p['title']); bad += 1
            cells[(x, y)] = p['title']
    if g['x'] + g['w'] > 24: print('out of grid:', p['title']); bad += 1
print('panels=%d problems=%d' % (len(d['panels']), bad))
PY
```

## 指标速查

与看板最后一段的文本面板保持一致，改一处记得改另一处。

| 指标 | 类型 | 标签 | 怎么读 |
|---|---|---|---|
| `http_server_request_duration_seconds` | histogram | `http_route`, `http_response_status_code` | RED 三件套的来源（otelhttp） |
| `msvc_http_problems_total` | counter | `http_route`, `http_response_status_code`, `error_code` | 失败按错误码，告警可只盯一个码 |
| `msvc_messaging_outbox_pending` / `_oldest_pending_age_seconds` | gauge | `job` | 每个副本报同一个全局值：max，不要 sum |
| `msvc_messaging_consumed_total` | counter | `queue`, `outcome` | ok / duplicate / retry / dead_letter / requeued |
| `msvc_messaging_dead_lettered_total` | counter | `queue`, `reason` | exhausted 才告警，non_retryable 是日常噪声 |
| `msvc_tasks_queue_depth` / `_oldest_pending_age_seconds` | gauge | `queue` | max，不要 sum |
| `msvc_tasks_completed_total` / `_retried_total` / `_dead_total{reason}` / `_reclaimed_total` / `_misfired_total` | counter | `queue` | dead 的 reason：exhausted / discarded |
| `pgxpool_acquired_connections` / `_idle_connections` / `_max_connections` | gauge | `db_client_connection_pool_name` | acquired 贴着 max = 池满 |
| `pgxpool_empty_acquire_total` / `_empty_acquire_wait_time_nanoseconds_total` | counter | | 排队次数与排队时长；慢 SQL 的 elapsed 不含这段 |
| `msvc_db_slow_queries_total` | counter | `logged` | logged=false 是被限速吞掉的行，总量两者相加 |
| `msvc_resilience_breaker_state_changes_total` | counter | `breaker`, `to` | to=open 即下游被熔断 |
