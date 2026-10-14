# 本地时间序列数据库（local-tsdb）技术文档

版本 1.0.0 · 纯 Python 标准库实现，零第三方依赖。

## 1. 概述

本项目参考三类成熟设计，实现了一个单文件本地时间序列数据库：

| 借鉴来源 | 落地点 |
|---|---|
| **SQLite 单文件设计** | 固定页（4096B）+ 头部页 + freelist 回收 + overflow 链页 + WAL 写前日志与崩溃恢复 |
| **InfluxDB 数据存储模型** | measurement/tags/fields/timestamp 数据模型、series key、列式 DataBlock、Gorilla 压缩、按时间边界裁剪块 |
| **TSDB 标准接口协议** | InfluxDB line protocol 写入 + InfluxQL 子集查询 + InfluxDB 兼容 HTTP API（`/ping` `/write` `/query`） |

代码布局：

```
tsdb/            # 数据库包
  model.py       # Point / series key / 字段类型
  compression.py# Gorilla 编解码（float XOR、int delta-of-delta）
  block.py       # DataBlock 列式块序列化
  index.py       # series -> blocks 索引 + tag 倒排
  storage.py     # PageStore：页式单文件 + WAL
  query.py       # QueryEngine：裁剪/合并/投影/聚合
  protocol.py    # line protocol 解析/序列化
  sql.py         # InfluxQL 子集（tokenizer + 递归下降）+ ResultSet
  http_server.py # HTTP API（http.server，单锁守护）
  core.py        # TimeSeriesDB 门面
demo/demo.py     # 端到端演示
tests/           # 121 个测试（单元 + 集成 + 性能）
```

## 2. 总体架构

```
┌─────────────────────────────────────────────────────────────┐
│  HTTP API (http_server)          Python API (core.TimeSeriesDB) │
│  /ping /health /write /query     write/select/aggregate/sql    │
├─────────────────────────────────────────────────────────────┤
│  InfluxQL 子集 (sql.SQLExecutor)   line protocol (protocol)      │
│  SELECT/聚合/GROUP BY time         parse_line/format_point       │
├─────────────────────────────────────────────────────────────┤
│                     QueryEngine (query.py)                    │
│  series 解析 → 块级时间裁剪 → 行过滤 → memtable 合并 → 投影/聚合   │
├─────────────────────────────────────────────────────────────┤
│  Index ─────────── DataBlock ─────────── PageStore             │
│  series→[(locator,min,max)]    Gorilla 列式块     页 + freelist │
│  tag 倒排（加载时重建）          min_ts/max_ts 裁剪   + WAL      │
└─────────────────────────────────────────────────────────────┘
```

写路径：`write()` 把 Point 缓冲进内存 memtable（按 series key 分组）；`commit()` 为每个 series 生成一个新的不可变 DataBlock，更新 Index，经 PageStore 的 WAL 原子持久化。读路径：QueryEngine 先解析匹配 series，再对每个块做 min/max 时间裁剪，只解码幸存行，最后合并未提交的 memtable 点并投影/聚合。

## 3. 存储层（SQLite 单文件借鉴）

### 3.1 页模型

- 整个数据库 = **一个文件** `<path>` + 可选 WAL 文件 `<path>.wal`；
- `PAGE_SIZE = 4096`，页号从 0 起；
- **页 0 是头部页**（对应 SQLite file header）：

```
magic "LTDB" | version u16 | page_size u16 | pad8
created_at i64 | last_write_at i64 (ns)
total_pages u32        # 高水位（已分配页数）
free_list_head i32     # freelist 头页号，-1 = 空
root_index_page i64    # Index blob 起始页
wal_seq u32           # WAL 帧计数
+ CRC32(上述字段)      # 头部完整性校验
```

### 3.2 Freelist（SQLite freelist 语义）

释放的页不截断文件，而是压入**链式空闲表**：页镜像前 4 字节（`>I`）改写为旧表头（哨兵 `0xFFFFFFFF`），头部页记录新表头。分配时优先弹 freelist，否则扩展 `total_pages`。

### 3.3 Blob API（overflow 链页）

大记录跨多整页透明存储：每内容页前 8 字节为 `>II(next_page, payload_len)`，`PAGE_CAP = 4088` 可用字节；`write_blob` 返回起始页号，`read_blob` 沿 `next_page` 链读回（末页 next = `-1`）。Index 与 DataBlock 都是这种 blob。

### 3.4 WAL 与崩溃恢复

- **提交**：重序列化头部 → 暂存脏页 → 向 `<path>.wal` 追加帧（每帧 `<QII>(seq, pageno, crc32)` + 完整 4096B 页镜像）并 `fsync` → 应用到主文件（数据页先、头部后）并 `fsync`；
- **打开**：`_replay_wal()` 顺序回放未 checkpoint 的帧——撕裂的帧头/页镜像/CRC 不符即停止（截断帧检测），有效帧幂等覆盖到主文件，随后删除 WAL。与 SQLite "recover-then-checkpoint" 同构；
- 因帧是**整页镜像**，回放天然幂等，无需 undo/redo 日志格式。

