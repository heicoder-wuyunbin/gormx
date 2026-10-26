# gormx

借鉴 MyBatis-Plus 的使用体验，为 GORM 增加不可变 Wrapper、泛型 DAO、一次调用分页、平铺连表查询和可选审计填充插件。保留 GORM 的连接池、事务、模型 Hook 与原生软删除能力，不重新实现 ORM。

- 条件构造：字符串字段版与 Lambda 字段版共用同一引擎，字段按模型白名单校验，值参数化绑定。
- 泛型 CRUD：`DAO[T]` 直接按模型类型操作，无需为每个表写 Repository。
- 一次调用分页：`SelectPage` 自动执行 count 与 limit，返回统一的 `Page[T]`。
- 可选增强：连表查询、审计填充插件、环境变量建连，不需要时完全不引入。

## 环境要求

- Go 1.22+
- GORM v1.31.1
- 已实际验证的数据库：SQLite（测试需 CGO 与 C 编译器）、MySQL 8.x

## 安装

```sh
go get github.com/heicoder-wuyunbin/gormx@latest
```

```go
import "github.com/heicoder-wuyunbin/gormx"
```

## 快速开始

```go
type User struct {
    ID        uint           `gorm:"primaryKey"`
    Name      string         `gorm:"size:100"`
    Age       int
    Status    int
    CreatedAt time.Time
    UpdatedAt time.Time
    DeletedAt gorm.DeletedAt `gorm:"index"`
}

dao, err := gormx.NewDAO[User](db)
if err != nil {
    return err
}

query := gormx.NewWrapper[User]().
    LikeIf(name != "", "Name", "%"+name+"%").
    OrderByDesc("Age")

page, err := dao.SelectPage(ctx, query, gormx.PageRequest{
    Current: 1,
    Size:    20,
})
```

不要在 `EqIf`/`WhereIf` 实参中解引用可能为 nil 的指针，Go 会先求值实参。可选指针字段改用显式 `if`，这样也能正确保留 `status=0`：

```go
if status != nil {
    query = query.Eq("Status", *status)
}
```

## 从环境变量建立连接

`env` 子包只做“环境变量优先、调用方默认值兜底”，不内置地址、账号或口令。默认值属于应用：

```go
cfg, err := env.Load("DB_", env.MySQLConfig{
    Host: "127.0.0.1", Port: 3306,
    User: "root", Password: "secret", Database: "app",
})
if err != nil {
    return err
}
db, err := env.Open(ctx, cfg, env.WithConnectRetry(60*time.Second, 2*time.Second))
```

| 环境变量 | 说明 |
| --- | --- |
| `DB_HOST`、`DB_PORT`、`DB_NAME`、`DB_USER`、`DB_PASSWORD` | 连接地址与账号；无库内默认值 |
| `DB_CHARSET` | 默认 `utf8mb4` |
| `DB_LOC` | 默认 `Local`，也接受 `UTC` 或 IANA 时区名 |
| `DB_MAX_OPEN_CONNS`、`DB_MAX_IDLE_CONNS` | 默认 `20`、`5` |
| `DB_CONN_MAX_LIFETIME`、`DB_CONNECT_TIMEOUT` | 默认 `5m`、`10s` |
| `DB_DSN` | 整体覆盖上面的分项，需自带 `parseTime=True` |

- 前缀由应用决定（`DB_`、`APP_` 都可以），同一进程内多个服务不会争抢变量名。
- `Load` 的回落顺序：环境变量 → 调用方 `defaults` → 端口/字符集/池/超时中立兜底 → 校验错误。`Host` 缺失返回 `ErrInvalidConfig`。
- `Open` 设置连接池后主动 Ping，失败会关闭连接；错误与日志不包含密码。重试默认关闭，容器启动竞态用 `WithConnectRetry` 显式开启。
- `FROM scratch` 镜像必须内嵌 `time/tzdata`，否则 `DB_LOC=Local` 会静默退化为 UTC；也可以直接用 `DB_LOC=UTC`。

## 条件构造

字符串字段版接受 Go 字段名或 GORM 列名，拒绝未知字段、忽略字段和 SQL 片段。所有列通过 `clause.Column` 引用，普通值参数化绑定。

```go
query := gormx.NewWrapper[User]().
    Eq("Status", 1).
    Or(func(q *gormx.Wrapper[User]) *gormx.Wrapper[User] {
        return q.Like("Name", "张%").Ge("Age", 30)
    }).
    OrderByAsc("ID")
// Status = 1 AND (Name LIKE '张%' OR Age >= 30)
```

