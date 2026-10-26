# 多表查询：第一阶段

当前已实现独立、只读、不可变的 `JoinQuery`。它与单表 `DAO[T]` / `Wrapper[T]` 并存，不修改现有接口语义，也不尝试一次覆盖 GORM 的所有查询能力。

## 已支持

| 能力 | 当前约定 |
| --- | --- |
| INNER JOIN / LEFT JOIN | 支持多表串联、自连接；每个表实例必须有独立别名 |
| ON 条件 | 一个连接新旧表的字段等值条件，加若干 AND 条件；可用分组表达额外条件 |
| WHERE 条件 | 跨表字段的值比较、范围、集合、LIKE、NULL、AND/OR |
| 字符串字段 | `table.Col("Name")`，接受 Go 字段名或模型数据库列名 |
| 类型字段 | `Bind(table, Field(...))`，约束模型绑定与条件值类型 |
| 输出投影 | 显式选择列并设置唯一输出别名，扫描到独立 DTO |
| 排序与分页 | 排序字段绑定表身份；自动补各表主键作为分页排序兜底 |
| 软删除 | 支持原生 `gorm.DeletedAt`；连接表的过滤放入 ON |
| 上下文、事务、并发 | 显式 Context、保留事务连接、不可变查询可复用 |

## 主要 API

```go
employees := gormx.TableOf[Employee]("e")
departments := gormx.TableOf[Department]("d")

query, err := gormx.NewJoinQuery(db, employees)
```

`TableOf` 只创建模型与别名的绑定，不创建数据库表。模型必须是非指针结构体。别名限定为字母/下划线开头、后续字母/数字/下划线，长度 1~63；同一查询的别名忽略大小写后不能重复。

每次 `TableOf` 都创建新的身份，即使模型和别名一样，也不能用另一次创建的实例替代已加入查询的表。这能避免来自其他查询的字段混入。

```go
query = query.LeftJoin(departments,
    gormx.On(employees.Col("DepartmentID"), departments.Col("ID")).
        And(departments.Col("TenantID").Eq(tenantID)),
)
```

`On` 的等值条件必须连接本次新加入的表与此前已存在的表。拒绝引用未来表、漏掉新表、重复表别名等错误。关联字段支持可空外键，但两列的 SQL 类型兼容性由调用者负责，不作编译期证明。

附加条件始终与基础等值连接条件作 AND。例如复合关联条件：

```go
on := gormx.On(employees.Col("DepartmentID"), departments.Col("ID")).
    And(gormx.JoinEqual(employees.Col("TenantID"), departments.Col("TenantID")))
```

不提供 CROSS JOIN，也不允许把任意 SQL 字符串作为 ON。

## 值条件与不可变构造

```go
name := gormx.Bind(employees,
    gormx.Field(func(e *Employee) *string { return &e.Name }),
)
id := gormx.Bind(employees,
    gormx.Field(func(e *Employee) *uint { return &e.ID }),
)

filtered := query.
    Where(employees.Col("TenantID").Eq(tenantID)).
    WhereIf(keyword != "", name.Like("%"+keyword+"%")).
    Where(gormx.JoinAny(id.Eq(1), id.Between(10, 20)))
```

- 方法返回新的查询，必须接住返回值；原查询保持不变。
- 列支持 `Eq/Ne/Gt/Ge/Lt/Le/Between/In/NotIn/Like/NotLike/IsNull/IsNotNull`。
- `In/NotIn` 使用可变参数：`id.In(ids...)`；字符串字段版可写 `employees.Col("ID").In(1, 2, 3)`。
- 与单表 API 一样，空 IN 恒假、空 NOT IN 恒真；空 `JoinAll/JoinAny` 返回错误。
- 普通值走参数绑定；LIKE 的 `%` 和 `_` 保留数据库通配符语义。
- `Bind` 不能将 Employee 的字段绑定给 Department；绑定为 `uint` 的字段不能用字符串调用 `Eq`。
- `Like` 的列类型、关联列的 SQL 类型及更新后的数据库约束不属于完整编译期 SQL 类型系统。
- 不要并发修改传入的指针或切片元素，也不要将用户输入包装为 `gorm.Expr`；显式表达式不是普通参数。

## DTO 与输出字段

```go
type EmployeeDepartment struct {
    EmployeeID     uint
    EmployeeName   string
    DepartmentName *string
}

query = query.Select(
    employees.Col("ID").As("employee_id"),
    employees.Col("Name").As("employee_name"),
    departments.Col("Name").As("department_name"),
)

rows, err := gormx.JoinList[EmployeeDepartment](ctx, query)
page, err := gormx.JoinPage[EmployeeDepartment](ctx, query, request)
```

Go 不支持给普通方法单独声明新的泛型类型参数，所以返回 DTO 的入口使用顶层泛型函数，而不是 `query.Page[DTO](...)`。

