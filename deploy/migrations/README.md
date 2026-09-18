# 数据库迁移

迁移按服务隔离：`deploy/migrations/<service>/000001_description.sql`。迁移文件必须可审计、可重复检查，涉及大表时记录锁风险和回滚/补偿方案。
