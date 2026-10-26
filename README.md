# gormx

借鉴 MyBatis-Plus 的使用体验，为 GORM 增加不可变 Wrapper、泛型 DAO、一次调用分页和可选审计填充插件。保留 GORM 的连接池、事务、模型 Hook 与原生软删除能力，不重新实现 ORM。

当前仓库提供可运行的基础实现，包含 P0、P1、P2 基础能力及第一阶段平铺连表查询；生产推广仍建议按阶段验收。[完整设计与排期](docs/design.md)。

## 已完成内容

以下功能已实现并有自动化测试，不代表尚未实现的扩展或所有生产场景也已验证。

| 阶段 | 已完成能力 | 已验证内容 |
| --- | --- | --- |
| P0：条件构造 | 不可变字符串 Wrapper、Lambda 字段、嵌套 AND/OR、排序、条件开关 | 字段映射、参数绑定、空集合、零值、条件复用、嵌入字段、不同命名策略下的缓存隔离 |
| P0：分页查询 | `Page[T]`、自动 count + limit、主键排序兜底、参数检查 | 查询次数与状态隔离、空结果、越界页、500 条上限、offset 溢出、查询失败不返回半成功结果 |
| P1：数据操作 | 泛型 CRUD、批量新增、条件更新和删除、主键方法 | ID 回填、零值更新、字段权限、非法主键、空条件写保护、批次失败回滚 |
| P1：事务与并发 | Context 传递、DAO 事务、原生 GORM 跨模型事务 | 提交、回滚、嵌套 Savepoint、并发查询和更新、MySQL Repeatable Read 一致性快照 |
| P2：填充与删除 | 可选审计填充插件、原生时间字段和软删除、模型 Hook | 每条写语句求值、map 零值保留、Select/Omit/SkipHooks、Hook 顺序、原生软删除恢复及物理清理 |
| 连表阶段 1 | 独立 `JoinQuery`、INNER/LEFT JOIN、自连接、显式投影、泛型 DTO、结果行分页 | 逐表字段与别名校验、连接表软删除放入 ON、可空结果、事务及并发隔离、跨表条件、错误传递 |
| 示例与验证 | 三种 Controller、编译期负例测试、模糊测试、基准测试 | 响应一致性、真实 HTTP 进程启动、异常响应、类型错误编译失败、Go 版本兼容性 |