## 4. InfluxDB 数据模型与磁盘格式

### 4.1 Point 与 series key

`Point(measurement, tags, fields, timestamp)`，时间戳为纳秒（0 = 写入时取当前）。series key 为规范化字符串 `measurement[,tag1=v1][,tag2=v2]`，**tags 按名排序**，保证同一 tag 集合恒得同一 key。字段类型标签：`i`(int64) / `f`(float64) / `b`(bool) / `s`(str)，驱动压缩路由（bool 先于 int 判定，避免 CPython 子类陷阱）。

### 4.2 DataBlock 布局（自描述、长度前缀）

```
u16 nfields
(name_len u16, name bytes, ftype u8) * nfields     # 按名排序
i32 point_count
i64 min_ts ; i64 max_ts                            # 裁剪依据
u32 ts_bytes_len ; bytes ts_compressed            # encode_ints_dod
(u32 col_bytes_len ; bytes col_data) * nfields    # 与描述符同序
```

块内时间戳列用 delta-of-delta；每个字段列按类型路由：float → Gorilla XOR，int → delta-of-delta，bool/str → 原始（长度前缀）。`from_points` 取字段名并集、缺失值零填充，保证每列矩形可压。块**不可变**：新样本开新块而非改写旧块，解码结果可按进程生命周期缓存（`_block_cache`）。

### 4.3 Index

持久化仅 `series_key -> [(locator, min_ts, max_ts)]` 一张表（单 blob）；measurement 集合与 `(tag_name, tag_value) → series keys` **倒排索引**在加载时从 key 解析重建——身份只有一个事实源。`series_for_tags` 对多个 tag 条件做集合交集，支撑 `host='a' AND region='x'` 类查询。

## 5. 核心算法

### 5.1 Gorilla Float64 XOR（gauges）

首值原样存 64 bit；其后每值：`xor = prev ^ cur`——

- `xor == 0`（未变）→ 只写 **1 个 0 flag bit**，连续不变值坍缩为单比特流；
- 变化 → 1 flag bit + `skip = leading_zeros(xor, 64)`（8 bit）+ 低 `64 - skip` 位。

解码时高位按构造全零，故 `cur = prev ^ low` 直接还原。对缓变仪表值（温度/电压/CPU 占用率）极省：100k 点实测 **8.6 bytes/point**（含时间戳列与全部开销）。

### 5.2 Integer delta-of-delta（counters）

布局：`count`(varint) + 首值原样 + 每样本一个 **zigzag varint 的二阶差分**。单调计数器二阶差恒 0 → zigzag(0)=0 → LEB128 下仅 1 字节/点。zigzag `(n<<1)^(n>>63)` 把小幅度有符号数映射到小无符号数，varint 用 LEB128（7 bit/字节，`0x80` 续位）。

### 5.3 时间边界裁剪（TSM 式优化）

Index 为每 series 记录每个块的 `(locator, min_ts, max_ts)`。查询 `[start, end]`：

```
for (loc, bmin, bmax) in blocks(series):
    if start is not None and bmax < start: continue   # 整块在范围前
    if end   is not None and bmin > end:   continue   # 整块在范围后
    decode(loc); yield rows where start <= ts <= end
```

整块越界**不解码**；命中块内再做行级过滤。实测：8% 时间跨度查询 0.032s vs 全量扫描 1.12s（100k 行）。

### 5.4 查询执行路径

`match_series`（measurement 前缀 + tag 倒排交集）→ 逐 series `_iter_rows`（块裁剪 → memtable 合并，均按 `[start, end]` 过滤）→ `select` 投影/limit 后按 `(series_key, ts)` 稳定排序；`aggregate` 流式折叠 count/min/max/sum/mean（mean 用 `(sum, n)` 累加器避免二次遍历），支持按 tag 值分组。

## 6. 接口协议层

### 6.1 Line protocol（写入）

```
measurement[,tag1=v1][,tag2=v2] field1=fval[,field2=fval]... [timestamp]
```

- 首个未转义空格分隔 key 与 fields；`\` 转义空格/逗号/等号/反斜杠；
- 值类型后缀：`5i`→int64、`true/false`→bool、可解析数字→float、其余为字符串（可带引号）；
- 末位纯数字 token = 纳秒时间戳；HTTP `/write?precision=s|m|u|ns` 按 InfluxDB 语义换算。

### 6.2 InfluxQL 子集（查询）

```
SELECT <select_list> FROM <measurement>
       [WHERE <cond> (AND <cond>)*]
       [GROUP BY <tag | time(dur)> , ...]
       [LIMIT n]
