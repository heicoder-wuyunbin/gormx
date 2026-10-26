# GORM 增强库设计与实施方案

## 1. 目标与设计哲学

目标不是在 Go 中照搬 Java 的继承和方法引用机制，而是保留 MyBatis-Plus 的三个体验优势：

1. 常规数据访问开箱即用，业务无需为每次筛选新增 DAO 方法。
2. 查询条件可以按请求参数组合，业务代码不拼接 SQL。
3. 分页、审计和删除策略具有一致约定，但仍能退回底层 ORM。

本实现采用组合而非继承：`DAO[T]` 管理执行入口，`Wrapper[T]` 描述条件，`Page[T]` 承载结果，Callback 插件扩展写行为。实现及测试已经落地，排期用于后续生产接入与逐阶段验收，不表示全部工作仍未开始。

## 2. 四大能力对照

| MyBatis-Plus 能力 | 原生 GORM 已有能力 | 实际体验差距 | gormx 方案 |
| --- | --- | --- | --- |
| `BaseMapper<T>` 泛型 CRUD | `Create/Find/Updates/Delete/Count/CreateInBatches`；v1.30 起已有 `gorm.G[T]` 泛型 API | 每个业务仍需约定统一方法、零值更新、错误处理与批次入口 | `DAO[T]` 提供一致的 CRUD、批量、计数及事务方法 |
| `QueryWrapper/LambdaQueryWrapper` | `Where`、`Scopes`、结构体/map 条件和 `clause` | 动态范围、嵌套、排序通常使用列名或 SQL 片段；结构体条件会忽略部分零值 | 不可变字符串 Wrapper + 泛型字段选择器，共用条件引擎 |
| `IPage/Page` | `Count`、`Limit`、`Offset` 可组合 | 各接口重复 count、偏移计算、结果封装与参数检查；需注意查询状态复用 | `SelectPage` 两次独立查询，统一 `Page[T]` |
| 自动填充、逻辑删除 | 原生时间填充、模型 Hook、Callback、`gorm.DeletedAt` | 业务操作人等上下文字段缺少统一规则；删除行为易被项目重复封装 | 可选 `FillPlugin`；软删除完全复用 GORM，保留模型删除 Hook |

因此，增强层的价值是规范和组合，而不是补上 GORM 完全不存在的 CRUD、批量或软删除能力。MyBatis-Plus 的分页通常还依赖分页插件；本方案直接使用 GORM 方言能力，无需 SQL 解析插件。

## 3. 分层设计

```text
HTTP Controller / Service
          |
          v
DAO[T] ------ PageRequest / Page[T]
  |
  +---- Wrapper[T] ---- Predicate[T] ---- Column[T,V]
  |         |
  |         v
  |    GORM Schema 校验 + clause 表达式
  |
  v
GORM Session / Transaction / Context
  |
  +---- 可选 FillPlugin
  +---- GORM 时间字段、模型 Hook、原生软删除
  |
  v
MySQL / SQLite / 其他 GORM 方言
```

### 层次 1：泛型 DAO

- `T` 必须是非指针结构体，实体入参使用 `*T`，列表为 `[]T`。
- 构造时读取实际数据库的 Schema，保留数据库命名策略、连接及事务。
- 所有执行方法显式接受 `context.Context`。
- 每次操作使用独立 Session，防止已执行查询的条件泄漏到下一次操作。
- `CreateBatch` 使用 GORM 批次写入；不逐条调用 `Create`。
- `Update` 用 map 保留零值，返回受影响行数，拒绝主键修改。
- `GetByID/UpdateByID/DeleteByID` 只接收单个字符串或整数主键；复合主键使用显式 Wrapper。
- `GetByID` 不把用户字符串传给 GORM 的“任意查询条件”快捷参数，数字主键字符串严格解析，避免 MySQL 隐式数值转换。
- 不引入隐式 upsert，不包装 `Save`。

**构造边界：** 必须传入原始 DB 或事务句柄。构造时清除预挂条件、排序与分页，避免不可预测的继承；租户和数据权限必须放入 Wrapper。该约定不能当作多租户权限框架。依赖 GORM Scope 的复杂业务应继续使用原生 GORM。

### 层次 2：双风格 Wrapper

字符串版以模型字段为白名单，兼容 Go 字段名和数据库列名，使用 `clause.Column` 输出引用后的列。普通值由驱动绑定，不拼接到 SQL 中。

Lambda 版使用：

```go
Age := gormx.Field(func(u *User) *int { return &u.Age })
query := gormx.NewLambda[User]().Where(Age.Ge(18))
```

类型安全分成两层：

| 边界 | 检查阶段 |
| --- | --- |
| 模型字段是否存在、字段重命名、`Age.Eq("18")` 参数不匹配 | Go 编译期 |
| 条件属于哪个模型，是否可交给对应 DAO | Go 编译期 |
| 地址是否来自当前模型、字段是否忽略、真实列名 | 运行时 Schema 校验 |
| 某个比较运算符是否适合该数据库字段、值是否满足数据库约束 | 调用者及数据库 |