`Select` 替换已有投影，不追加；列表和分页必须显式投影，不提供 `SELECT *`。每个输出别名必须符合标识符规则且唯一，并匹配 DTO 的可读数据库字段名，支持 `gorm:"column:..."` 标签与数据库配置的命名策略。不匹配在执行查询前报错，避免静默返回未填充字段。

DTO 必须是非指针结构体；未选择的 DTO 字段保持零值。字段值转换由 GORM/数据库驱动处理。LEFT JOIN 产生的 NULL 应使用 `*string`、`*uint` 或 `sql.NullString` 等可空类型接收，不能用普通零值来区分“没有关联记录”与“关联值恰好为空”。

查询通过 GORM `Scan` 扫描结果，不应用 DTO 自身的软删除条件，也不调用源模型或 DTO 的 `AfterFind`。DTO 是投影，不是本次查询的持久化模型。

## 软删除与权限

假设用户关联的部门已经软删除：

- 主表 `gorm.DeletedAt` 条件进入 WHERE，排除已删除用户。
- 部门的 `gorm.DeletedAt` 条件进入 ON，保留用户，但其部门输出为 NULL。
- INNER JOIN 同样在 ON 中过滤已删除部门，此时没有有效部门的用户不会返回。
- 调用者额外将部门条件放到 WHERE，会主动过滤 NULL 扩展行，这是显式业务选择。

实现读取各源模型的 QueryClause 元数据，支持 GORM 原生 `SoftDeleteQueryClause` 及其零值配置。遇到其他模型级 QueryClause 会返回错误，避免静默丢弃可见性约束。第三方标志位软删除、任意租户插件等不能假设自动兼容。

**这不是全局租户权限插件。** `NewJoinQuery` 与 `NewDAO` 一样，只应接收原始 DB 或事务句柄，不保留预先附加的 WHERE、Scope、排序和分页。权限必须覆盖每张参与查询的表：

```go
query = query.
    LeftJoin(departments,
        gormx.On(employees.Col("DepartmentID"), departments.Col("ID")).
            And(departments.Col("TenantID").Eq(tenantID))).
    Where(employees.Col("TenantID").Eq(tenantID))
```

`tenantID` 必须来自可信身份上下文，不能直接接受客户端任意租户参数。字段白名单验证也不等于业务字段授权。

当前不提供 JoinQuery 的 `Unscoped`/恢复/删除/更新方法。需要查看已删除记录或连表写入时，使用单独授权的原生 GORM 路径。安装在 DB 上的 Query/Row Callback 仍由 GORM 执行，自定义插件需针对无源 Model 的多表 DTO 查询单独适配与测试。

## 分页单位

`JoinPage` 的单位始终是 **JOIN 结果行**：

1. 同样的 FROM、JOIN、ON 和 WHERE 执行 `COUNT(*)`，不带投影排序或分页。
2. 再执行显式投影的 SELECT，应用排序及 LIMIT/OFFSET。
3. 成功时固定两次查询，包括总数为零及越界页；任一步失败不返回半成功 Page。

页码与条数复用 `PageRequest`：默认第 1 页、20 条，上限 500 条，拒绝负数及 offset 溢出。分页追加每个表的主键排序，帮助区分重复主实体对应的不同结果行；参与表没有主键时，调用者仍需提供能区分结果行的排序。

一个用户有两条订单时，JOIN 产生两行，总数为 2，不会自动去重为一个用户。仅将 count 改成 DISTINCT 不能解决子列表被 LIMIT 截断的问题，所以本阶段不提供这种隐式变换。

若需要分页完整用户对象及其所有订单，下一阶段应实现“分页主实体 ID，再批量加载关联集合”，或使用原生 GORM `Preload`；查询次数不再保证为两次。

两次语句默认不是一致快照，需要时在显式事务内构造 JoinQuery。事务连接会保留：

```go
err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
    query, err := gormx.NewJoinQuery(tx, employees)
    if err != nil {
        return err
    }
    // 在此配置 Join、权限与投影，再调用 JoinPage。
    _ = query
    return nil
})
```

隔离级别按数据库和业务需求选择；不用一次普通事务承诺所有数据库都具备相同快照行为。

## 验证与后续

已完成 SQLite/MySQL 双数据库验证：左/内连接、自连接、多表串联、软删除位置、显式租户过滤、NULL 输出、类型绑定、别名冲突、越界字段、DTO 校验、查询次数、结果行分页、取消 Context、事务连接、错误传递及并发复用。

连表新增了编译期负例测试和别名/参数绑定模糊测试；测试命令与最新覆盖率见 [README](../README.md#本次验证结果)。[完整示例](../examples/joins/query.go) 可通过 `go test -v ./examples/joins` 验证。

尚未实现：一对多主实体分页及嵌套装配、EXISTS 构造器、GROUP BY/HAVING、DISTINCT、聚合表达式、RIGHT/FULL/CROSS JOIN、子查询表、JOIN 写入、全局权限插件。生产规模 JOIN 性能及其他数据库方言也未验证。