Lambda 版通过纯字段选择器建立模型与值类型的关联：

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

两种风格使用同一个引擎，可以混用。选择器重命名由编译器检查；字段是否可持久化由 GORM Schema 校验，结果按 Schema 缓存，支持 `column`、`embeddedPrefix` 和命名策略。比较运算符是否适用于某个数据库类型仍由调用者负责，这不是 SQL 类型系统。

Wrapper **不可变**，动态构造务必接住返回值：

```go
base := gormx.NewWrapper[User]().Eq("Status", 1)
adults := base.Ge("Age", 18) // base 不受影响
```

Wrapper 和 DAO 可以并发复用；不要并发修改传入的指针、map 或切片元素。`In` 会复制切片容器，但不深拷贝元素。

## DAO 与 CRUD

```go
// 查询
user, err := dao.GetByID(ctx, 1)              // 无记录返回 gorm.ErrRecordNotFound
first, err := dao.First(ctx, query)
users, err := dao.List(ctx, query)            // 无隐式上限，无记录返回非 nil 空切片
total, err := dao.Count(ctx, query)

// 新增（回填自增 ID）
err = dao.Create(ctx, &user)
err = dao.CreateBatch(ctx, users, 100)        // 默认写事务下，跨批次失败整体回滚

// 更新：map 保留 0、false、""，拒绝主键与不可更新字段
rows, err := dao.UpdateByID(ctx, 1, map[string]any{"Status": 0, "Age": 0})

// 删除：有 gorm.DeletedAt 时软删除，否则物理删除
rows, err = dao.DeleteByID(ctx, 1)
```

- `ByID` 支持单个字符串或整数主键，数字字符串会严格解析；复合或特殊类型主键请用 Wrapper。
- 更新返回数据库报告的 `RowsAffected`；MySQL 报告匹配行还是实际改变行受连接参数影响。
- 不提供 `Save`，避免其“更新不到则插入”的隐式语义。
- `List` 不限制条数，HTTP 列表接口应使用 `SelectPage`。

所有更新、删除必须携带有效限制条件。空 Wrapper、只有排序、全部关闭的动态条件、单独 `NotIn(empty)` 都会返回 `ErrUnsafeWrite`；`In(empty)` 恒不匹配，可安全影响零行。

该保护只检查条件结构，不证明条件不会覆盖全表。字段、条件和更新 map 的业务授权仍由应用负责；不要允许客户端自由指定所有模型字段，也不要把用户输入包装成 `gorm.Expr`，显式传入的表达式具有执行 SQL 的能力。

## 分页

```go
page, err := dao.SelectPage(ctx, query, gormx.PageRequest{Current: 1, Size: 20})
```

```json
{ "records": [], "total": 0, "current": 1, "size": 20, "pages": 0 }
```

- 从第 1 页开始；参数为 0 时使用第 1 页、每页 20 条。
- 负数、每页超过 500、offset 整数溢出返回 `ErrInvalidPage`。
- 正常流程始终执行 `COUNT` 和 `SELECT ... LIMIT ... OFFSET ...` 两次查询，包括空结果与越界页。
- count 不带排序、limit、offset；数据查询追加主键排序，作为同值排序的兜底。无主键模型应自行提供唯一排序。
- 默认两条独立语句，写入并发下总数和记录可能短暂不一致；需要同一快照时使用合适隔离级别的事务。
- 深分页仍有 OFFSET 成本，游标分页不在当前 API 内。

## 连表查询

使用独立的 `JoinQuery`，不改变 `DAO[T]` / `Wrapper[T]`。第一阶段面向一对一、多对一的平铺 DTO，不自动装配一对多嵌套对象。

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

需要类型约束时，复用 `Field` 并绑定到表实例：

```go
name := gormx.Bind(e, gormx.Field(func(v *Employee) *string { return &v.Name }))
query = query.WhereIf(keyword != "", name.Like("%"+keyword+"%"))
// name.Eq(123) 无法编译；Bind 也不能把 Employee 字段绑到 Department 表。
```