选择器必须是无副作用的地址选择函数，不解析 Go 函数 AST，也不使用 `unsafe`。通过反射解析嵌入字段和列映射，并按 Schema 缓存结果。它不是 Java getter 方法引用的完全等价物。

Wrapper 每次链式调用复制条件和排序切片，可以从一个基础 Wrapper 派生多条查询。所有顶层条件是 AND；`Or(func(...))` 表示追加一个内部为 OR 的括号组，而不是切换后续连接符。类型版使用 `All/Any` 显式表达括号。

空组返回错误。`In(empty)` 恒假，`NotIn(empty)` 恒真，语义不依赖方言对空集合 SQL 的处理。写保护能拒绝无条件或显然恒真的组合，但不会进行通用逻辑证明。

### 层次 3：Page[T]

`Page[T]` 包含 `Records/Total/Current/Size/Pages`；默认 1 页、20 条，上限 500 条。

执行流程：

1. 校验页码、每页条数和 offset 溢出。
2. 编译 Wrapper，一次解析字段映射。
3. 新 Session 执行不带排序、limit、offset 的 count。
4. 新 Session 复用相同过滤条件，应用排序及分页，查询记录。
5. 返回完整结果；任一步出错都返回错误，不返回半成功 Page。

为排序同值追加主键作为 tie-breaker，降低翻页重复或遗漏。没有主键时由调用者提供唯一排序。数据库并发写入仍可能改变 OFFSET 位置。

两次查询不自动放入事务，避免默认增加事务成本。需要一致快照时，由服务使用如 MySQL InnoDB Repeatable Read 的事务包裹；具体效果取决于数据库、引擎及隔离级别。

### 层次 4：Callback 与软删除

原生时间字段无需插件。自定义审计采用 `db.Use(NewFillPlugin(...))` 注册：

- Create 阶段在 `gorm:before_create` 前填充。
- Update 阶段在 `gorm:setup_reflect_value` 后、`gorm:before_update` 前填充。
- 值提供者从 Context 获取已认证操作人，不从不可信 Header 直接取身份。
- 插件错误写入 `tx.AddError`，中止本次操作。
- 默认保留已提供的值；可用 `Overwrite` 强制设置。
- 尊重 GORM 字段权限、Select/Omit、SkipHooks；缺失字段跳过。

这是一套方便业务复用的机制，不是不可绕过的审计或权限强制层。严谨的审计仍需结合服务授权、数据库约束和审计存储。

逻辑删除使用 `gorm.DeletedAt` 的原生 Query/Delete Clause。删除权限、审计扩展沿用 `BeforeDelete/AfterDelete`，没有第二套软删除 SQL 改写回调。批量条件删除时 Hook 收到的模型不包含每条记录的完整数据，不应假设能够直接逐行获得删除前快照。

删除路径不走 Update Callback，故不会隐式触发 `UpdatedBy` 填充。若要记录“谁删除了哪条数据”，后续应提供专用策略或事务内审计记录，不把 `UpdatedBy` 当作已实现的删除审计。

## 4. Controller 改造对比

以下省略重复的参数校验和 HTTP 错误响应；完整、可编译且经过响应一致性测试的版本位于 `examples/controller/main.go`。

### 改造前

```go
q := db.WithContext(ctx).Model(&User{})
if name != "" {
    q = q.Where("name LIKE ?", "%"+name+"%")
}
if status != nil {
    q = q.Where("status = ?", *status)
}
if minAge != nil {
    q = q.Where("age >= ?", *minAge)
}
var total int64
if err := q.Session(&gorm.Session{}).Count(&total).Error; err != nil {
    return err
}
records := make([]User, 0)
if err := q.Session(&gorm.Session{}).Order("age DESC, id ASC").
    Offset((current-1)*size).Limit(size).Find(&records).Error; err != nil {
    return err
}
// 还需计算总页数，组装响应。
```

### 改造后：字符串版

```go
q := gormx.NewWrapper[User]().
    LikeIf(name != "", "Name", "%"+name+"%").
    OrderByDesc("Age")
if status != nil {
    q = q.Eq("Status", *status)
}
if minAge != nil {
    q = q.Ge("Age", *minAge)
}
page, err := dao.SelectPage(ctx, q, gormx.PageRequest{
    Current: current, Size: size,
})
```

### 改造后：Lambda 版

```go
q := gormx.NewLambda[User]().
    WhereIf(name != "", gormx.Like(UserName, "%"+name+"%")).
    OrderBy(UserAge.Desc())
if status != nil {
    q = q.Where(UserStatus.Eq(*status))
}
if minAge != nil {
    q = q.Where(UserAge.Ge(*minAge))
}
page, err := dao.SelectPage(ctx, q, request)
```

每个新列表接口只需组合条件，无需新增 `FindByNameAndStatusAndAge` 等 DAO 方法。Controller 示例直接调用 DAO 是为了便于比较，真实项目存在业务编排、事务或权限时，应由 Service 层负责这些职责。

