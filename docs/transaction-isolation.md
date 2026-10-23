# NanoDB 事务隔离级别说明

本文说明 NanoDB 显式事务（`Begin` / `Commit` / `Rollback`）的隔离级别、能防止的异常、并发冲突行为以及使用限制。

## 1. 结论

NanoDB 只提供一种固定的隔离级别：

> **集合粒度的乐观可串行化**（集合快照读 + 提交时校验整个读集，冲突即中止）

- 不支持按事务配置 `READ COMMITTED`、`REPEATABLE READ` 等级别。
- 不加锁等待，不会死锁；冲突时直接返回 `ErrConflict`，由调用方决定是否重试。
- 事务提交成功时，其效果等价于在提交时刻串行执行。

## 2. 实现模型

一次事务的生命周期如下：

1. **`Begin()`**
   - 只记录事务对象，不持有数据库锁，也不复制数据。
2. **首次访问某个集合**
   - 在数据库读锁下深拷贝该集合（文档、顺序、索引），记录当时的集合版本号。
   - 之后事务内对该集合的所有读和写都作用于这份快照。
3. **事务内读写**
   - 读：只读快照，因此同一集合内可重复读。
   - 写：修改快照，并记录到事务的 WAL 操作列表；其他会话看不到这些改动。
4. **`Commit()`**
   - 短暂获取数据库写锁。
   - 校验事务访问过的**所有**集合：当前版本是否仍等于首次访问时的版本。
   - 全部一致：写入一条 `{"op":"tx","ops":[...]}` WAL 记录，用快照替换正式集合，提交成功。
   - 任一不一致：返回 `ErrConflict`，事务结束，所有改动丢弃，正式数据不变。
5. **`Rollback()`**
   - 直接丢弃快照和 WAL 操作列表，无副作用。

集合版本号由所有写路径递增：插入、替换、更新、按条件更新、删除、按条件删除、批量写入、删除集合、创建/删除索引，以及其他事务的提交。因此普通（非事务）写入也会与事务产生冲突检测。

## 3. 隔离级别对照

| 标准隔离级别 | NanoDB 支持情况 |
|---|---|
| READ UNCOMMITTED（读未提交） | 不支持。未提交写入只存在于事务私有快照中，不可能被其他事务读到。 |
| READ COMMITTED（读已提交） | 不支持。事务内不会看到其他会话后续提交的新值。 |
| REPEATABLE READ（可重复读） | 支持其读行为：同一集合在事务内多次读取结果一致。 |
| SERIALIZABLE（可串行化） | 支持（乐观方式、集合粒度）。提交成功的事务可视为串行执行。 |
| SNAPSHOT ISOLATION（快照隔离） | 读行为类似，但 NanoDB 额外校验整个读集，比普通快照隔离更严格。 |

## 4. 能防止的异常

以下保证均以 **`Commit()` 返回 `nil`** 为前提。

| 异常 | 是否防止 | 说明 |
|---|---|---|
| 脏读（Dirty Read） | 是 | 未提交写入只存在于事务快照内。 |
| 不可重复读（Non-repeatable Read） | 是 | 首次访问集合即固定快照，后续读取不变化。 |
| 幻读（Phantom Read） | 是 | 查询基于同一集合快照，事务内新增/删除不会改变结果。 |
| 丢失更新（Lost Update） | 是 | 提交时校验读集版本，期间被改写过就会冲突。 |
| 写偏差（Write Skew） | 是 | 校验覆盖事务访问过的全部集合，而不只是写入集合。 |

## 5. 一致性认证：为什么只读事务也要 Commit

快照是在**首次访问每个集合时**分别建立的，而不是在 `Begin()` 时一次性建立全局快照。因此：

- 如果事务已经读取了集合 A，随后其他事务修改了 A，本事务再读取集合 B，就可能同时看到“A 的旧值”和“B 的新值”。
- 这种状态无法通过提交：A 在事务的读集中且版本已变化，`Commit()` 会检测到并返回 `ErrConflict`。因此认证之后不会出现不可串行化的结果。

因此：

- **只读事务也应调用 `Commit()`**。提交成功表示读集在事务期间未被修改，读到的跨集合结果是一致且可串行化的。
- 如果只执行查询然后 `Rollback()`（或直接丢弃事务），则只有“单集合内可重复读”的保证，跨集合快照未经过认证。

## 6. 冲突行为

### 6.1 冲突粒度是集合，而不是文档