- `NewJoinQuery` 只接收原始 DB 或事务句柄，不保留预挂条件与 Scope；租户权限必须逐表显式设置。
- `On` 必须用字段等值关联新表与已加入的表，附加条件通过 `On(...).And(...)` 添加；复杂分组用 `JoinAll/JoinAny`，附加字段比较用 `JoinEqual`。
- `Select` 替换投影，输出必须显式 `As` 并映射到 DTO 可读字段；不允许 `SELECT *`、重复别名或未加入查询的表字段。
- 主表软删除过滤在 WHERE，连接表过滤在 ON；未知模型级 QueryClause 返回错误，不静默忽略。
- `JoinPage[R]` 统计 JOIN 结果行，不是去重后的主实体数；一对多会出现重复主实体。
- `JoinList[R]` 返回无隐式上限的平铺结果，DTO 扫描不执行 `AfterFind`。
- LEFT JOIN 的可空字段使用指针或 `sql.Null*`。
- 暂不封装一对多主实体分页、聚合、DISTINCT、JOIN 写入和查看已删除行。

[完整多表 API 与边界说明](docs/joins.md) · [可编译示例](examples/joins/query.go)

## 自动填充

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

- 规则需对应模型字段，未映射字段会跳过；插件在模型 `BeforeCreate/BeforeUpdate` 之前执行。
- 支持模型结构体、结构体批量及 `map[string]any`；不支持把任意 DTO 当模型更新。
- 值按每次写语句求值，错误通过 `tx.AddError` 阻止写入。
- `Overwrite=false` 保留非零结构体字段及已提供的 map 键（含显式零值）。
- 插件尊重字段权限、`Select/Omit` 和 `SkipHooks`，因此不是不可绕过的权限机制。
- 注册必须在服务开始并发处理请求之前完成。

## 软删除

声明 `gorm.DeletedAt` 即启用原生软删除：查询与 count 自动过滤，删除变成更新时间。无此字段的模型物理删除。

`BeforeDelete/AfterDelete` 保持可用，可用于删除授权等扩展。软删除不会触发 Update 回调，`UpdatedBy` 规则不会变成“删除人”填充。

物理清理、恢复、查看已删除记录应使用显式 `Unscoped()`，并在应用层单独授权。软删除不自动解决唯一索引冲突，解决方案见[设计文档](docs/design.md)。

## 事务

```go
err := dao.Transaction(ctx, func(txDAO *gormx.DAO[User]) error {
    if err := txDAO.Create(ctx, &user); err != nil {
        return err
    }
    _, err := txDAO.UpdateByID(ctx, user.ID, map[string]any{"Status": 1})
    return err
})
```

跨模型事务或指定隔离级别时，直接使用原生 GORM，并在事务内构造 DAO：

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

聚合、预加载、锁、子查询、复杂 SQL 继续使用原生 GORM；增强层不试图覆盖全部 GORM API。

## API 一览

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
| 连接初始化 | `env.Load`、`env.Open`、`env.WithConnectRetry` |

## 使用边界

- `NewDAO` 只应接收原始 DB 或事务句柄，会清除已附加的查询条件。带租户条件的 `db.Where(...)` 不会继续生效，租户条件必须显式加入 Wrapper。
- 写保护只检查条件结构，不替代业务授权、字段权限和索引评估。
- 分页是 offset 模式，深分页成本随页码增长；需要稳定快照时使用事务。
- JOIN 目前只支持平铺投影的 INNER/LEFT JOIN，不覆盖聚合、DISTINCT、一对多嵌套装配。
- 目前验证 SQLite 与 MySQL；PostgreSQL、SQL Server 等方言尚未验证。

## 运行测试

```sh
go test -count=1 ./...
go test -race -shuffle=on -count=3 -cover ./...
go build ./...
go vet ./...
go mod verify
```

没有 `GORMX_TEST_DSN` 时 MySQL 测试会跳过，**跳过不等于通过**。设置专用测试库后可跑完整 MySQL 契约：

```sh
GORMX_TEST_DSN='root:secret@tcp(127.0.0.1:3306)/gormx_test?charset=utf8mb4&parseTime=True&loc=Local' \
  go test -run '^TestMySQL' -count=1 -v .
```

模糊测试与基准：

```sh
go test -run '^$' -fuzz '^FuzzPageRequest$' -fuzztime=15s -parallel=2 .
go test -run '^$' -bench . -benchmem -benchtime=1s -count=3 .
```

## 文档

- [完整设计](docs/design.md)
- [连表查询](docs/joins.md)
- [控制器示例](examples/controller/main.go)

## 许可证

[Apache License 2.0](LICENSE)
