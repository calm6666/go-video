# deploy

存放本地、测试和生产部署资源。生产 Secret 不提交仓库。

```text
deploy/
├── docker-compose/       # 仅本地依赖：MySQL、Redis、MinIO、Redpanda、OpenSearch
├── k8s/                  # Deployment、Service、ConfigMap、HPA（约定见其 README，清单尚未落地）
├── migrations/           # 按服务管理的数据库迁移（一服务一目录）
├── opensearch/           # 搜索/行为索引的分词器、索引与文档脚本
└── README.md
```

`docker-compose.yml` 当前只编排基础设施，不含任何 `services/*` 或 `gateway/*` 容器：
本仓尚无镜像构建与 Registry 约定，服务按 [docs/commands.md](../docs/commands.md) 的 `go run` 方式本地起。
Kubernetes 清单同理：`deploy/k8s/README.md` 写的是落地约定，不是已有产物；
在 CI 出镜像之前不要手写 38 份 Deployment 当作「已部署」。
