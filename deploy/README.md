# deploy

存放本地、测试和生产部署资源。生产 Secret 不提交仓库。

```text
deploy/
├── docker-compose/       # 本地依赖和最小服务集
├── k8s/                  # Deployment、Service、ConfigMap、HPA
├── migrations/           # 按服务管理的数据库迁移
└── README.md
```