版本号按集合维护。事务读取集合 `users` 后，即使另一个事务只修改了 `users` 中一条完全无关的文档，本事务提交也会返回 `ErrConflict`。

- 好处：实现简单，且不会漏掉写偏差、谓词相关冲突。
- 代价：热点集合上的事务容易相互冲突，可能出现“假冲突”。

### 6.2 失败即中止，不等待

- 没有锁等待、没有死锁检测、没有自动重试。
- `ErrConflict` 返回时事务已经结束，不能继续使用，也不会应用任何改动。
- 后续操作返回 `ErrTxDone`；重新执行需要调用 `Begin()` 开启新事务。

### 6.3 重试示例（Go）

```go
const maxAttempts = 5

for attempt := 0; attempt < maxAttempts; attempt++ {
    tx, err := db.Begin()
    if err != nil {
        return err
    }
    if _, err := tx.Update("accounts", from, map[string]any{"$inc": map[string]any{"balance": -100}}); err != nil {
        _ = tx.Rollback()
        return err
    }
    if _, err := tx.Update("accounts", to, map[string]any{"$inc": map[string]any{"balance": 100}}); err != nil {
        _ = tx.Rollback()
        return err
    }
    err = tx.Commit()
    if err == nil {
        return nil
    }
    if errors.Is(err, engine.ErrConflict) {
        continue // 重新读取并重试
    }
    return err
}
return errors.New("事务重试次数耗尽")
```

## 7. 不支持的能力

| 能力 | 说明 |
|---|---|
| 可配置隔离级别 | 只有固定的集合粒度乐观可串行化。 |
| 悲观锁 / `SELECT ... FOR UPDATE` / 锁等待 | 不支持。 |
| Savepoint / 嵌套事务 | 不支持，一个连接同一时间只有一个事务。 |
| 事务内 DDL | 事务内不能创建/删除索引或集合；CLI 会拒绝这些命令。 |
| 跨进程事务 | 事务状态保存在当前进程内。NanoDB 是单进程嵌入式引擎，多实例并发打开同一目录不在支持范围内。 |
| 部分提交 | 提交是整批原子的，没有“部分成功”。 |
| 自动冲突重试 | 由调用方处理 `ErrConflict`。 |

## 8. 性能与使用建议

- **保持事务短小**：事务期间不持有数据库锁，但每个首次访问的集合都会被完整深拷贝，长时间事务会占用更多内存。
- **缩小读集**：只访问真正需要的集合。访问的集合越多，提交冲突概率越高。
- **热点集合要预期冲突**：同一集合上的并发写事务会因集合级版本校验而串行化，必要时在应用层重试。
- **只读事务记得 `Commit`**：需要一致性快照时，用提交成功来确认读集未被并发修改。
- **失败后必须重新 `Begin`**：冲突、`ErrClosed`、WAL 写入失败都会结束当前事务。

## 9. 接口速查

### Go API

```go
tx, err := db.Begin()          // 开启事务
tx.Get / tx.Find / tx.Count    // 快照读
tx.Insert / tx.Replace         // 写
tx.Update / tx.UpdateMany
tx.Delete / tx.DeleteMany
tx.Batch([]engine.BatchOp{...}) // 事务内原子批量
tx.Commit()                    // 提交；冲突返回 ErrConflict
tx.Rollback()                  // 回滚
```

### CLI（交互式 shell）

```text
nano> begin
nano(tx)> put users '{"name":"Alice"}'
nano(tx)> commit
```

事务中提示符为 `nano(tx)>`；`exit` 或输入结束会回滚未提交事务。

### HTTP

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/v1/transactions` | 开启事务，返回 `tx_id` |
| POST | `/v1/transactions/{id}/ops` | 事务内批量写 |
| GET/POST | `/v1/transactions/{id}/documents/{coll}` | 事务内查询 / 插入 |
| GET/PUT/PATCH/DELETE | `/v1/transactions/{id}/documents/{coll}/{docid}` | 事务内单文档读写 |
| POST | `/v1/transactions/{id}/commit` | 提交，冲突返回 `409` |
| POST | `/v1/transactions/{id}/rollback` | 回滚 |
| DELETE | `/v1/transactions/{id}` | 回滚 |

HTTP 事务状态保存在服务端内存中，**30 分钟未使用会自动回滚**；提交或回滚后 `tx_id` 立即失效（再次使用返回 `404`）。
