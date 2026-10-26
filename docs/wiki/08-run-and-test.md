# 08 运行与测试

[返回 Wiki 首页](README.md)

## 1. 环境要求

| 项 | 要求 |
| --- | --- |
| Go | 1.22+（推荐 1.26.1） |
| 数据库 | SQLite（测试/默认）；MySQL 8.x（集成测试，可选） |
| C 工具链 | 仅 SQLite 测试需要（CGO）；macOS 安装 Xcode Command Line Tools |

## 2. 构建与静态检查

```sh
go build ./...
go vet ./...
go mod verify
```

2026-10-26 本机验证结果：三项全通过。

## 3. 默认测试（无需数据库环境变量）

```sh
go test -count=1 ./...
```

范围：

- SQLite 契约测试（CRUD、Wrapper、分页、填充、软删除、事务）；
- 单表、连表、示例测试；
- 编译期类型安全测试。

```
ok  gormx
ok  gormx/examples/controller
ok  gormx/examples/joins
```

说明：

- 未设置 `GORMX_TEST_DSN` 时，所有 `TestMySQL*` 测试**明确跳过**。**跳过不等于通过。**
- `TestLambdaCompileTimeSafety` 会启动 `go test` 子进程做负例编译验证，需要 `go` 在 PATH 中；`-short` 模式下会跳过。

## 4. 完整验证（含竞态、随机顺序、覆盖率）

```sh
go test -race -shuffle=on -count=3 -cover ./...
```

包含：竞态检测、随机执行顺序、连续 3 轮、语句覆盖率。本机验证：库语句覆盖率 95.5%，Controller 包 71.9%（子进程未合并覆盖率），`examples/joins` 100%。

## 5. MySQL 集成测试

### 5.1 准备

1. 创建专用测试库（不与生产库混用）。
2. 安全设置环境变量 `GORMX_TEST_DSN`，DSN 需启用 `parseTime=True`：

   ```sh
   export GORMX_TEST_DSN='user:pass@tcp(127.0.0.1:3306)/gormx_test?charset=utf8mb4&parseTime=True&loc=Local'
   ```

   > 凭据只通过环境变量传入，不得提交到仓库或写入共享脚本；不要连接生产数据库。

3. 执行全部 MySQL 契约及快照测试：

   ```sh
   go test -run '^TestMySQL' -count=1 -v .
   ```

库测试只创建 `gx_<时间戳>_` 前缀的临时表，退出时清理，不创建/删除数据库。

### 5.2 真实 HTTP 进程测试

需要本机 `8080` 端口空闲，并授予连接账号 `CREATE/DROP DATABASE` 权限：

```sh
GORMX_HTTP_TEST_DSN="$GORMX_TEST_DSN" \
  go test -run '^TestControllerMySQLProcess$' -count=1 -v ./examples/controller
```

该测试：

1. 创建并删除独立的 `gx_http_<时间戳>` 数据库；
2. 构建并启动真实示例进程；
3. 验证首次 `AutoMigrate`、正常重启（不迁移）、接口结果与错误状态码；
4. 最后停止进程并清理数据库。

### 5.3 同时启用后重跑完整验证

```sh
GORMX_HTTP_TEST_DSN="$GORMX_TEST_DSN" \
  go test -race -shuffle=on -count=3 -cover ./...
```

## 6. 模糊测试（不依赖 MySQL）

```sh
go test -run '^$' -fuzz '^FuzzPageRequest$'      -fuzztime=15s -parallel=2 .
go test -run '^$' -fuzz '^FuzzWrapperBinding$'   -fuzztime=15s -parallel=2 .
go test -run '^$' -fuzz '^FuzzJoinBinding$'      -fuzztime=15s -parallel=2 .
```

| Fuzz 目标 | 验证内容 | 2026-10-26 基线 |
| --- | --- | --- |
| `FuzzPageRequest` | 分页整数边界 | 15s：289,928 次，无失败 |
| `FuzzWrapperBinding` | 字段白名单与普通值不改变 SQL 结构 | 15s：16,712 次，无失败 |
| `FuzzJoinBinding` | 多表别名与绑定 | 15s：273,709 次，无失败 |

## 7. 基准测试

