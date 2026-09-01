# 外部系统接入说明

当前生产部署启用了 `direct_upload.index_only: true`。S3 Center 是索引、额度与授权控制平面，任何上传/下载文件内容都直接在第三方系统与实际 Cloudflare R2/S3 之间传输。

完整公开接入契约：

- [索引模式第三方接入 API](INDEX-MODE-API.md)
- [OpenAPI 3.1 定义](index-mode-openapi.yaml)

## 重要变化

第三方不再把 `https://s3-api.aiid.qzz.io` 当作标准 S3 Endpoint，也不再配置 S3 Center 的 Region、Access Key、Secret Key 或 Path-Style。它需要配置：

```text
Control API Base URL = https://s3-api.aiid.qzz.io
Bearer Token         = 管理员独立签发
Logical Bucket       = assets
Object Prefix        = 管理员分配，例如 partners/acme/
```

调用方先向控制 API 申请一个短时操作，收到实际 R2/S3 的预签名 URL、HTTP 方法和签名 Header，再直接请求该上游地址。浏览器临时访问链接就是 `POST /v1/objects/presign` 返回的 `request.url`；它可能对应多个 R2 Account 中的任意一个，文件流量不经过 S3 Center。

标准 S3 Endpoint + SigV4 只适用于数据代理模式。若第三方产品只能填写通用 S3 Endpoint 且不能实现两阶段控制 API，它与当前零代理部署不兼容，需要单独开发适配器或改用代理模式。

新建直传对象的上游物理键会保留短且安全的文件扩展名，例如逻辑 `video.mp4` 对应 `__objects/du_xxx.mp4`。会话 ID 仍保证唯一；历史无扩展名对象继续由数据库索引定位，无需迁移。

管理端的动态 R2 Account、代理出口和免费额度说明见 [R2-ACCOUNT-MANAGEMENT.md](R2-ACCOUNT-MANAGEMENT.md)。
