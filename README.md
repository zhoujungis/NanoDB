# NanoDB

NanoDB 是一个用 Go 编写的轻量级嵌入式文档数据库（NoSQL），当前版本 `0.1.0`。

它不依赖任何外部服务，以 JSON 文档和集合为数据模型，内置快照 + WAL 持久化、二级索引、原子批量写入和显式事务，并提供命令行、交互式 shell 与 REST 三种使用方式。

- 模块：`nanodb`（Go 1.22+，无第三方依赖）
- 存储目录：默认 `./nanodb-data`，包含 `snapshot.json` 与 `wal.log`
- 单进程、可嵌入，也可作为独立 CLI 或 HTTP 服务运行
- 适合中小规模数据、工具类项目、桌面/边缘场景；不适合作为多节点或超大数据集的生产数据库

## 目录

- [特性](#特性)
- [环境与构建](#环境与构建)
- [快速开始](#快速开始)
- [数据模型](#数据模型)
- [查询](#查询)
- [更新](#更新)
- [二级索引](#二级索引)
- [原子批量写入](#原子批量写入)
- [显式事务](#显式事务)
- [命令行](#命令行)
- [HTTP API](#http-api)
- [Go 嵌入](#go-嵌入)
- [持久化与恢复](#持久化与恢复)
- [性能参考](#性能参考)
- [测试](#测试)
- [项目结构](#项目结构)
- [已知限制](#已知限制)
- [常见问题](#常见问题)

## 特性

- **文档模型**：集合内直接存 JSON 对象，无固定表结构；`_id` 必须是字符串，未提供时自动生成带时间前缀的 ID。
- **持久化**：快照 + 预写日志（WAL）；默认每次写入 `fsync`；WAL 超过阈值自动压缩；崩溃后自动重放，尾部撕裂记录会被安全截断。
- **查询**：字段等值、比较、集合、存在性、正则、逻辑组合；支持多字段排序、分页和字段投影；排序键预抽取，带 `limit` 的排序自动走 Top-N，避免全量排序。
- **二级索引**：单字段有序索引，支持等值、`$in`、范围查询和数组 multikey；可为索引添加唯一约束；查询计划自动选择候选集最小的索引。
- **原子批量写入**：一组写操作要么全部生效，要么全部不生效，成功时只落一条 WAL 记录。
- **显式事务**：`Begin` / `Commit` / `Rollback`，集合快照读 + 提交时冲突检测，详见 [事务隔离级别说明](docs/transaction-isolation.md)。
- **并发安全**：同一个 `*DB` 实例可被多个 goroutine 并发使用。
- **多入口**：交互式 shell、一次性 CLI 命令、REST 服务，覆盖全部核心能力。

## 环境与构建

要求：

- Go 1.22 或更高版本
- 不需要 cgo，也不需要任何外部数据库或服务
- 同一数据目录同一时间只能被一个进程打开

构建到 `bin/`：

```bash
go build -o bin/nanodb ./cmd/nanodb
```

也可以直接安装到 `GOBIN`：

```bash
go install ./cmd/nanodb
```

## 快速开始

### 1. 基本读写

```bash
# 插入（自动生成 _id）
./bin/nanodb -db ./data put users '{"name":"Alice","age":30}'

# 指定 _id
./bin/nanodb -db ./data put users '{"_id":"u2","name":"Bob","age":24}'

# 读取
./bin/nanodb -db ./data get users u2

# 条件查询 + 排序 + 分页
./bin/nanodb -db ./data find users '{"age":{"$gte":18}}' -sort age:-1 -limit 10

# 更新
./bin/nanodb -db ./data update users u2 '{"$inc":{"age":1}}'

# 统计与删除
./bin/nanodb -db ./data count users '{"age":{"$gt":20}}'
./bin/nanodb -db ./data delete users u2
```

`-db` 指定数据目录，默认 `./nanodb-data`；目录不存在时会自动创建。所有命令成功时把结果以缩进 2 空格的 JSON 打印到 stdout：

```text
$ ./bin/nanodb -db ./data put users '{"name":"Alice","age":30}'
{
  "_id": "0192a1b2c3d4e5f6a7b8c9d0e",
  "age": 30,
  "name": "Alice"
}

$ ./bin/nanodb -db ./data collections
[
  {
    "name": "users",
    "count": 1
  }
]

$ ./bin/nanodb -db ./data stats
{
  "dir": "./data",
  "collections": 1,
  "documents": 1,
  "indexes": 0,
  "wal_bytes": 118,
  "sync": true
}
```

失败时退出码为 `1`，错误信息以 `错误:` 前缀写入 stderr，例如：

```text
$ ./bin/nanodb -db ./data get users missing
错误: 文档不存在: users/missing
```

### 2. 交互式 shell

不指定命令时进入 shell：

```text
$ ./bin/nanodb -db ./data
NanoDB 0.1.0 交互式 shell。JSON 参数请用单引号包裹。
nano> put users '{"name":"Alice","age":30}'
nano> find users
nano> begin
事务已开始
nano(tx)> put users '{"name":"Bob"}'
nano(tx)> commit
事务已提交
nano> exit
```

事务模式下提示符变为 `nano(tx)>`；直接退出或输入 EOF 会自动回滚未提交事务。

### 3. 启动 HTTP 服务

```bash
./bin/nanodb -db ./data serve -addr :8080 -token secret
```

- `-addr` 默认 `:8080`
- `-token` 为空表示不鉴权；设置后需要携带请求头

```bash
curl localhost:8080/healthz
curl -H 'X-NanoDB-Token: secret' localhost:8080/v1/users
```

## 数据模型

```text
数据库目录
├── snapshot.json   # 某一时刻的全量数据与索引定义
└── wal.log         # 快照之后的增量操作日志（JSON Lines）
```

```text
数据库
└── 集合（collection，类似表但无固定结构）
    └── 文档（JSON object，必须含字符串 _id）
```

规则：

- 文档必须是 JSON 对象；`_id` 必须是非空字符串。插入时未提供会自动生成，格式为 13 位十六进制毫秒时间戳 + 12 位随机十六进制字符。
- 写入会深拷贝文档，调用方之后修改原对象不会影响数据库；读取返回的也是副本，修改结果不会回写。
- 集合在首次写入时自动创建；空集合（且没有索引）在压缩后可能不再保留。
- 经 CLI / HTTP 写入的数字会被 JSON 解码为 `float64`；Go 嵌入直接传入 `int` 等类型时会保留原类型，查询比较时统一按数值处理。
- 字段名支持点号路径（如 `profile.city`）用于查询、排序、投影和索引，但文档本身仍可自由嵌套。
- 字段名以 `$` 开头的会被当作操作符，不要用 `$` 作为普通字段名。

字段值可以是任意 JSON 类型，不同用途下的支持情况如下：

| JSON 类型 | 存储 | 过滤/排序 | 可建索引 |
|---|---|---|---|
| 字符串 | 支持 | 等值、正则、字典序比较 | 支持 |
| 数字 | 支持 | 按数值比较 | 支持 |
| 布尔 | 支持 | 等值 | 支持 |
| `null` | 支持 | 仅 `$eq: null` / `$exists` 有意义 | 不写入索引 |
| 数组 | 支持 | multikey 语义（任意元素命中） | 数组本身不索引，元素逐个索引 |
| 对象 | 支持 | 可按对象整体等值匹配 | 不写入索引 |

嵌套层级没有硬性限制，但层数越深，点号路径解析与深拷贝的成本越高；建议控制在 3～4 层以内。

通过 Go API 写入时，`[]string`、`map[string]string` 等具体类型的容器会在写入边界自动归一化为 `[]any` / `map[string]any`，并做深拷贝：写入后修改调用方传入的切片或 map 不会影响库内数据，这些字段也能正常参与点号路径查询、multikey 索引和 `$in` 过滤。`[]byte` 保留原类型，按 JSON 的 base64 语义存储。

一个完整的文档示例：

```json
{
  "_id": "u1",
  "name": "Alice",
  "age": 30,
  "active": true,
  "tags": ["go", "db"],
  "profile": {
    "city": "Shanghai",
    "joined": "2024-03-01"
  }
}
```

查询、排序、投影都可以用 `profile.city` 这种点号路径访问嵌套字段：

```bash
./bin/nanodb -db ./data find users '{"profile.city":{"$eq":"Shanghai"}}' -fields name,profile.city
```

## 查询

### 过滤条件

```json
{"age": {"$gte": 18}, "tags": {"$in": ["go", "db"]}}
```

CLI 写法：

```bash
./bin/nanodb -db ./data find users '{"age":{"$gte":18}}'
```

字段操作符：

| 操作符 | 语义 | 字段缺失时 |
|---|---|---|
| 字段等值 | `{"name":"Alice"}`，等价于 `$eq` | 仅 `null` 能匹配缺失 |
| `$eq` | 等于 | 仅 `null` 能匹配 |
| `$ne` | 不等于 | 匹配 |
| `$gt` / `$gte` / `$lt` / `$lte` | 数值或字符串比较 | 不匹配 |
| `$in` | 属于给定数组中的任意值 | 不匹配 |
| `$nin` | 不属于给定数组 | 匹配 |
| `$exists` | `true` 表示字段存在，`false` 表示字段缺失 | 按实际存在性判断 |
| `$regex` | Go 正则匹配字符串 | 不匹配 |
| `$not` | 对字段条件取反（如 `{"$not":{"$gt":10}}`） | 取反后判断 |

补充语义：

- **数组字段**：等值 / `$eq` / `$in` / `$regex` 采用 multikey 语义，任意一个元素匹配即算命中；`$ne` / `$nin` 只有在所有元素都不匹配时才成立。
- **数值**：`int`、`int64`、`float64` 等统一按数值比较，`1` 与 `1.0` 视为相等。
- **正则**：使用 Go `regexp` 语法；编译失败的条件不匹配任何文档（不会报错），也不会对非字符串字段生效。
- **比较**：不同类别的值（例如字符串与数字）无法比较，比较类操作符返回不匹配。

逻辑操作符：

```json
{
  "$and": [
    {"age": {"$gte": 18}},
    {"age": {"$lt": 65}}
  ],
  "$or": [
    {"city": "Shanghai"},
    {"tags": {"$in": ["go"]}}
  ]
}
```

- `$and`：全部条件成立。
- `$or`：任意条件成立；空数组视为不匹配。
- `$not`：对整个子过滤条件取反，例如 `{"$not":{"age":{"$lt":18}}}`。
- JSON 中数组会自然解码为 `[]any`；Go 嵌入时也需要传 `[]any{...}`，传 `[]map[string]any` 不会被识别。

### 排序

- 语法：`sort=age:-1,name:1`，`-1` 降序、`1` 升序；CLI 中为 `-sort age:-1,name:1`。
- 多字段按顺序比较，前一个字段相等时再比较下一个。
- 缺失字段始终排在最后，不受升降序影响。
- 数值按数值比较；字符串按字典序；类型不同且无法比较时按规范化后的字符串比较。
- 排序稳定：所有排序字段都相等时保持原始顺序。
- 同时指定 `sort` 和 `limit` 时，内部使用大小为 `skip+limit` 的 Top-N 堆，只保留前 N 条再排序。

### 分页与投影

- `limit` / `skip` 必须是非负整数；`limit=0` 表示不限制。
- `skip` 在排序之后应用。
- `fields` 为逗号分隔的字段列表，例如 `name,age` 或 `profile.city`。
- `_id` 默认保留：即使没有写进 `fields`，投影结果也会带上 `_id`。
- 多个投影字段之间是并集；暂不支持排除式投影（如只写 `-password`）。
- 有投影时直接按字段拷贝，不再先复制整份文档。

Go 中使用 `engine.Query`（`import "nanodb/engine"`）：

```go
docs, err := db.Find("users", engine.Query{
    Filter: map[string]any{"age": map[string]any{"$gte": 18}},
    Sort:   []engine.SortField{{Field: "age", Desc: true}},
    Limit:  10,
    Skip:   0,
    Fields: []string{"name", "age"},
})
```

### 查询执行流程

一次 `Find` 内部按下面的顺序执行，了解它有助于判断该给哪个字段建索引：

1. **编译过滤条件**：把 `map` 形式的 `filter` 展开成切片，并预编译其中的 `$regex`，只做一次，避免逐文档迭代 map 或重复编译正则。
2. **选择候选集**：如果过滤条件里有可用的索引（顶层字段或 `$and` 子条件），挑候选文档最少的索引；否则使用集合全部文档。
3. **回表过滤**：对每个候选文档按完整 `filter` 再匹配一次。索引只负责缩小范围，不保证语义完全等价，所以这一步不能省。
4. **排序**：有 `sort` 时抽取排序键；同时有 `limit` 时只维护前 `skip+limit` 条（Top-N 堆），否则全量排序。
5. **分页**：先 `skip` 再 `limit`。
6. **投影**：有 `fields` 时在拷贝阶段直接裁剪字段，没有投影时整份深拷贝。

`Count` 只执行第 1～3 步：不走排序、分页和投影，也不会生成文档副本，所以通常比 `Find` 再取 `len` 快得多。

## 更新

更新操作符：

| 操作符 | 说明 | 示例 |
|---|---|---|
| `$set` | 设置字段（支持点号路径） | `{"$set":{"profile.city":"Beijing"}}` |
| `$unset` | 删除字段 | `{"$unset":{"temp":""}}` |
| `$inc` | 数值增减 | `{"$inc":{"age":1}}` |
| `$push` | 向数组追加元素 | `{"$push":{"tags":"new"}}` |
| `$addToSet` | 向数组追加不重复元素 | `{"$addToSet":{"tags":"go"}}` |
| `$pull` | 从数组移除匹配元素，支持条件 | `{"$pull":{"tags":{"$gte":3}}}` |

```bash
./bin/nanodb -db ./data update users u2 '{"$inc":{"age":1},"$set":{"active":true}}'
```

行为说明：

- `Update` 更新单个文档；文档不存在返回 `ErrNotFound`。
- `UpdateMany` 按过滤条件批量更新，返回受影响文档数；集合不存在时返回 `0`。
- `Replace` 整体替换文档，`_id` 始终保留（请求体里的其他 `_id` 会被忽略）。
- 更新先校验再写入，非法表达式返回 `ErrInvalidUpdate`，不会产生半更新。
- `$inc` 的目标必须是数字，`$push` / `$addToSet` / `$pull` 的目标必须是数组，否则报错。
- `$pull` 的值可以是字面量，也可以是条件对象（如 `{"$gte":3}`）。

## 二级索引

```bash
./bin/nanodb -db ./data index create users age          # 普通索引
./bin/nanodb -db ./data index create users email unique # 唯一索引
./bin/nanodb -db ./data index list users
./bin/nanodb -db ./data index drop users age
```

索引规则：

- 单字段有序索引；字段支持点号路径（如 `profile.city`）。
- 支持可索引的标量：字符串、布尔、数字。`null`、缺失字段、对象等不会被写入索引。
- 数组字段按 multikey 处理：每个非空元素建一条索引项，同一文档内重复元素只保留一次。
- 唯一索引按“每个元素”校验：两个文档的数组若含有相同元素，同样会冲突。
- 创建唯一索引会校验存量数据，冲突返回 `ErrDuplicateKey`，索引不会创建。
- 以相同参数重复创建同一索引是幂等的（返回成功）；同字段但 `unique` 不同返回 `ErrInvalid`。
- 删除不存在的索引视为成功。
- 索引定义随快照和 WAL 持久化，重启后自动重建。

查询计划：

- 从过滤条件中选择候选集最小的索引，只扫描候选文档；没有可用索引时回退全表扫描。
- 可用的索引条件：等值（含 `$eq`）、`$in`、`$gt` / `$gte` / `$lt` / `$lte`。
- 会检查顶层字段以及 `$and` 的子条件；`$or` / `$not` 下不会使用索引候选集（直接全表扫描），以保证结果正确。
- 范围查询在有序索引项上二分定位边界，复杂度为 `O(log n + 命中数)`。
- 等值 / `$in` 查询使用索引键到文档 ID 的映射。
- 索引本身不存储文档副本，最终结果仍会回表并按查询语义重新过滤。

`index list` 的输出是索引定义数组，顺序按字段名排序：

```text
$ ./bin/nanodb -db ./data index list users
[
  {
    "field": "age",
    "unique": false
  },
  {
    "field": "email",
    "unique": true
  }
]
```

索引选择示例：假设 `users` 上同时有 `age` 和 `email` 索引，执行

```json
{"age": {"$gte": 18}, "email": "a@example.com"}
```

两个条件都能生成候选集，planner 会分别计算候选数量，选择更小的那个（这里是 `email` 的等值查询），再用完整过滤条件回表校验。等值条件的候选集通常远小于范围条件，因此高选择性字段更适合建索引。

维护成本：

- 每次插入、更新、删除都会同步维护该集合上的所有索引，写放大与索引数量成正比。
- 创建索引会扫描一次集合全部文档，数据量大时耗时为 `O(n)`。
- 唯一索引在写入时需要额外校验冲突。
- 索引条目常驻内存，内存占用约为“文档数 × 索引数 × 单条索引项”。
- 低选择性字段（例如只有 `true/false` 的布尔字段）建索引收益有限，通常不如全表扫描。

限制：暂不支持复合索引、文本索引、地理索引、聚合管道、索引覆盖查询和 `$not` 索引优化。

## 原子批量写入

CLI：

```bash
./bin/nanodb -db ./data batch '[
  {"op":"insert","coll":"users","doc":{"name":"Alice"}},
  {"op":"insert","coll":"users","doc":{"name":"Bob"}},
  {"op":"update","coll":"users","id":"<id>","patch":{"$inc":{"age":1}}},
  {"op":"delete","coll":"users","id":"<id>"}
]'
```

从标准输入读取（适合大 JSON）：

```bash
cat ops.json | ./bin/nanodb -db ./data batch -
```

操作字段：

| 字段 | 适用操作 | 说明 |
|---|---|---|
| `op` | 全部 | `insert` / `update` / `delete` |
| `coll` | 全部 | 集合名，不能为空 |
| `doc` | `insert` | 文档对象；未提供 `_id` 时自动生成 |
| `id` | `update` / `delete` | 目标文档 ID，不能为空 |
| `patch` | `update` | 更新操作符对象 |

语义：

- 整批写入在集合克隆上暂存，任一操作失败则整批不生效，正式数据完全不变。
- 成功时只写一条 `batch` WAL 记录，崩溃恢复时整批重放，不会出现半提交。
- 可以跨集合操作。
- `update` 的目标文档不存在会整批失败；`delete` 的目标不存在不算错误，结果中 `deleted` 为 `false`。
- 重复 `_id`、唯一索引冲突、非法更新表达式都会导致整批失败。
- 返回 `[]BatchResult`，每项包含 `op`、`coll`、`id`、`doc`（插入/更新后的文档）或 `deleted`。

## 显式事务

### Go API

```go
tx, err := db.Begin()
if err != nil {
    return err
}
defer tx.Rollback() // 幂等；提交后调用返回 ErrTxDone

if _, err := tx.Update("accounts", from, map[string]any{"$inc": map[string]any{"balance": -100}}); err != nil {
    return err
}
if _, err := tx.Update("accounts", to, map[string]any{"$inc": map[string]any{"balance": 100}}); err != nil {
    return err
}
if err := tx.Commit(); err != nil {
    if errors.Is(err, engine.ErrConflict) {
        // 事务期间相关集合被其他写入修改，需重新 Begin 后重试
    }
    return err
}
```

事务内方法与 `DB` 基本一一对应：`Get` / `Find` / `Count` / `Insert` / `InsertMany` / `Replace` / `Update` / `UpdateMany` / `Delete` / `DeleteMany` / `Batch`。

### 隔离与冲突

- 隔离级别固定为**集合粒度的乐观可串行化**：首次访问集合时深拷贝快照并记录版本号，提交时校验整个读集。
- 提交时所有访问过的集合版本一致才成功；任一不一致返回 `ErrConflict`，事务结束，全部改动丢弃。
- 事务内可以重复读同一集合，也能看到自己未提交的写入。
- 只读事务同样需要 `Commit()` 来认证跨集合一致性。
- 冲突粒度是集合：对同一集合的并发写事务可能产生“假冲突”。
- 不加锁等待，不会死锁；不支持 Savepoint、事务内 DDL 和跨进程事务。
- 事务结束后调用任何事务方法都返回 `ErrTxDone`。

完整语义、隔离级别对照和重试建议见 [docs/transaction-isolation.md](docs/transaction-isolation.md)。

### HTTP 事务

```bash
TX=$(curl -s -X POST -H 'X-NanoDB-Token: secret' \
  localhost:8080/v1/transactions | jq -r .tx_id)

curl -X POST -H 'X-NanoDB-Token: secret' \
  -H 'Content-Type: application/json' \
  -d '{"name":"Alice"}' \
  localhost:8080/v1/transactions/$TX/documents/users

curl -X POST -H 'X-NanoDB-Token: secret' \
  localhost:8080/v1/transactions/$TX/commit
```

- 事务会话保存在服务端内存中，30 分钟未使用会自动回滚；提交或回滚后 `tx_id` 立即失效。
- HTTP 服务停机时会停止所有空闲定时器并回滚未提交事务。
- 事务内查询支持与普通查询相同的 `filter` / `sort` / `limit` / `skip` / `fields` / `count` 参数。

## 命令行

```text
nanodb [-db 目录] [-no-sync] <命令> [参数...]
```

全局参数：

| 参数 | 默认值 | 说明 |
|---|---|---|
| `-db` | `./nanodb-data` | 数据目录；不存在时自动创建 |
| `-no-sync` | `false` | 关闭每次写入的 `fsync`，吞吐更高但掉电可能丢失最近写入 |

命令：

```text
shell                          进入交互式 shell（默认），支持 begin/commit/rollback
put <集合> <JSON文档>           插入文档，未提供 _id 时自动生成
get <集合> <id>                 读取单个文档
find <集合> [过滤JSON]          查询文档（-sort/-limit/-skip/-fields）
count <集合> [过滤JSON]         统计文档数量
update <集合> <id> <更新JSON>   更新文档
delete <集合> <id>              删除单个文档
drop <集合>                     删除整个集合
index create <集合> <字段> [unique]  创建二级索引；unique 表示唯一索引
index drop <集合> <字段>        删除索引
index list <集合>               查看索引
batch <操作JSON数组|->          原子批量写入；- 表示从标准输入读取
collections                     列出所有集合
stats                           查看数据库统计
compact                         生成快照并清空 WAL
serve [-addr :8080] [-token X]  启动 HTTP 服务
help                            显示帮助
```

`find` 的选项：

```text
-sort 字段:-1,...   排序，-1 降序、1 升序
-limit N            返回条数上限
-skip N             跳过条数
-fields 字段1,字段2 投影
```

示例：

```bash
./bin/nanodb -db ./data find users '{"age":{"$gte":18}}' \
  -sort age:-1,name:1 -skip 20 -limit 10 -fields name,age

./bin/nanodb -db ./data index create users email unique
./bin/nanodb -db ./data stats
```

各命令的输出形状：

| 命令 | 输出 |
|---|---|
| `put` | 插入后的文档（含 `_id`） |
| `get` | 文档对象；不存在时报错 |
| `find` | 文档数组；无命中时为 `[]` |
| `count` | `{"count":n}` |
| `update` | 更新后的完整文档 |
| `delete` / `drop` | `{"deleted":true}` / `{"dropped":"集合名"}` |
| `index create` | `{"created":true,"field":"...","unique":false}` |
| `index drop` | `{"dropped":"字段名"}` |
| `index list` | 索引定义数组 |
| `batch` | 每步结果的数组 |
| `collections` | `[{"name":"...","count":n}]` |
| `stats` | 统计对象（见上） |
| `compact` | `{"compacted":true}` |

`serve` 的输出是日志（写到 stderr），不会打印 JSON：

```text
$ ./bin/nanodb -db ./data serve -addr :8080 -token secret
2026/10/23 18:30:00 NanoDB HTTP 服务已启动: http://localhost:8080
```

收到 `Ctrl+C`（SIGINT）或 SIGTERM 后会优雅停机：等待在途请求最多 5 秒，回滚所有未提交的 HTTP 事务，然后关闭数据库。

退出码：

- `0`：成功
- `1`：参数、I/O、数据库或查询错误；错误信息以 `错误:` 前缀输出到 stderr

### 交互式 shell

```text
nano> help
可用命令:
...
nano> begin
事务已开始
nano(tx)> put users '{"name":"Bob"}'
nano(tx)> rollback
事务已回滚
nano> exit
```

- `exit` / `quit` / `\q`：退出；有未提交事务时自动回滚。
- `begin` / `commit` / `rollback`：管理事务；无事务时提交/回滚会提示错误。
- `help` / `?`：查看命令帮助。
- `serve` 不能在 shell 内启动，会提示退出后运行。
- 事务中只允许数据命令，DDL（`drop`、`index`）和 `compact` 等会被拒绝。

## HTTP API

启动：

```bash
./bin/nanodb -db ./data serve -addr :8080 -token secret
```

通用约定：

- 除 `/healthz` 外，设置 token 后所有请求都需要鉴权；支持 `X-NanoDB-Token: <token>` 或 `Authorization: Bearer <token>`，使用常量时间比较。
- 写入接口的 `Content-Type` 应为 `application/json`。
- 请求体上限 8 MiB，超限返回 `413`；JSON 后出现多余内容会返回 `400`。
- 错误响应体统一为 `{"error":"..."}`。
- 方法不支持时返回 `405`，并带 `Allow` 头。
- 服务端超时：请求头 10s、读 30s、写 30s、空闲连接 60s。

### 路由

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/healthz` | 健康检查（无需鉴权） |
| GET | `/` | 服务信息：`{"name","version","status"}` |
| GET | `/v1/collections` | 列出集合，返回 `[{"name","count"}]` |
| DELETE | `/v1/collections/{coll}` | 删除集合，返回 `{"dropped":coll}` |
| GET | `/v1/{coll}` | 查询；参数 `filter`、`sort`、`limit`、`skip`、`fields`、`count` |
| POST | `/v1/{coll}` | 插入一个文档或文档数组 |
| DELETE | `/v1/{coll}?filter=...` | 按条件删除文档，返回 `{"deleted":n}`；省略 `filter` 表示删除集合内**全部**文档 |
| GET | `/v1/{coll}/{id}` | 读取文档 |
| PUT | `/v1/{coll}/{id}` | 整体替换文档，`_id` 保留 |
| PATCH | `/v1/{coll}/{id}` | 按更新操作符修改文档 |
| DELETE | `/v1/{coll}/{id}` | 删除文档，返回 `{"deleted":true}` |
| GET | `/v1/indexes/{coll}` | 列出索引 |
| POST | `/v1/indexes/{coll}` | 创建索引，body：`{"field":"age","unique":false}` |
| DELETE | `/v1/indexes/{coll}/{field}` | 删除索引，返回 `{"dropped":field}` |
| POST | `/v1/batch` | 原子批量写入 |
| GET | `/v1/admin/stats` | 统计信息 |
| POST | `/v1/admin/compact` | 压缩存储，返回 `{"compacted":true}` |

> 注意：`DELETE /v1/{coll}` 不传 `filter`（或 `filter=` 为空）会删除该集合内所有文档。只想删除部分文档时务必带上过滤条件；清空集合更推荐 `DELETE /v1/collections/{coll}`，这样集合本身也会被删除。

### 查询参数

| 参数 | 说明 |
|---|---|
| `filter` | JSON 对象，URL 编码 |
| `sort` | `age:-1,name:1` |
| `limit` / `skip` | 非负整数；`limit` 省略或为 0 表示不限制 |
| `fields` | 逗号分隔的投影字段 |
| `count` | `1` / `true` / `yes` 时只返回数量：`{"count":n}` |

注意：`count=true` 只统计满足 `filter` 的文档数，忽略 `sort`、`limit`、`skip` 和 `fields`。

### 请求与响应示例

```bash
# 插入单个文档 → 201，返回带 _id 的完整文档
curl -X POST localhost:8080/v1/users \
  -H 'Content-Type: application/json' \
  -d '{"name":"Alice","age":30}'

# 批量插入数组 → 201，{"inserted":n,"docs":[...]}
curl -X POST localhost:8080/v1/users \
  -H 'Content-Type: application/json' \
  -d '[{"name":"A"},{"name":"B"}]'

# 查询 → 200，返回 JSON 数组
curl -G localhost:8080/v1/users \
  --data-urlencode 'filter={"age":{"$gte":18}}' \
  -d 'sort=age:-1' -d 'limit=10'

# 计数
curl -G localhost:8080/v1/users -d 'count=true'

# 读取 / 替换 / 修改 / 删除
curl localhost:8080/v1/users/u1
curl -X PUT localhost:8080/v1/users/u1 -H 'Content-Type: application/json' -d '{"age":31}'
curl -X PATCH localhost:8080/v1/users/u1 -H 'Content-Type: application/json' -d '{"$inc":{"age":1}}'
curl -X DELETE localhost:8080/v1/users/u1

# 按条件删除
curl -G -X DELETE localhost:8080/v1/users --data-urlencode 'filter={"age":{"$lt":18}}'

# 创建唯一索引
curl -X POST localhost:8080/v1/indexes/users \
  -H 'Content-Type: application/json' \
  -d '{"field":"email","unique":true}'

# 原子批量
curl -X POST localhost:8080/v1/batch \
  -H 'Content-Type: application/json' \
  -d '[{"op":"insert","coll":"users","doc":{"name":"A"}},{"op":"insert","coll":"users","doc":{"name":"B"}}]'
```

批量插入的原子性：

- POST 数组会逐个插入，**不是**原子操作：遇到错误时，错误之前已插入的文档会保留。
- 如果需要“全成或全不成”，请使用 `/v1/batch` 或显式事务。

### 响应体示例

下表列出各路由的典型响应（省略鉴权请求头）：

```text
GET /healthz
200 {"status":"ok"}

GET /
200 {"name":"NanoDB","version":"0.1.0","status":"ok"}

GET /v1/collections
200 [{"name":"users","count":2}]

GET /v1/admin/stats
200 {
  "dir": "./data",
  "collections": 1,
  "documents": 2,
  "indexes": 1,
  "wal_bytes": 342,
  "sync": true
}

POST /v1/users            （单个文档）
201 {"_id":"u1","age":30,"name":"Alice"}

POST /v1/users            （数组）
201 {"inserted":2,"docs":[{"_id":"u1",...},{"_id":"u2",...}]}

GET /v1/users
200 [{"_id":"u1",...},{"_id":"u2",...}]

GET /v1/users?count=true
200 {"count":2}

GET /v1/users/u1
200 {"_id":"u1","age":30,"name":"Alice"}

PUT /v1/users/u1
200 {"_id":"u1","age":31}

PATCH /v1/users/u1
200 {"_id":"u1","age":31,"name":"Alice"}

DELETE /v1/users/u1
200 {"deleted":true}

DELETE /v1/users?filter={"age":{"$lt":18}}
200 {"deleted":0}

GET /v1/indexes/users
200 [{"field":"age","unique":false}]

POST /v1/indexes/users
201 {"field":"age","unique":false}

DELETE /v1/indexes/users/age
200 {"dropped":"age"}

POST /v1/batch
200 {"count":2,"results":[{"op":"insert","coll":"users","id":"u1","doc":{...}},...]}

POST /v1/admin/compact
200 {"compacted":true}

DELETE /v1/collections/users
200 {"dropped":"users"}
```

错误响应示例：

```text
GET /v1/users/missing
404 {"error":"document not found"}

POST /v1/users（重复 _id）
409 {"error":"nanodb: duplicate _id: \"u1\""}

GET /v1/users（limit 非法）
400 {"error":"invalid limit: strconv.Atoi: parsing \"x\": invalid syntax"}

PUT /v1/users/u1（方法不匹配的错误路由）
405 {"error":"method not allowed"}，响应头 Allow: GET, PUT, PATCH, DELETE
```

### 事务路由

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/v1/transactions` | 开启事务，返回 `201 {"tx_id":...}` |
| POST | `/v1/transactions/{id}/ops` | 事务内批量写，返回 `{"count","results"}` |
| GET | `/v1/transactions/{id}/documents/{coll}` | 事务内查询 |
| POST | `/v1/transactions/{id}/documents/{coll}` | 事务内插入（支持单文档或数组） |
| DELETE | `/v1/transactions/{id}/documents/{coll}?filter=...` | 事务内按条件删除 |
| GET | `/v1/transactions/{id}/documents/{coll}/{docid}` | 事务内读取 |
| PUT | `/v1/transactions/{id}/documents/{coll}/{docid}` | 事务内替换 |
| PATCH | `/v1/transactions/{id}/documents/{coll}/{docid}` | 事务内更新 |
| DELETE | `/v1/transactions/{id}/documents/{coll}/{docid}` | 事务内删除 |
| POST | `/v1/transactions/{id}/commit` | 提交，返回 `{"committed":true,"tx_id":id}` |
| POST | `/v1/transactions/{id}/rollback` | 回滚，返回 `{"rolled_back":true,"tx_id":id}` |
| DELETE | `/v1/transactions/{id}` | 回滚，同上 |

事务或文档不存在返回 `404`；事务冲突返回 `409`。

完整流程：

```bash
BASE=localhost:8080
AUTH='X-NanoDB-Token: secret'

# 1. 开启事务 → 201 {"tx_id":"<32 位十六进制>"}
TX=$(curl -s -X POST -H "$AUTH" $BASE/v1/transactions | jq -r .tx_id)

# 2. 事务内写入与查询（提交前对其他请求不可见）
curl -X POST -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"_id":"a1","balance":100}' \
  $BASE/v1/transactions/$TX/documents/accounts

curl -X PATCH -H "$AUTH" -H 'Content-Type: application/json' \
  -d '{"$inc":{"balance":-10}}' \
  $BASE/v1/transactions/$TX/documents/accounts/a1

curl -G -H "$AUTH" --data-urlencode 'filter={"balance":{"$gt":0}}' \
  $BASE/v1/transactions/$TX/documents/accounts

# 3. 提交 → 200 {"committed":true,"tx_id":"..."}
curl -X POST -H "$AUTH" $BASE/v1/transactions/$TX/commit
```

若事务期间其他请求修改过同一集合，提交返回 `409`：

```json
{"error":"nanodb: transaction conflict: collection \"accounts\" changed during transaction"}
```

此时需要重新 `POST /v1/transactions`，从最新数据重新读取后重试。`tx_id` 的生命周期：

- 创建后 30 分钟未使用会自动回滚；每次访问都会重置计时。
- `commit` / `rollback` / `DELETE /v1/transactions/{id}` 后立即失效，再次使用返回 `404`。
- 服务停机时所有未提交事务都会回滚。

### 错误码

| 状态码 | 场景 |
|---|---|
| 400 | 参数或 JSON 不合法、更新表达式非法、空批量、数组元素不是对象 |
| 401 | 鉴权失败 |
| 404 | 集合/文档/事务不存在、未知路由 |
| 405 | 方法不支持（带 `Allow` 头） |
| 409 | `_id` 重复、唯一索引冲突、事务冲突 |
| 413 | 请求体超过 8 MiB |
| 500 | 未归类的内部错误 |
| 503 | 数据库已关闭 |

## Go 嵌入

```go
package main

import (
    "fmt"

    "nanodb/engine"
)

func main() {
    db, err := engine.Open("./data", &engine.Options{
        Sync:             true,
        CompactThreshold: 4 << 20,
    })
    if err != nil {
        panic(err)
    }
    defer db.Close()

    doc, err := db.Insert("users", map[string]any{"name": "Alice", "age": 30})
    if err != nil {
        panic(err)
    }
    fmt.Println(doc["_id"])

    docs, err := db.Find("users", engine.Query{
        Filter: map[string]any{"age": map[string]any{"$gte": 18}},
    })
    if err != nil {
        panic(err)
    }
    fmt.Println(len(docs))
}
```

### 打开选项

| 选项 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `Sync` | `bool` | `true`（`opts == nil` 时） | 每次写入是否 `fsync` |
| `CompactThreshold` | `int64` | 4 MiB | WAL 字节数上限，超过后自动压缩；`<= 0` 使用默认值 |

注意：传入 `&engine.Options{}` 表示显式选择 `Sync: false`。需要默认行为时传 `nil`，或显式写 `Sync: true`。

### DB API

| 方法 | 返回 | 说明 |
|---|---|---|
| `Insert(coll, doc)` | `(map[string]any, error)` | 插入，自动补 `_id` |
| `InsertMany(coll, docs)` | `([]map[string]any, error)` | 逐个插入；失败时返回已成功部分 |
| `Get(coll, id)` | `(map[string]any, bool)` | 读取副本；不存在返回 `false` |
| `Find(coll, q)` | `([]map[string]any, error)` | 查询并返回副本 |
| `Count(coll, filter)` | `(int, error)` | 统计数量 |
| `Replace(coll, id, doc)` | `(map[string]any, error)` | 整体替换，保留 `_id` |
| `Update(coll, id, patch)` | `(map[string]any, error)` | 单文档更新 |
| `UpdateMany(coll, filter, patch)` | `(int, error)` | 批量更新 |
| `Delete(coll, id)` | `(bool, error)` | 删除并返回是否原本存在 |
| `DeleteMany(coll, filter)` | `(int, error)` | 按条件删除 |
| `Batch(ops)` | `([]BatchResult, error)` | 原子批量 |
| `CreateIndex(coll, field, unique)` | `error` | 创建索引 |
| `DropIndex(coll, field)` | `error` | 删除索引；不存在视为成功 |
| `ListIndexes(coll)` | `[]IndexSpec` | 按字段名排序的索引列表 |
| `DropCollection(coll)` | `error` | 删除集合；不存在视为成功 |
| `Collections()` | `[]CollectionInfo` | 按名称排序的集合及文档数 |
| `Stats()` | `Stats` | 目录、集合/文档/索引数、WAL 字节数、是否同步 |
| `Compact()` | `error` | 写快照并清空 WAL |
| `Close()` | `error` | 关闭；重复调用返回 `nil` |

### 常用写法

插入并区分错误类型：

```go
_, err := db.Insert("users", map[string]any{"_id": "u1", "email": "a@example.com"})
switch {
case errors.Is(err, engine.ErrDuplicateID):
    // _id 已存在
case errors.Is(err, engine.ErrDuplicateKey):
    // 唯一索引冲突
case err != nil:
    return err
}
```

批量更新与条件删除：

```go
updated, err := db.UpdateMany("users",
    map[string]any{"active": false},
    map[string]any{"$set": map[string]any{"archived": true}},
)
if err != nil {
    return err
}
fmt.Println("updated:", updated)

deleted, err := db.DeleteMany("users", map[string]any{"archived": true})
```

原子批量写入：

```go
results, err := db.Batch([]engine.BatchOp{
    {Op: "insert", Coll: "users", Doc: map[string]any{"name": "Alice"}},
    {Op: "insert", Coll: "users", Doc: map[string]any{"name": "Bob"}},
})
```

索引管理：

```go
if err := db.CreateIndex("users", "email", true); err != nil {
    return err
}
for _, ix := range db.ListIndexes("users") {
    fmt.Println(ix.Field, ix.Unique)
}
```

### 生命周期

- `Close` 之后所有数据方法返回 `ErrClosed`（`Get` 返回 `false`），重复 `Close` 返回 `nil`。
- 进程退出前应 `Close`，否则最后一次写入可能尚未 `fsync`（`Sync: true` 时每次写入已同步，但关闭仍会释放文件句柄）。
- `Compact` 可以在运行中调用；它会短暂阻塞写入。

### 错误值

| 错误 | 含义 |
|---|---|
| `ErrNotFound` | 文档不存在 |
| `ErrDuplicateID` | 插入的 `_id` 已存在 |
| `ErrDuplicateKey` | 唯一索引冲突 |
| `ErrClosed` | 数据库已关闭 |
| `ErrInvalid` | 参数非法（空集合名、空索引字段、空批量等） |
| `ErrInvalidUpdate` | 更新表达式非法 |
| `ErrConflict` | 事务提交冲突 |
| `ErrTxDone` | 事务已结束 |

错误可通过 `errors.Is` 判断；部分错误包含操作序号或字段名等上下文。

### 并发

- 同一个 `*DB` 可被多个 goroutine 并发读写，内部使用读写锁。
- 单个 `Tx` 内部有互斥保护，但一个事务仍应只在一个逻辑流程中使用。
- 批量写入和事务提交会在内存中克隆集合，集合很大时需要考虑瞬时内存开销。

## 持久化与恢复

```text
data/
├── snapshot.json   # 全量数据与索引定义
└── wal.log         # 快照之后的增量操作（JSON Lines）
```

快照结构（概念示例）：

```json
{
  "version": 1,
  "gen": 3,
  "collections": {
    "users": [{"_id": "u1", "name": "Alice"}]
  },
  "indexes": {
    "users": [{"field": "age", "unique": false}]
  }
}
```

`gen` 是快照对应的 WAL 代数，每次压缩递增一次：重放 `wal.log` 时只应用 `gen` 不小于快照代数的记录。这样即使压缩过程中“快照已重命名、WAL 尚未清空”时崩溃，旧记录也会被整体跳过，不会重复叠加到快照之上（否则唯一索引可能报冲突）。

WAL 每行一条记录，字段包括 `op`、`gen`、`coll`、`id`、`doc`、`ops`、`field`、`unique`：

| `op` | 含义 |
|---|---|
| `put` | 插入或覆盖一条文档（更新也写成 `put`） |
| `delete` | 删除一条文档 |
| `drop` | 删除整个集合 |
| `batch` | 原子批量，`ops` 内为上述子操作 |
| `tx` | 事务提交，`ops` 内为事务内写操作 |
| `index.create` | 创建索引 |
| `index.drop` | 删除索引 |

`wal.log` 是 JSON Lines：每行一条完整记录，顺序即写入顺序。示例：

```json
{"op":"put","coll":"users","doc":{"_id":"u1","name":"Alice","age":30}}
{"op":"delete","coll":"users","id":"u2"}
{"op":"index.create","coll":"users","field":"age","unique":true}
{"op":"batch","ops":[{"op":"put","coll":"users","doc":{"_id":"u3"}},{"op":"delete","coll":"users","id":"u4"}]}
{"op":"drop","coll":"users"}
```

- `unique: false` 会被省略（`omitempty`），缺省即普通索引。
- `gen` 为写该记录时的 WAL 代数；`0` 会被省略，因此未经过压缩的库记录中通常看不到该字段。
- 事务提交写为一条 `{"op":"tx","ops":[...]}` 记录，批量写为 `{"op":"batch","ops":[...]}`。
- 更新在 WAL 中表现为一条覆盖式的 `put`，记录的是更新后的完整文档。
- 每条记录写完即结束；崩溃留下半行时，重放会在该半行处停止并截断。
- 写入过程中如果 WAL 追加失败，文件会回退到上一条完整记录，避免半条记录与后续写入拼成非法 JSON。

打开流程：

1. 加载 `snapshot.json`（不存在则视为空库）。
2. 按顺序重放 `wal.log`，跳过 `gen` 小于快照代数的旧记录。
3. 如果重放后 WAL 字节数超过 `CompactThreshold`，立即压缩。

写入与压缩：

- `Sync: true` 时每次写入后 `fsync` WAL；`Sync: false` 时由操作系统决定刷盘时机。
- WAL 超过阈值时自动压缩：先写临时快照、`fsync`、原子重命名，再清空 WAL。
- 也可以手动执行 `compact` 或 `POST /v1/admin/compact`。
- 快照不保存空集合；空集合在压缩后消失。
- 崩溃导致 WAL 尾部出现不完整记录时，重放会在该记录处停止并截断到最后一个完整记录，之前已提交的数据不受影响。
- 只支持单进程打开：不要用两个进程同时指向同一目录。

### 备份与迁移

备份：

1. 停止写入进程（CLI 退出，或向 `serve` 发送 `Ctrl+C` 等待优雅停机）。
2. 复制整个数据目录（至少包含 `snapshot.json` 与 `wal.log`）。
3. 恢复时把目录放回原位再启动即可。

也可以先执行一次 `compact`，让 `snapshot.json` 包含全量数据、`wal.log` 清空，这样备份只需保证快照文件完整。

迁移到其他机器：

- 两个文件都不包含绝对路径，直接复制目录即可。
- 不建议手工编辑 `snapshot.json`；如果必须修改，请保持 `version` 为 `1`，并确保每个文档都有合法的字符串 `_id`。
- 不要只删除 `wal.log` 来“清理”：它包含快照之后的增量，删除会丢失这些写入。正确做法是执行 `compact`。
- 导出数据可以用 `find` 拿到 JSON 数组；导入时优先用 `batch -` 从标准输入灌入，避免一次请求过大。

## 性能参考

10 万条文档，等值/范围查询命中 1000 条时的参考值（Windows 本机实测，随机器与负载波动较大）：

| 场景 | 耗时 | 分配次数/op |
|---|---:|---:|
| 等值查询 · 索引 | 3.0 ms | 2,037 |
| 等值查询 · 全表扫描 | 12.0 ms | 2,012 |
| 范围查询 · 索引 | 3.1 ms | 2,037 |
| 范围查询 · 全表扫描 | 27.3 ms | 2,012 |
| 投影查询 · 索引 | 3.1 ms | 2,037 |
| 排序 + 分页（1 万命中，skip 500 / limit 50） | 12.9 ms | 208 |
| 随机序排序 Top-50（10 万命中） | 6.5 ms | 6 |
| 随机序全量排序（10 万命中） | 85.3 ms | 6 |

优化点：

- 索引范围查询用二分定位边界，复杂度为 `O(log n + 命中数)`，不再扫描全部索引项。
- 有投影时边拷贝边投影，分配次数与非投影查询一致。
- 排序先一次性抽取排序键，避免每次比较都回表取字段。
- `sort` + `limit` 走 Top-N 堆；随机序数据下 Top-N 约为全量排序的 1/13。
- 过滤条件在查询开始前编译一次，避免每条文档分配迭代器。
- 连续跑基准会触发 CPU 降频，绝对耗时波动可能超过 30%，横向对比建议以 `allocs/op` 为准。

调优建议：

- 高频过滤/排序字段建索引，但不要为低选择性字段建太多索引。
- 只需部分字段时使用 `fields` 投影。
- 只需要前 N 条时同时给出 `sort` 和 `limit`，触发 Top-N。
- 能接受掉电丢最近写入时，用 `-no-sync` / `Sync:false` 换取吞吐。
- 写入量大的场景可适当增大 `CompactThreshold`，减少压缩次数（代价是重启重放时间变长）。

## 测试

```bash
go vet ./...
go test ./...
go test -cover ./...
go test -race ./...   # 需要 CGO 和 C 编译器，Windows 需安装 gcc
go test ./engine -run '^$' -bench Benchmark -benchmem
```

当前覆盖率约为：engine 80%、server 80%、cmd 75%。测试覆盖：

- 引擎：CRUD、查询操作符、排序/分页/投影、索引与唯一约束、批量原子性、事务隔离与冲突、WAL 重放、压缩、关闭后行为、更新错误路径。
- server：鉴权、路由、405、参数校验、错误码、事务会话与超时回收、请求体上限。
- CLI：参数解析、一次性命令全链路、shell 事务与 EOF 回滚、serve 启停。

基准统一使用 10 万条文档；`BenchmarkFind*` 覆盖等值、范围、投影、分页与排序，`BenchmarkSortRandom*` 是随机序数据上的全量排序与 Top-N A/B。

## 项目结构

```text
NanoDB/
├── go.mod
├── README.md
├── docs/
│   └── transaction-isolation.md   # 事务隔离级别与冲突说明
├── cmd/nanodb/
│   ├── main.go                    # CLI 参数、命令分发、shell、serve
│   └── main_test.go
├── server/
│   ├── server.go                  # REST 路由、鉴权、事务会话
│   ├── server_test.go
│   └── error_test.go
└── engine/
    ├── db.go                      # 存储引擎、CRUD、WAL、快照、压缩
    ├── query.go                   # 过滤匹配、排序、投影
    ├── update.go                  # 更新操作符
    ├── index.go                   # 二级索引与查询计划
    ├── batch.go                   # 原子批量写入
    ├── tx.go                      # 显式事务
    ├── id.go                      # _id 生成
    ├── copy.go                    # 文档深拷贝
    ├── db_test.go
    ├── query_test.go
    ├── index_test.go
    ├── batch_test.go
    ├── tx_test.go
    └── bench_test.go
```

## 已知限制

- 不支持 SQL、聚合管道、复合索引、文本索引、地理索引。
- 不支持多进程同时打开同一目录，也不支持副本、分片或集群。
- 不支持 Savepoint、事务内 DDL（`drop`、`index create/drop`、`compact`）和跨进程事务。
- 事务隔离级别固定，不可配置；冲突粒度是集合。
- HTTP 只有共享 token 鉴权，没有用户体系和细粒度权限。
- 文档与索引常驻内存，数据规模受内存限制；WAL 是 JSON Lines，存储开销高于二进制格式。
- HTTP 单次请求体上限 8 MiB；大导入建议拆分或使用 CLI `batch -`。
- 数组插入（POST 数组）不是原子的；需要原子性时使用批量接口或事务。

## 常见问题

**Q：为什么 WAL 文件越来越大？**

WAL 超过 `CompactThreshold`（默认 4 MiB）会自动压缩。如果没有触发，可以手动执行：

```bash
./bin/nanodb -db ./data compact
```

**Q：写入后进程被强制结束，数据会丢吗？**

默认 `Sync: true`，已成功返回的写入在掉电前会 `fsync`。如果用了 `-no-sync`，最近写入可能丢失。无论哪种设置，WAL 尾部不完整记录都会在下次打开时被截断，不会影响更早的完整记录。

**Q：为什么事务提交返回 `ErrConflict`？**

事务期间访问过的集合被其他写操作修改了。重新 `Begin()`、重新读取最新数据后重试即可。冲突粒度是集合，同一集合上的并发写事务可能互相冲突。

**Q：唯一索引对数组字段怎么算冲突？**

数组按 multikey 展开，每个元素都参与唯一性校验；两个文档只要有一个相同元素就冲突。

**Q：删除不存在的索引/集合会报错吗？**

不会。`DropIndex` 和 `DropCollection` 对不存在的目标是幂等的。

**Q：能从两个进程同时打开同一个数据目录吗？**

不能。NanoDB 没有跨进程锁；请确保同一目录同一时间只被一个进程使用。

**Q：可以在 HTTP 服务运行时用 CLI 打开同一目录吗？**

不可以，原因同上。CLI 与 serve 应使用不同目录，或先停止服务。

**Q：查询明明有索引，为什么还是全表扫描？**

常见原因：

- 条件写在 `$or` 或 `$not` 下面，planner 不会用索引候选集。
- 条件嵌在 `$or` 内部的子过滤里，即使字段有索引也不会被选中。
- 字段类型不可索引（`null`、对象），或字段实际缺失。
- 索引建在 `age` 上，但查询用的是 `profile.age`，两者不是同一个字段。
- 索引尚未创建成功或字段名不一致，可用 `index list` 确认。

**Q：怎么快速导入大量数据？**

优先用 CLI 的 `batch -` 从标准输入读取，单条 WAL 记录、原子提交，也绕开了 HTTP 的 8 MiB 限制：

```bash
./bin/nanodb -db ./data batch - < ops.json
```

如果不需要原子性，也可以拆成多个批次，避免单批内存占用过高。

**Q：如何备份和恢复？**

停止进程后复制整个数据目录即可，详见[备份与迁移](#备份与迁移)。不要在服务运行时直接复制文件，可能拿到不一致的快照与 WAL 组合。

**Q：`Update` / `Delete` 遇到不存在的集合会报错吗？**

- `Update` / `Replace` 对不存在的文档返回 `ErrNotFound`。
- `UpdateMany` / `DeleteMany` 对不存在的集合返回 `0`，不报错。
- `Delete` 对不存在的文档返回 `(false, nil)`。
- `Get` 对不存在的文档返回 `(nil, false)`。

**Q：HTTP 事务会话为什么突然 404 了？**

`tx_id` 只在服务端内存中保存。30 分钟没有请求会被自动回滚，服务重启也会全部丢失。长事务应拆短，或改用单次 `/v1/batch`。

**Q：投影结果里为什么总有 `_id`？**

`_id` 是文档身份，投影默认始终保留。想彻底去掉只能在拿到结果后自行删除（HTTP/CLI 都会带上它）。
