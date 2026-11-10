# RecallWeave


基于Go的可追溯个人历史记忆Agent：保留原始消息，将会话整理为可检索记忆，让Agent按需检索、核对原文，并返回结构化引用。

主要技术：Go、Gin、GORM、PostgreSQL、pgvector、SSE、MCP。

## 技术文档

- [完整Markdown技术文档](docs/technical/RecallWeave-Technical-Guide.md)
- [分章阅读与源码学习路线](docs/technical/README.md)
- [全部架构图、时序图和状态图](docs/technical/08-diagram-atlas.md)
- [API、配置、启动与排障](docs/technical/05-api-config-operations.md)
- [测试、真实模型验收和面试说明](docs/technical/06-validation-and-interview.md)

当前主要为后端工程原型，frontend目录尚无完整界面。文档区分源码实现、测试支持、实测结果和已知边界；四个真实模型场景不作为总体准确率。

## 本地入口

数据库、pgvector和本机配置准备好后，在仓库根目录运行：

```powershell
go build ./...
go test ./... -count=1 -timeout 90s
go run ./cmd/server
```

`cmd/server`固定读取本机`config/config.yaml`；该文件不应提交真实凭据。请先阅读[脱敏配置与运行前提](docs/technical/05-api-config-operations.md)。默认普通测试不调用真实模型。

另有stdio MCP入口`cmd/mcp`与远程连接探针`cmd/mcp-probe`。它们的职责和权限不同，详见技术文档。