```sh
go test -run '^$' -bench . -benchmem -benchtime=1s -count=3 .
```

基线（Apple M1 Pro，Go 1.26.1，内存 SQLite 10,000 行，每页 20）：

| 场景 | 耗时 | 分配次数 |
| --- | --- | --- |
| 字符串 Wrapper 构建 | 299.8 ns/op | 13 allocs/op |
| Lambda Wrapper 构建 | 152.9 ns/op | 6 allocs/op |
| 原生 GORM count + 分页 | 730.5 us/op | 295 allocs/op |
| 字符串 Wrapper 分页 | 731.2 us/op | 306 allocs/op |
| Lambda Wrapper 分页 | 746.8 us/op | 303 allocs/op |

> 分页数值包含 SQLite 执行成本与测量波动，仅用于回归对比，不是 MySQL 压测结果或生产 SLA。

## 8. Go 1.22 与新版 macOS 说明

本机 macOS 26.5.2 上，Go 1.22.12 默认构建的独立程序可能出现：

```
dyld: missing LC_UUID load command
```

已知规避：用已安装的 C 工具链做外部链接：

```sh
GOTOOLCHAIN=go1.22.12 GOFLAGS='-ldflags=-linkmode=external' \
  GORMX_HTTP_TEST_DSN="$GORMX_TEST_DSN" go test -count=1 ./...
```

日常开发推荐当前已验证的 Go 1.26.1。未验证的操作系统/Go 版本不在结论范围内。

## 9. 运行 Controller 示例

前置：本机安全设置 `GORMX_DSN` 指向开发数据库。

首次初始化示例表（可选，仅一次）：

```sh
GORMX_AUTO_MIGRATE=1 go run ./examples/controller
```

之后直接：

```sh
go run ./examples/controller
```

接口：

| 路由 | 实现 |
| --- | --- |
| `GET /before/users` | 原生 GORM |
| `GET /users` | 字符串 Wrapper |
| `GET /lambda/users` | Lambda Wrapper |

请求示例：

```sh
curl 'http://127.0.0.1:8080/users?name=alice&status=0&minAge=18&current=1&size=20'
```

注意事项：

- 示例只监听 `127.0.0.1`，无鉴权，不得直接暴露公网。
- 不自动插入数据；`GORMX_AUTO_MIGRATE=1` 只建表。
- 测试会比较三种接口的 JSON 响应，包含零值条件、无记录、越界页和非法参数。

## 10. 跑连表示例测试

```sh
go test -v ./examples/joins
```

验证租户过滤的 `Employee → Department` LEFT JOIN 投影分页，见 [examples/joins/query.go](../../examples/joins/query.go)。

## 11. 测试源码地图

| 文件 | 验证范围 |
| --- | --- |
| [gormx_test.go](../../gormx_test.go) | SQLite/MySQL 共用 CRUD、Wrapper、分页、填充、并发契约 |
| [edge_test.go](../../edge_test.go) | 复合主键、删除 Hook、错误传递、并发更新、嵌入字段 |
| [contract_extended_test.go](../../contract_extended_test.go) | 字段缓存、写权限、事务、软删除恢复、真实 MySQL 快照 |
| [join_test.go](../../join_test.go) / [join_fuzz_test.go](../../join_fuzz_test.go) | 连表、自连接、DTO、行分页、软删除位置、绑定/别名 fuzz |
| [typesafety_test.go](../../typesafety_test.go) | 编译期正/负例 |
| [fuzz_test.go](../../fuzz_test.go) | 分页边界、字段白名单、绑定 fuzz |
| [benchmark_test.go](../../benchmark_test.go) | Wrapper 构建与分页基准 |
| [Controller 测试](../../examples/controller/main_test.go) / [HTTP 测试](../../examples/controller/http_test.go) | 三处理器一致性与真实进程 |

## 12. 安全须知

- 库测试、HTTP 测试、模糊测试**只操作专用临时库/临时表**，退出时清理。
- 凭据仅通过环境变量注入。
- 不要对**生产数据库**运行这些测试。
- 未特别说明的输出（如覆盖率）不能作为无缺陷证明；覆盖率与有限时长 fuzz 都有上限。