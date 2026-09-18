# api

存放需要跨服务共享的 protobuf、事件 schema、错误码协议和兼容性说明。服务内部 API 优先放在对应 `services/<service>/api` 或 `rpc`，不要把所有接口集中成一个不可维护的大协议文件。

目录建议：

```text
api/
├── proto/<domain>/v1/*.proto
├── events/<domain>/v1/*.json 或 *.proto
└── README.md
```