[本次验证结果](#本次验证结果) · [测试复现方法](#运行验证) · [后续优化功能](#后续优化功能)

## 环境

- Go 1.22+；本次主验证环境为 Go 1.26.1、MySQL 8.4.5、macOS 26.5.2 / Apple M1 Pro。
- Go 1.22.12 也已验证；在本机较新 macOS 上，独立程序需要外部链接，详见下方兼容性说明。
- GORM v1.31.1；支持按 GORM 方言构建 SQL，目前实际验证 SQLite 与 MySQL。
- SQLite 测试使用 `go-sqlite3`，需要 CGO 和 C 编译器；macOS 可使用 Xcode Command Line Tools。

## 安装

```sh
go get github.com/heicoder-wuyunbin/gormx@latest
```

```go
import "github.com/heicoder-wuyunbin/gormx"
```

## 快速使用

```go
type User struct {
    ID        uint           `gorm:"primaryKey"`
    Name      string         `gorm:"size:100"`
    Age       int
    Status    int
    CreatedAt time.Time
    UpdatedAt time.Time
    DeletedAt gorm.DeletedAt  `gorm:"index"`
}

dao, err := gormx.NewDAO[User](db)
if err != nil {
    return err
}

query := gormx.NewWrapper[User]().
    LikeIf(name != "", "Name", "%"+name+"%").
    OrderByDesc("Age")
if status != nil {
    query = query.Eq("Status", *status)
}

page, err := dao.SelectPage(ctx, query, gormx.PageRequest{
    Current: 1,
    Size:    20,
})
```

不要在 `EqIf`/`WhereIf` 实参中解引用可能为 nil 的指针，Go 会先求值实参。可选指针字段使用上面的显式 `if`，也能正确保留 `status=0`。

> `NewDAO` 只应接收原始 DB 或事务句柄，会清除已附加的查询条件。不要传入带租户条件的 `db.Where(...)` 并期望条件继续生效。租户条件必须显式加入 Wrapper，复杂 GORM Scope 查询使用原生 GORM。

## 两种 Wrapper 风格

字符串字段版接受 Go 字段名或 GORM 列名，按模型白名单校验，拒绝未知字段、忽略字段和 SQL 片段。所有列通过 `clause.Column` 引用，普通值参数化绑定。

```go
query := gormx.NewWrapper[User]().
    Eq("Status", 1).
    Or(func(q *gormx.Wrapper[User]) *gormx.Wrapper[User] {
        return q.Like("Name", "张%").Ge("Age", 30)
    }).
    OrderByAsc("ID")
// Status = 1 AND (Name LIKE '张%' OR Age >= 30)
```

Lambda 风格通过纯字段选择器建立模型、字段值类型关联：

```go
var (
    UserName = gormx.Field(func(u *User) *string { return &u.Name })
    UserAge  = gormx.Field(func(u *User) *int { return &u.Age })
)

query := gormx.NewLambda[User]().
    WhereIf(name != "", gormx.Like(UserName, "%"+name+"%")).
    Where(UserAge.Between(18, 40)).
    OrderBy(UserAge.Desc())

// UserAge.Eq("18") 无法编译：期望 int。
// 将 User 的条件传给 DAO[Order] 也无法编译。
```

`NewLambda` 和 `NewWrapper` 使用同一个引擎，也允许两种条件混用。选择器字段重命名由编译器检查；是否对应可持久化字段由运行时 GORM Schema 校验，结果按 Schema 缓存，支持 `column`、`embeddedPrefix` 和命名策略。比较运算符是否适用于某个数据库类型仍由调用者负责，这不是 SQL 类型系统。

Wrapper **不可变**，动态构造务必接住返回值：

```go
base := gormx.NewWrapper[User]().Eq("Status", 1)
adults := base.Ge("Age", 18) // base 不受影响
```

Wrapper 和 DAO 可以并发复用；不要并发修改传入的指针、map 或切片元素。`In` 会复制切片容器，但不会深拷贝元素。

## API 概览

| 类别 | 方法 |
| --- | --- |
| 查询 | `GetByID`、`First`、`List`、`Count`、`SelectPage` |
| 新增 | `Create`、`CreateBatch` |
| 更新 | `Update`、`UpdateByID` |
| 删除 | `Delete`、`DeleteByID` |
| 事务 | `Transaction` |
| 字符串条件 | `Eq/EqIf`、`Ne`、`Gt/Ge/Lt/Le`、`Like/LikeIf/NotLike` |
| 集合与范围 | `In/NotIn`、`Between`、`IsNull/IsNotNull` |
| 分组与排序 | `And/Or`、`OrderByAsc/OrderByDesc` |
| 类型条件 | `Where/WhereIf`、`All/Any`、`Column` 比较方法、`Like`、`OrderBy` |
| 多表查询 | `TableOf/Bind`、`NewJoinQuery`、`InnerJoin/LeftJoin`、`On`、`Select`、`JoinList/JoinPage` |

- `First/GetByID` 无记录返回 `gorm.ErrRecordNotFound`；`List/Page.Records` 无记录返回非 nil 空切片。
- `ByID` 支持单个字符串或整数主键，数字字符串会严格解析；复合或特殊类型主键请使用 Wrapper。
- `List` 不限制条数，HTTP 列表接口应使用 `SelectPage`。
- 批量新增回填自增 ID；默认写事务开启时，跨批次失败会整体回滚。失败后内存实体可能仍保留已分配的 ID，不代表事务已提交。
- `Update` 使用 map，正确保留 `0`、`false`、`""`，同时拒绝主键和不可更新字段。更新值的数据库类型由 GORM 校验。
- 更新返回数据库报告的 `RowsAffected`，MySQL 是否报告匹配行或实际改变行受连接参数影响。
- 不提供 `Save`，避免其更新后可能回退插入的隐式语义。

```go
rows, err := dao.Update(ctx,
    gormx.NewWrapper[User]().In("ID", []uint{1, 2, 3}),
    map[string]any{"Status": 0, "Age": 0},
)
```

所有更新、删除必须携带有效限制条件。空 Wrapper、只有排序、全部关闭的动态条件、单独 `NotIn(empty)` 会返回 `ErrUnsafeWrite`。`In(empty)` 恒不匹配，允许安全地影响零行；`NotIn(empty)` 恒为真。

该保护只检查条件结构，不证明任意条件不会覆盖全表。字段、条件和更新 map 的业务授权仍由应用负责；不要允许客户端自由指定所有模型字段，也不要把用户输入包装成 `gorm.Expr`。显式传入的 GORM 表达式具有执行 SQL 的能力，不属于普通参数。

## 分页契约

```json
{
  "records": [],
  "total": 0,
  "current": 1,
  "size": 20,
  "pages": 0
}
```

- 从第 1 页开始；参数为 0 时使用第 1 页、每页 20 条。
- 负数、每页超过 500、offset 整数溢出会返回 `ErrInvalidPage`。
- 正常流程始终执行 `COUNT` 和 `SELECT ... LIMIT ... OFFSET ...` 两次查询，包括空结果与越界页。
- count 不带排序、limit、offset；数据查询追加主键排序作为同值排序的兜底。无主键模型应自行提供唯一排序。
- 默认是两条独立语句，写入并发下总数和记录可能短暂不一致。需要同一快照时使用合适隔离级别的事务。
- 深分页仍有 OFFSET 成本；游标分页属于后续增强，不在当前 API 内。

## 连表查询

使用独立的 `JoinQuery`，不改变已有 `DAO[T]` / `Wrapper[T]` API。第一阶段以一对一、多对一的平铺 DTO 查询为主，不自动装配一对多嵌套对象。

例如 `Employee.DepartmentID -> Department.ID`，完整模型和可编译函数见 [连表示例](examples/joins/query.go)：

```go
type EmployeeDepartment struct {
    EmployeeID     uint
    EmployeeName   string
    DepartmentName *string // LEFT JOIN 未匹配时为 nil
}

e := gormx.TableOf[Employee]("e")
d := gormx.TableOf[Department]("d")
query, err := gormx.NewJoinQuery(db, e)
if err != nil {
    return err
}
query = query.
    LeftJoin(d, gormx.On(e.Col("DepartmentID"), d.Col("ID")).
        And(d.Col("TenantID").Eq(tenantID))).
    Where(e.Col("TenantID").Eq(tenantID)).
    Select(
        e.Col("ID").As("employee_id"),
        e.Col("Name").As("employee_name"),
        d.Col("Name").As("department_name"),
    ).
    OrderBy(e.Col("ID").Asc())

page, err := gormx.JoinPage[EmployeeDepartment](ctx, query, request)
```

需要类型约束时，复用现有 `Field` 并绑定到表实例：

```go
name := gormx.Bind(e, gormx.Field(func(v *Employee) *string { return &v.Name }))
query = query.WhereIf(keyword != "", name.Like("%"+keyword+"%"))
// name.Eq(123) 无法编译；Bind 也不能把 Employee 字段绑定到 Department 表。
```

- `NewJoinQuery` 只接收原始 DB 或事务句柄，同样不保留预挂条件和 Scope；租户权限必须逐表显式设置。
- 每次 `TableOf` 创建独立身份。复用同一实例引用字段；自连接使用不同实例和别名。
- `On` 必须用字段等值关联新表与已加入的表；附加条件通过 `On(...).And(...)` 添加。复杂分组使用 `JoinAll/JoinAny`，附加字段比较使用 `JoinEqual`。
- `Select` 替换投影；所有输出必须显式 `As`，且映射到 DTO 可读字段的数据库列名。不允许 `SELECT *`、重复别名或未加入查询的表字段。
- 原生 `gorm.DeletedAt` 的主表过滤放在 WHERE，连接表过滤放在 ON。未知模型级 QueryClause 返回错误，不静默忽略。任意第三方权限插件需要单独适配验证。
- `JoinPage[R]` 正常执行 count + select，**统计 JOIN 结果行，不是去重后的主实体数**。一对多会出现重复主实体，不能用它直接分页完整的子列表。
- `JoinList[R]` 返回无隐式上限的平铺结果，`query.Count(ctx)` 不要求投影。DTO 扫描不执行源模型或 DTO 的 `AfterFind`。
- LEFT JOIN 的可空字段使用指针或 `sql.Null*`。目前不封装一对多主实体分页、聚合、DISTINCT、JOIN 写入或查看已删除行。

[完整多表 API 与边界说明](docs/joins.md)。运行示例验证：`go test -v ./examples/joins`。

## 自动填充与软删除

时间字段优先使用 GORM 的 `CreatedAt/UpdatedAt` 或 `autoCreateTime/autoUpdateTime`。业务审计字段使用可选插件：

```go
type actorKey struct{}

actor := func(ctx context.Context) (any, error) {
    id, ok := ctx.Value(actorKey{}).(string)
    if !ok || id == "" {
        return nil, errors.New("authenticated actor required")
    }
    return id, nil
}

err := db.Use(gormx.NewFillPlugin(
    gormx.FillRule{Field: "CreatedBy", OnCreate: true, Value: actor},
    gormx.FillRule{
        Field: "UpdatedBy", OnCreate: true, OnUpdate: true,
        Overwrite: true, Value: actor,
    },
))
```

规则需对应模型字段，未映射字段会跳过。插件在模型 `BeforeCreate/BeforeUpdate` 前执行，支持模型结构体、结构体批量及 `map[string]any`；不支持将任意 DTO 当作模型更新。值按每次写语句求值，错误通过 `tx.AddError` 阻止写入。

`Overwrite=false` 保留非零结构体字段及已提供的 map 键，包含显式零值。插件尊重字段权限、`Select/Omit` 和 `SkipHooks`，因此不是不可绕过的权限机制。注册必须在服务开始并发处理请求之前完成。

声明 `gorm.DeletedAt` 即启用原生软删除，查询和 count 自动过滤；无此字段的模型会物理删除。`BeforeDelete/AfterDelete` 保持可用，用于删除授权等扩展。软删除不会触发 Update 回调，因此 `UpdatedBy` 规则不会自动变为“删除人”填充。

物理清理、恢复、查看已删除记录应通过显式原生 GORM `Unscoped()` 操作，并在应用层单独授权。软删除不自动解决唯一索引冲突，详情见设计文档。

## 事务与原生 GORM

```go
err := dao.Transaction(ctx, func(txDAO *gormx.DAO[User]) error {
    if err := txDAO.Create(ctx, &user); err != nil {
        return err
    }
    _, err := txDAO.UpdateByID(ctx, user.ID, map[string]any{"Status": 1})
    return err
})
```

跨模型事务或指定隔离级别：

```go
err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
    users, err := gormx.NewDAO[User](tx)
    if err != nil {
        return err
    }
    _, err = users.SelectPage(ctx, query, request)
    return err
}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
```

平铺 INNER/LEFT JOIN 可使用 `JoinQuery`；聚合、预加载、锁、子查询和复杂 SQL 继续使用原生 GORM。增强层不试图覆盖全部 GORM API。

## 运行验证

默认执行 SQLite、功能边界、编译期类型检查及示例测试；没有数据库环境变量时，MySQL 测试会明确跳过。**跳过不等于通过。**

```sh
go test -count=1 ./...
go test -race -shuffle=on -count=3 -cover ./...
go build ./...
go vet ./...
go mod verify
```

先创建专用测试库，再安全设置 `GORMX_TEST_DSN`，DSN 需启用 `parseTime=True`。执行全部 MySQL 契约及快照测试：

```sh
go test -run '^TestMySQL' -count=1 -v .
```

上述库测试只创建 `gx_<时间戳>_` 前缀的临时表，退出时清理，不创建或删除数据库。不要连接生产数据库。凭据只通过环境变量传入，不提交到仓库或写入共享脚本。

真实 HTTP 进程测试单独启用，需要本机 `8080` 端口空闲，连接账号具备 `CREATE/DROP DATABASE` 权限：

```sh
GORMX_HTTP_TEST_DSN="$GORMX_TEST_DSN" \
  go test -run '^TestControllerMySQLProcess$' -count=1 -v ./examples/controller
```

该测试自行创建并删除 `gx_http_<时间戳>` 独立数据库，不迁移传入 DSN 所指向的数据库。会构建并启动真实示例，验证首次 AutoMigrate、正常重启、接口结果及错误状态码，最后停止进程并清理数据库。仅对专用本地测试实例执行。

同时启用两种数据库测试后，重跑完整验证：

```sh
GORMX_HTTP_TEST_DSN="$GORMX_TEST_DSN" \
  go test -race -shuffle=on -count=3 -cover ./...
```

模糊测试和性能基线均不依赖 MySQL：

```sh
go test -run '^$' -fuzz '^FuzzPageRequest$' -fuzztime=15s -parallel=2 .
go test -run '^$' -fuzz '^FuzzWrapperBinding$' -fuzztime=15s -parallel=2 .
go test -run '^$' -fuzz '^FuzzJoinBinding$' -fuzztime=15s -parallel=2 .
go test -run '^$' -bench . -benchmem -benchtime=1s -count=3 .
```

测试源码入口：

| 文件 | 验证范围 |
| --- | --- |
| [gormx_test.go](gormx_test.go) | SQLite/MySQL 共用 CRUD、Wrapper、分页、填充与并发契约 |
| [edge_test.go](edge_test.go) | 复合主键、删除 Hook、错误传递、并发更新、嵌入字段 |
| [contract_extended_test.go](contract_extended_test.go) | 字段缓存、写权限、事务、恢复、真实 MySQL 快照 |
| [join_test.go](join_test.go) / [join_fuzz_test.go](join_fuzz_test.go) | 双数据库连表、自连接、DTO、行分页、软删除、跨表引用、别名及绑定 |
| [typesafety_test.go](typesafety_test.go) | 正常代码可编译；错误值类型、错误模型、错误 Like 字段类型无法编译 |
| [fuzz_test.go](fuzz_test.go) | 分页整数边界、字段白名单和普通值不改变 SQL 结构 |
| [benchmark_test.go](benchmark_test.go) | Wrapper 构建与万行 SQLite 分页对照基线 |
| [Controller 测试](examples/controller/main_test.go) / [HTTP 测试](examples/controller/http_test.go) | 三种处理器响应一致性、数据库异常、启动校验、真实进程 |

## 本次验证结果

验证日期：**2026-10-26**。以下是实际执行结果，不是预期结果。

| 检查项 | 结果 |
| --- | --- |
| Go 1.26.1 下 SQLite + MySQL 完整功能测试 | 通过 |
| 真实 MySQL HTTP 进程、首次迁移与不迁移重启 | 通过 |
| 随机顺序、连续 3 轮、`-race` 竞态检查 | 通过 |
| 增强库语句覆盖率 | **95.5%**，包含新增连表实现；此前单表版本为 96.2% |
| Controller 包语句覆盖率 | **71.9%**；参数解析、三种处理器和响应函数均为 100% |
| 连表功能及示例 | SQLite/MySQL 连表契约通过；`examples/joins` 语句覆盖率 100% |
| 类型安全编译测试 | 2 个正例通过，5 种错误代码均被编译器拒绝，含跨模型 Bind 和连表字段值类型 |
| 分页模糊测试 | 15 秒，289,928 次执行，无失败 |
| 字段与 SQL 绑定模糊测试 | 15 秒，16,712 次执行，无失败 |
| 多表别名与绑定模糊测试 | 15 秒，273,709 次执行，无失败 |
| `go build ./...`、`go vet ./...`、`go mod verify` | 全部通过 |
| Go 1.22.12 下库及处理器测试 | 通过 |
| Go 1.22.12 下独立进程 | 默认链接在本机失败；改用外部链接后完整测试通过 |

Controller 的 `main()` 通过独立进程测试执行，子进程未合并到 `go test -cover` 的覆盖率，因此包覆盖率不是 100%。库内部分防御性错误分支仍未覆盖；覆盖率和有限时长的模糊测试都不是无缺陷证明。

### Go 1.22 与新版 macOS

本机 macOS 26.5.2 启动 Go 1.22.12 默认构建的独立程序时，实际出现 `dyld: missing LC_UUID load command`。这影响本机旧工具链的可执行文件启动，不能标记为默认配置兼容。

使用已安装的 C 工具链进行外部链接后，包含 MySQL 和真实 HTTP 进程的测试全部通过：

```sh
GOTOOLCHAIN=go1.22.12 GOFLAGS='-ldflags=-linkmode=external' \
  GORMX_HTTP_TEST_DSN="$GORMX_TEST_DSN" go test -count=1 ./...
```

上述命令仍需预先设置 `GORMX_TEST_DSN`。日常开发推荐当前已验证的 Go 1.26.1；未验证的操作系统或 Go 版本不在本次结论范围内。

### 本地性能基线

Apple M1 Pro，Go 1.26.1；每组执行 3 次，以下取中位数。分页使用内存 SQLite 的 10,000 条记录、相同条件、排序和两次查询，每页 20 条。

| 场景 | 耗时 | 分配次数 |
| --- | --- | --- |
| 字符串 Wrapper 构建 | 299.8 ns/op | 13 allocs/op |
| Lambda Wrapper 构建 | 152.9 ns/op | 6 allocs/op |
| 原生 GORM count + 分页 | 730.5 us/op | 295 allocs/op |
| 字符串 Wrapper 分页 | 731.2 us/op | 306 allocs/op |
| Lambda Wrapper 分页 | 746.8 us/op | 303 allocs/op |

构建基准中 Lambda 一次 `Where` 添加多个条件，字符串版逐次链式添加，分配差异不能泛化为所有查询的性能结论。分页数值包含 SQLite 执行成本和测量波动，仅用于后续回归对比，不是 MySQL 压测结果或生产 SLA。

### 尚未验证

- PostgreSQL、SQL Server 等其他方言，以及 Linux/Windows 平台矩阵。
- 百万级数据、生产索引、深分页及持续高并发负载。
- 网络中断、数据库重启、死锁、超时重试与连接池耗尽等故障场景。
- 分片、全局租户隔离、JOIN 聚合和一对多主实体分页等尚未实现的增强功能；当前 JOIN 尚无生产性能基线。
- 生产迁移和灰度发布。测试通过不代替业务授权、索引评估及上线验收。

## 后续优化功能

下面均为**待实现或待完善**事项，与上面的完成清单分开管理。P0/P1/P2 基础代码已落地，不意味着这些增强项也已经完成。

| 优先级 | 可优化功能 | 目标与验收方向 |
| --- | --- | --- |
| 高 | 租户与数据权限约束 | 设计不能意外丢失的强制过滤机制；评估拒绝带条件 DB 句柄，避免依赖构造时被清除的 Scope |
| 高 | CI 与发布工程 | 自动执行 Go/数据库矩阵、竞态及短时 fuzz；确定真实 module 路径、许可证和版本策略 |
| 高 | 分页性能 | 新增游标分页、可选跳过 count；保留现有 `SelectPage` 两次查询契约，新增 API 明确总数语义 |
| 高 | 乐观锁 | 版本字段条件更新、冲突错误与受影响行数校验，测试并发覆盖写 |
| 中 | 更完整的类型安全 | 生成字段代码、类型安全更新、受约束的排序/比较 API，减少反射和字符串更新字段 |
| 中 | 关联查询阶段 2 | 主实体 ID 分页后批量加载子集合、EXISTS 筛选、聚合；不要改变已有结果行分页语义 |
| 中 | 原生 GORM 组合入口 | 平铺投影已实现；继续评估 Scope、预加载、锁及事务选项，防止状态污染或权限绕过 |
| 中 | 审计与软删除策略 | 增加删除人/删除时间审计、授权恢复入口和软删除唯一索引方案 |
| 中 | 分配与缓存优化 | 基于现有 benchmark/profile 优化不可变 Wrapper 拷贝和字段缓存，保持并发隔离 |
| 中 | 可观测性与故障验证 | 增加慢查询指标、追踪、连接异常和超时测试；重试必须先明确事务与幂等边界 |
| 中 | 示例服务工程化 | 配置化监听地址、请求超时和优雅退出；鉴权仍属于应用层 |

## Controller 示例

[可运行源码](examples/controller/main.go) 包含三个行为一致的 GET 接口：

- `/before/users`：原生 GORM。
- `/users`：字符串 Wrapper。
- `/lambda/users`：Lambda Wrapper。

先在本机安全设置 `GORMX_DSN` 指向开发数据库。仅首次初始化示例表时：

```sh
GORMX_AUTO_MIGRATE=1 go run ./examples/controller
```

之后可直接 `go run ./examples/controller`，示例不会自动插入数据。示例仅监听本机，没有鉴权，不应直接暴露公网。

```sh
curl 'http://127.0.0.1:8080/users?name=alice&status=0&minAge=18&current=1&size=20'
```

测试还会比较三种 Controller 的 JSON 响应，包含零值条件、无记录、越界页和非法参数。

## 许可证

[Apache License 2.0](LICENSE)