## 5. 分阶段实施排期

按 1 名熟悉 Go/GORM 的工程师、已有 MySQL 环境估算，生产接入约 8~10 个工作日。当前仓库已完成基础代码和回归测试；以下排期保留业务接入、评审和灰度时间，可根据实际业务缩短。

| 阶段 | 时间 | 工作项 | 上线验收 |
| --- | --- | --- | --- |
| P0：Wrapper + 分页 | 第 1~4 天 | 第 1 天冻结 API 与模型约定；第 2 天字符串/类型条件与嵌套；第 3 天分页及边界；第 4 天接入一个只读列表并灰度 | 两种 Wrapper 结果一致；分页确实两次查询；零值/空集合/非法字段覆盖；无业务 SQL 与额外 DAO 方法 |
| P1：更新与批量 | 第 5~6 天 | 零值更新、批量新增和条件批量更新/删除、主键规范、事务回滚；接入一个写场景 | 空条件拦截；跨批次失败回滚；主键回填；回归记录不存在和受影响行数 |
| P2：填充与逻辑删除 | 第 7~8 天 | 审计插件配置、原生软删除接入、Hook 验证、Context 身份约定 | 审计缺失失败；批量/更新 map 行为一致；软删除影响 List/Count/Page；删除 Hook 保留 |
| 发布缓冲 | 第 9~10 天 | 业务性能基线、索引 EXPLAIN、接入评审、灰度监控、版本文档 | 并发无竞态；查询耗时符合业务 SLO；回退路径明确 |

各阶段独立开关：P0 只替换读路径；P1 再替换写路径；P2 插件为可选注册。出现接入问题，可逐接口退回原生 GORM，不要求整体迁移。模型新增软删除字段属于数据库语义变更，需单独迁移和回滚评估，不能仅通过移除插件撤销。

## 6. 已完成验证

本地验证环境：Go 1.26.1 darwin/arm64，MySQL 8.4.5 Homebrew。2026-10-26 的完整测试清单、Go 1.22 链接兼容性说明及性能基线见 [README 验证结果](../README.md#本次验证结果)。

| 验证 | 结果 |
| --- | --- |
| SQLite 条件、CRUD、分页、批量、事务、填充、软删除 | 通过 |
| 同一核心契约在真实 MySQL 运行 | 通过 |
| Count/Select 查询次数及状态隔离 | 通过 |
| SQL 普通值绑定、非法列、零值更新、空条件写保护 | 通过 |
| 字段 tag、嵌入字段、Schema 缓存、取消 Context | 通过 |
| Controller 三种实现响应一致性 | 通过 |
| 随机顺序连续 3 轮测试、竞态检查，启用 MySQL 与真实 HTTP 测试 | 通过，含新增连表实现的库语句覆盖率 95.5% |
| 独立 JoinQuery、自连接、平铺 DTO、结果行分页和软删除过滤 | SQLite/MySQL 通过；[多表设计](joins.md) |
| `go vet ./...` | 通过 |

MySQL 库测试使用专用 `gormx_test` 数据库中的时间戳前缀临时表，结束后清理自己创建的表。真实 HTTP 进程测试另行创建并删除 `gx_http_<时间戳>` 数据库，需要显式启用及建库权限。两者均不操作已有业务表，数据库凭据未写入仓库。

覆盖率并不等于完整性；尚未执行生产规模压测，也未验证 PostgreSQL、SQL Server、多数据库分片、关联实体分页和聚合等场景。

## 7. 上线前必须明确的边界

- **租户与数据权限：** 当前没有全局租户插件。必须把权限条件写入 Wrapper，不能依赖被清除的预挂 Scope。
- **软删除唯一索引：** 已删除记录仍占普通唯一索引。不要简单假设将可空 `deleted_at` 加入唯一索引就能约束活跃记录，MySQL 对 NULL 的唯一性语义可能允许重复。按业务采用生成列活跃标记、归档策略或独立唯一性设计。
- **逻辑删除迁移：** 新增 `DeletedAt` 必须验证历史数据、索引、恢复路径与物理清理授权。
- **读一致性：** Page 默认不是单快照；需要强一致时显式使用事务。
- **性能：** 精确 count 和深 OFFSET 对大表昂贵；按真实条件设计索引并验证 EXPLAIN。暂不引入不透明的 count 缓存。
- **扩展范围：** 已通过独立 JoinQuery 支持平铺 INNER/LEFT JOIN；多表分页统计结果行而非去重主实体。不自动封装关联实体分页、预加载、GROUP BY、锁、乐观锁、多租户和游标分页。复杂查询继续使用原生 GORM，详见 [多表边界](joins.md)。
- **类型安全：** 当前字段安全是编译期类型约束加运行时映射，不等同于所有 SQL 语义都能在编译期证明。
- **发布准备：** 正式发布前确定真实 module 路径、许可证、版本策略与 CI 数据库矩阵。当前无擅自创建的 Git 提交或远端推送。
