# 验证记录

## 2026-09-23 放行授权联动部件状态

验证日期：2026-09-23（Asia/Shanghai）

### 代码质量

以下命令均实际执行成功：

```bash
cd backend
gofmt -w internal/model/release_authorization.go internal/repository/release_authorization.go
go test ./...
go test -race ./...
go vet ./...
go build ./...

cd ../frontend
npm run typecheck
npm run build
```

### 服务层回归（`internal/service/part_linkage_test.go`）

- 部件 `hold` 时提交复核被阻止：返回 `PartLinkBlockedError`（含部件编号），授权保持 `draft` v1，阻塞原因持久化并可通过 `Get` 回读，`link_blocked` 审计写入。
- 部件 `retired` 时批准被阻止：授权保持 `review`；review 状态授权不被联动吊销。
- 权限顺序不变：operator 批准仍 403、同人复核仍职责分离拒绝，门禁在权限检查之后执行。
- 部件恢复后提交成功，持久化的阻塞原因被清除。
- 无关联部件（编号不存在）时双人复核流程不受影响。
- 部件进入 `hold`：approved 与 restricted 授权在同一事务记为 `revoked`，批准版本保留在版本链中，`linkOutcome` 记录联动结果，审计数量精确。
- 并发 8 个相同版本号的部件迁移：恰好 1 个成功，其余版本冲突或状态机拒绝，授权只被吊销一次。
- 注入版本链冲突强制联动失败：部件迁移整体回滚，授权与审计无任何半更新。
- 列表接口携带实时部件状态。

### SQLite 端到端 API（开发模式实际运行）

- 部件 `received -> hold` 后提交复核：HTTP 409 `part_link_blocked`，报文含部件编号；`GET` 回读显示 `linkedPartStatus=hold` 与持久化阻塞原因，状态与版本不变。
- 部件恢复 `inspection` 后提交复核成功，阻塞原因清空；operator 自批仍 403；reviewer 批准为 v3。
- 部件 `inspection -> hold`：授权同事务变为 `revoked` v4，版本链保留 approved v3，`linkOutcome` 记录部件编号与目标状态。
- 种子部件 AP-004 `released -> retired`：RA-003 联动吊销，批准版本保留。
- 6 个并发相同版本号的部件 `hold` 迁移：恰好 1 个 HTTP 200，其余 409/422；授权恰好吊销一次（4 个版本、4 条审计）。
- 授权与部件审计历史顺序正确，`link_blocked` 审计 before=after=draft。
- 放行授权列表实时回读每条记录的部件状态、阻塞原因和联动结果。

## 2026-08-22 基线验证

## 代码质量

以下命令均实际执行成功：

```bash
cd backend
gofmt -w <本次修改的 Go 文件>
go test ./...
go test -race ./...
go vet ./...
go build ./...

cd ../frontend
npm run typecheck
npm run build

cd ..
docker compose config --quiet
```

- 非测试 Go 代码：3239 行。
- 非测试 `.go` 文件：38 个。
- 服务层回归测试覆盖证书异人发布、放行双人复核、operator 越权、同人伪装 reviewer、复核后编辑锁定、版本与审计数量。

## 空卷 Compose 与 API

执行 `KEEP_RUNNING=1 ./scripts/validate.sh`，脚本先运行 `docker compose down -v --remove-orphans`，再从空命名卷构建并启动 MySQL、Redis、MinIO、backend、frontend。验证结果：

- `/healthz` 返回 database=`ready`、redis=`ready`。
- `viewer` 可读取部件与会话，POST 写入返回 HTTP 403。
- `operator` 创建放行草稿 v1，并提交为 review v2；其自批请求返回 HTTP 403。
- `reviewer` 独立批准为 v3，`submittedBy=operator`、`reviewedBy=reviewer`。
- 三个放行版本分别保留 actor、request ID、状态、证据和原因。
- `operator` 创建证书 v1；其发布请求返回 HTTP 403；`reviewer` 发布为 valid v2。
- 证书版本保留 `preparedBy=operator`、`verifiedBy=reviewer` 和请求 ID。
- 实体审计历史包含 `gb515-auth-create`、`gb515-auth-review`、`gb515-auth-approve`；审计汇总覆盖两个独立操作者。

## 内置 Browser

仅使用 Codex 内置 Browser，在 `http://127.0.0.1:18515` 实际验证：

- 登录页可选择 admin/reviewer/operator/viewer 并建立真实 JWT 会话。
- `/parts`、`/inspections`、`/certificates`、`/authorizations`、`/audit` 五个页面均加载成功。
- 在部件页通过确认对话框将 AP-001 从 received 推进到 inspection，页面刷新后状态正确。
- `PartStatusBadge` 在部件与放行页渲染；`CertificatePanel` 在检查与证书页展示版本、操作者和请求 ID。
- operator 在 review/approved 放行记录上只看到“等待复核员”，没有批准按钮；draft 仍可提交 review。
- viewer 不显示新增和状态推进按钮，只显示只读状态。
- 审计页展示 operator/reviewer 的创建、提交、批准、发布动作及对应请求 ID。
- 桌面全页截图和 390 x 844 移动端截图已检查；移动端表格采用稳定横向滚动，不挤压文字。
- 最终控制台 `error`/`warning` 日志为空。

## 清理

验证结束后执行：

```bash
docker compose down -v --remove-orphans
```

并确认没有名称包含 `aircraft-component-airworthiness-release` 的运行中容器。
