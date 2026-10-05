# OpenSearch 本地索引资源（search-indexer / search-query）

本目录只放 **词典、示例文档和说明**。索引结构（settings + mappings + 分词器）的唯一来源是
`services/search-indexer/internal/esclient.IndexBody`，本目录不保存第二份 mapping：

```powershell
# 查看当前进程会建出什么结构（-pretty 便于人工 diff）
go run ./services/search-indexer/cmd/esmapping -analyzer cjk -pretty
```

## 目录

| 路径 | 作用 |
|---|---|
| `analysis/go_video_stopwords.txt` | 停用词，挂在**写入**分析器上；改内容需重建索引才对已写入文档生效 |
| `analysis/go_video_synonyms.txt` | 同义词，只挂**查询**分析器（`updateable`）；改内容集群 reload 即可生效，不必重建 |
| `samples/content.sample.ndjson` | 一条一行的样例文档（不带 bulk 动作行），字段与 `esclient.ContentDoc` 一致 |
| `scripts/es-init.ps1`（仓库根的 scripts 目录） | 分词/文档验证脚本：等健康、查插件、建开发索引、灌样例、`_analyze` 与冒烟查询 |

compose 把 `analysis/` 只读挂载到集群容器的 `/usr/share/opensearch/config/analysis`，
所以配置里的路径是容器内绝对路径（见 `services/search-indexer/etc/searchindexer.v1.yaml`）。

## 分词策略选择

`OpenSearch.Analyzer.Kind` 决定 tokenizer/filter 组合，`scripts/es-init.ps1` 会实测集群插件后给结论：

| Kind | tokenizer（写入 / 查询） | 依赖 | 说明 |
|---|---|---|---|
| `cjk`（默认） | `standard` / `standard` + 内置 `cjk` bigram filter | 无插件 | 开箱可用，中文靠二元组召回；精度低于 ik，停用词只能命中二元组 |
| `ik` | `ik_max_word` / `ik_smart` | `analysis-ik` 插件 | 召回与精度最好，中文业务推荐；插件需自建镜像 |
| `smartcn` | `smartcn` / `smartcn` | `analysis-smartcn` 插件 | 若无该插件，建索引会直接报错，不会静默降级 |
| `standard` | `standard` / `standard` | 无 | 中文整串成为一个 token，只能做英文/ID 调试，禁止用于生产 |

`analysis-ik` 不是 OpenSearch 发行版自带的，需要构建带插件的镜像，例如：

```dockerfile
FROM opensearchproject/opensearch:2.11.1
RUN ./bin/opensearch-plugin install --batch https://get.infini.cloud/opensearch/analysis-ik/2.11.1
```

插件版本必须与集群版本一致。判断当前集群实际装了什么：`GET /_cat/plugins?format=json`
（`es-init.ps1` 每次都会打印）。

## 词典维护规则

1. 两个文件都必须是 **UTF-8 无 BOM**、每行一条、`#` 开头为注释；集群启动时读一次，
   改文件后 `POST /_reload_search_analyzers/<index>`（同义词）或重建索引（停用词）才生效。
2. 停用词按**实际产出的 token** 写：`cjk` 策略下中文 token 是二元组（如 `的了`），
   单字 `的` 命中不了；换 `ik`/`smartcn` 后应按词写。
3. 同义词只在查询侧展开，不影响已索引内容，因此**不能**用来修正错误的写入分词。
4. 词典变更属于搜索效果变更，需要连同 `docs/roadmap.md` 的召回说明一起评审。

## 索引结构与上线流程

- 物理索引名由 `repository.Options.NewIndexName` 生成：`<alias>_<schema>_<unix>`，
  例如 `go_video_content_v1_1760000000`。别名（默认 `go_video_content`）才是 search-query 的查询目标。
- `mappings._meta` 会落地 `schema_version`、`analyzer`（以及词典路径）。
  改分词族等于改结构：**递增 `OpenSearch.SchemaVersion` → 走 search-indexer 的重建任务 → 切别名**，
  禁止原地改已上线索引的 mapping（`AGENTS.md` §5、§10）。
- `es-init.ps1` 只写显式命名的开发索引，并且拒绝写「已经是别名」的目标；
  重建与别名切换由 `POST /admin/search/index/rebuild`（gateway/admin → search-indexer）驱动，不在脚本里做。

## 本地一次跑通

```powershell
docker compose -f deploy/docker-compose/docker-compose.yml up -d opensearch
# 本地匿名集群：把 AllowAnonymousWrites 设为 true（生产必须 false）
.\scripts\es-init.ps1 -Endpoint http://127.0.0.1:9200 -Index go_video_content_dev_v1 -Analyzer cjk
```

脚本输出即结论：集群健康 → 插件清单 → 建索引结果 → `_analyze` 切词 → 样例文档写入条数 →
中文冒烟查询命中数 → 别名现状。任何一步失败都以非零退出码结束，不会「看起来成功」。