SHOW MEASUREMENTS
```

- `<select_list>`：`*`、裸字段名或聚合 `count|sum|mean|min|max(field)`（`count()` 可省字段）；含聚合即进入聚合模式，**禁止与裸字段/`*` 混用**；非聚合查询不允许 GROUP BY；
- WHERE：`time >/>= /</<=` 右值为 epoch 整数、RFC-3339 字符串或 `now() ± <dur>`；tag 条件仅 `=`/`!=`（字符串字面量）；多条件 AND 链。
  **严格不等式语义**：整数纳秒域上 `time > t` → `[t+1, ∞)`，`time < t` → `(-∞, t-1]`（偏移一个 tick），与 InfluxDB 一致；
- duration 词法：`1h` 被 tokenize 为 num + ident，由 `_read_duration()` 消费；**裸数字视为 ns**（故 `time(100)` 合法）；单位 ns/us/ms/s/m/h/d/w；
- GROUP BY time(B)：桶 = `ts // B * B`；与 tag 分组组合时每 (tag 组, 桶) 一行，列序 `[time?, group tags..., <func>_<field>|count]`。

### 6.3 HTTP API（InfluxDB 兼容）

| 方法 | 路径 | 行为 |
|---|---|---|
| GET | `/ping` | `204` 空响应（liveness） |
| GET | `/health` | `200 "OK"` |
| POST | `/write?precision=ns\|s\|m\|u` | line protocol 批量入库；成功 `204`，解析错误 `400` JSON |
| GET/POST | `/query?q=<InfluxQL>` | 默认 CSV（`text/csv`）；`Accept: application/json` 时返回 `{"columns": [...], "rows": [...]}`；SQL 错误 `400` |

单进程 + 一把锁守护全部 DB 访问（本地工具/演示定位，非多客户端服务）。handler 异常统一兜底为 `500 {"error": ...}`，不会裸断连接。

## 7. 测试结果

### 7.1 单元 / 集成测试（`python -m unittest discover -s tests`）

**Ran 121 tests — OK**（约 5.7s；含 `-W error::ResourceWarning` 无资源泄漏）。

| 模块 | 用例数 | 覆盖点 |
|---|---:|---|
| test_model | 17 | Point/series key 规范化、类型推断（bool≠int）、长度前缀名 |
| test_compression | 15 | XOR/delta-of-delta 往返、不变值坍缩、负数 zigzag、varint 边界 |
| test_block | 4 | DataBlock 序列化往返、零填充矩形列、min/max ts |
| test_index | 9 | series 注册/块追加、tag 倒排交集、blob 持久化往返 |
| test_query | 11 | 块裁剪正确性、memtable 合并、投影/limit/聚合分组 |
| test_storage | 8 | 页分配/freelist 回收、WAL replay、撕裂帧忽略、头部 CRC 损坏检测 |
| test_protocol | 12 | line protocol 解析/序列化往返、转义、类型后缀 |
| test_engine | 6 | 端到端：写→提交→读回、重开持久化、多块裁剪间隙 |
| test_sql | 27 | tokenizer/递归下降、严格不等式 ±1 tick、now()±dur、RFC-3339、GROUP BY time/tag、CSV/JSON 渲染 |
| test_http_server | 8 | /ping 204、/health、write→query 闭环、precision=s、CSV/JSON 双格式、400/404 |
| **合计** | **121** | |

### 7.2 性能测试（tests/test_perf.py，100k 点 × 8 series）

数据：`cpu,host=h<i> usage=(j%100)/100 @ BASE+j`，i∈[0,8)、j∈[0,12500)，经 line protocol `write_lines` + `commit`。

| 指标 | 实测 |
|---|---|
| 写入吞吐（parse+buffer+commit） | **~44k points/s**（100k 点 ≈ 2.3s；上限断言 <60s） |
| 文件大小 | **860,160 B = 8.60 bytes/point**；原始 line protocol 文本 4,499,999 B → **~5.2x 于文本载荷**（压缩 sanity 断言 <2MB） |
| 全量扫描 100k 行 | ~1.12s |
| 窄范围裁剪（8% 时间跨度，[6000,7000]） | **~0.032s**，返回行数精确 = 8×1001 = 8008（断言） |
| 全量 mean 聚合 | ~0.99s，结果 0.495（places=6 精确断言） |

### 7.3 Demo（`python demo/demo.py`，EXIT=0）

四段端到端：① API `Point` + line protocol 写 cpu/mem/load 共 30 点 → commit（文件 24,576 B）；② `select`/`aggregate` 读回（mean usage = 0.200）；③ InfluxQL：`SELECT * ... LIMIT 3` CSV、`GROUP BY time(100)` 桶均值、`SHOW MEASUREMENTS → ['cpu','load','mem']`；④ 真实 socket 上 `GET /ping→204`、`POST /write→204`（net/iface=eth0）、`GET /query` JSON → `columns=['count'], rows=[{'count':1}]`。

## 8. 已知限制

- **单写者**：一把进程内锁，无跨进程文件锁；非多客户端服务定位；
- **块只增不改**：每次 commit 为有写入的 series 开新块，旧数据块不回收（freelist 仅回收 Index blob 页），长期高频提交会累积小文件——未实现 compaction/合并；
- **无保留策略/降采样/连续查询**：时间裁剪只到块级 min/max，无 TSM 式二级键值缓存与预聚合 rollup；
- **聚合为内存折叠**：解码后流式计算，无部分聚合物化；
- InfluxQL 为**子集**（无子查询/正则/多 measurement join），line protocol 未实现 `#` 注释行以外的扩展语法。
