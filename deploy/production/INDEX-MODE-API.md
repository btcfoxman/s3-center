# S3 Center 索引模式第三方接入 API

版本：`v1`

生产控制端点：`https://s3-api.aiid.qzz.io`

逻辑 Bucket：`assets`

本文是第三方系统接入 S3 Center **索引模式**的公开契约。索引模式下，S3 Center 只负责身份验证、逻辑目录、R2/S3 选址、容量预留和限时授权；文件内容始终由调用方直接上传到、下载自实际的上游 R2/S3。

机器可读定义见 [`index-mode-openapi.yaml`](index-mode-openapi.yaml)。

## 1. 接入参数

第三方只需要以下配置：

| 配置 | 值或来源 | 用途 |
|---|---|---|
| Control API Base URL | `https://s3-api.aiid.qzz.io` | 创建授权、查询索引、提交上传 |
| Bearer Token | 由 S3 Center 管理员分配 | 鉴权及 Bucket/前缀隔离 |
| Logical Bucket | `assets` | 逻辑命名空间 |
| Object Prefix | 由管理员分配，例如 `partners/acme/` | 该调用方可操作的键前缀 |

索引模式没有提供给客户端填写的 S3 `Region`、`Access Key ID`、`Secret Access Key`、`Path-Style` 或“公开访问基址”。这些是标准 S3 代理模式的参数；在零数据代理模式下使用固定 S3 Endpoint 无法把同一个 SigV4 请求无损改签并分发到多个 R2 Account。

所有控制请求使用：

```http
Authorization: Bearer <S3_CENTER_TOKEN>
Content-Type: application/json
```

Token 应只保存在服务端或可信客户端。不要把长期 Bearer Token 写入公开网页、桌面安装包或移动端包中。

### 1.1 管理员为第三方签发 Token

每个第三方使用独立 Token，并在服务端配置中限制逻辑 Bucket、对象前缀和最小权限：

```yaml
direct_upload:
  tokens:
    - name: partner-acme
      token: "${S3_CENTER_TOKEN_ACME}"
      scopes: ["uploads:write", "uploads:read", "objects:read", "objects:list", "objects:delete"]
      buckets: ["assets"]
      prefixes: ["partners/acme/"]
```

只读系统可只分配 `objects:read`、`objects:list`；无需删除时不要分配 `objects:delete`。真实 Token 只写入服务器 Secret/`.env`，不能提交到 Git。新增、轮换或吊销 Token 后需要重启当前服务进程生效。

## 2. 数据路径

```text
第三方系统 ──小型 JSON 控制请求──> S3 Center ──索引/配额──> PostgreSQL
     │                                  │
     │                                  └─ HEAD / DELETE / Multipart finalize
     │
     └──────── 文件字节 PUT / GET ───────────────────────> 实际 R2/S3
```

- 上传响应里的 `upload.url`、下载响应里的 `request.url` 是上游真实预签名 URL，域名可能因对象所在的 R2 Account/S3 Provider 而不同。
- 对象内容不会经过 `s3-api.aiid.qzz.io`，S3 Center 不保存 R2 副本或缓存。
- S3 Center 仍会发起不包含对象响应体的控制调用，例如 `HEAD` 校验、`DELETE`、创建/完成 Multipart。
- URL 仅在响应的 `expires_at` 前有效，不能当作永久公开链接保存。每次访问前重新申请即可。

## 3. 单文件上传（不超过 256 MiB）

### 3.1 创建并预留容量

```http
POST /v1/uploads
Authorization: Bearer <token>
Content-Type: application/json
Idempotency-Key: <UUID>

{
  "bucket": "assets",
  "key": "partners/acme/2026/report.pdf",
  "size": 482913,
  "content_type": "application/pdf"
}
```

`size` 必须是最终对象的精确字节数。`Idempotency-Key` 必填；同一业务上传重试时必须复用原值，不同文件必须使用新值。

成功返回 `201`：

```json
{
  "id": "du_4a6d...",
  "status": "url_issued",
  "declared_bytes": 482913,
  "expires_at": "2026-09-01T15:10:00Z",
  "upload": {
    "method": "PUT",
    "url": "https://<account>.r2.cloudflarestorage.com/<bucket>/__objects/du_4a6d....pdf?...",
    "headers": {
      "Content-Type": ["application/pdf"],
      "X-Amz-Meta-Upload-Id": ["du_4a6d..."]
    },
    "expires_at": "2026-09-01T15:10:00Z"
  }
}
```

### 3.2 直接 PUT 到上游

严格使用响应中的 `method`、`url` 和全部 `headers`；不能修改 URL、Host、查询参数或已签名 Header。

```bash
curl --fail-with-body -X PUT '<upload.url>' \
  -H 'Content-Type: application/pdf' \
  -H 'X-Amz-Meta-Upload-Id: du_4a6d...' \
  --data-binary @report.pdf
```

上游返回 2xx 后，文件字节已经在实际 R2/S3，但逻辑目录尚未生效。

### 3.3 提交逻辑索引

```http
POST /v1/uploads/{id}/complete
Authorization: Bearer <token>
```

S3 Center 会 `HEAD` 上游对象，验证字节数、Content-Type 与会话标记后，原子提交索引和配额。成功返回 `200`：

```json
{
  "id": "du_4a6d...",
  "status": "committed",
  "logical_key": "assets/partners/acme/2026/report.pdf",
  "declared_bytes": 482913,
  "expires_at": "2026-09-01T15:10:00Z"
}
```

提交超时可安全重试。放弃未提交上传可调用 `DELETE /v1/uploads/{id}`。

## 4. 大文件原生 Multipart 上传

当前最大声明对象为 5 TiB、最多 10,000 个 Part。除最后一个 Part 外，Cloudflare R2/S3 通常要求每个 Part 至少 5 MiB；建议使用 8–64 MiB 固定分片。

### 4.1 创建 Multipart

```http
POST /v1/multipart
Authorization: Bearer <token>
Content-Type: application/json
Idempotency-Key: <UUID>

{
  "bucket": "assets",
  "key": "partners/acme/videos/demo.mp4",
  "size": 734003200,
  "content_type": "video/mp4"
}
```

返回 `201`，其中 `id` 是 S3 Center 会话 ID。上游 Multipart Upload ID 不会暴露。

### 4.2 为每个 Part 申请 URL 并直传

```http
POST /v1/multipart/{id}/parts/presign
Authorization: Bearer <token>
Content-Type: application/json

{"part_number": 1}
```

成功返回：

```json
{
  "part_number": 1,
  "request": {
    "method": "PUT",
    "url": "https://<actual-upstream>/...?partNumber=1&...",
    "headers": {},
    "expires_at": "2026-09-01T15:10:00Z"
  }
}
```

调用方直接 PUT 该 Part，并保存上游响应头 `ETag`（包括其实际值，提交时原样回传）。Part 可并行、可覆盖重传；完成清单必须按 `part_number` 严格升序且不能重复。

### 4.3 完成 Multipart

```http
POST /v1/multipart/{id}/complete
Authorization: Bearer <token>
Content-Type: application/json

{
  "parts": [
    {"part_number": 1, "etag": "\"etag-part-1\""},
    {"part_number": 2, "etag": "\"etag-part-2\""}
  ]
}
```

S3 Center 请求上游组装后再执行 `HEAD`，仅当最终大小等于创建时的 `size` 才提交索引。完成请求超时可原样重试。放弃时调用：

```http
DELETE /v1/multipart/{id}/abort
Authorization: Bearer <token>
```

## 5. 查询逻辑索引

```http
GET /v1/objects?bucket=assets&prefix=partners/acme/&max_keys=100&start_after=<last-key>
Authorization: Bearer <token>
```

返回来自 S3 Center 数据库的逻辑目录，不扫描上游、不读取文件内容：

```json
{
  "bucket": "assets",
  "prefix": "partners/acme/",
  "objects": [
    {
      "key": "partners/acme/2026/report.pdf",
      "size_bytes": 482913,
      "updated_at": "2026-09-01T12:03:12Z"
    }
  ],
  "is_truncated": false,
  "next_start_after": ""
}
```

`max_keys` 范围为 1–1000。若 Token 限制了前缀，查询的 `prefix` 也必须位于该授权前缀之下。

## 6. 直接访问文件

### 6.1 申请 GET 或 HEAD

```http
POST /v1/objects/presign
Authorization: Bearer <token>
Content-Type: application/json

{
  "bucket": "assets",
  "key": "partners/acme/2026/report.pdf",
  "method": "GET",
  "expires_seconds": 10800
}
```

`method` 只能是 `GET` 或 `HEAD`。生产环境默认与上限均为 **3 小时（10,800 秒）**：`expires_seconds` 省略、为 `0` 或填写 `10800` 都会得到 3 小时授权；调用方也可以填写更小的正整数主动缩短有效期。

返回 `200`：

```json
{
  "bucket": "assets",
  "key": "partners/acme/2026/report.pdf",
  "size_bytes": 482913,
  "request": {
    "method": "GET",
    "url": "https://<actual-upstream>/__objects/du_4a6d....pdf?...",
    "headers": {},
    "expires_at": "2026-09-01T15:08:12Z"
  }
}
```

调用方随后直接请求 `request.url`：

```bash
curl --fail-with-body -X GET '<request.url>' -o report.pdf
```

这个 URL 就是本次临时访问链接。它对应索引中实际保存该对象的某个 R2/S3，不经过 S3 Center。它不是固定公开链接，过期后需重新申请。

### 6.2 删除

```http
POST /v1/objects/delete
Authorization: Bearer <token>
Content-Type: application/json

{"bucket":"assets","key":"partners/acme/2026/report.pdf"}
```

成功返回 `204`。S3 Center 协调删除实际上游对象和逻辑索引；删除没有对象响应体，因此仍属于控制平面操作。

### 6.3 管理员按目录删除

第三方 Bearer Token 只能按对象删除，避免一个业务调用误删整棵目录。管理员可以在管理端对象浏览器选择 **删除目录**，或使用 Admin API：

```bash
curl -X DELETE \
  -H "X-Admin-Token: $ADMIN_API_TOKEN" \
  "https://s3-api.aiid.qzz.io/admin/api/objects?prefix=assets/partners/acme/2026/"
```

目录删除以逻辑 Key 前缀为范围。S3 Center 从索引定位每个对象的实际 R2/S3 和物理 Key，通过各账号配置的代理调用上游删除 API，并同步释放索引额度；临时失败的物理副本会进入 Cleanup Queue 重试。文件内容不会先下载到 S3 Center。

## 7. 服务端调用示例

### JavaScript / TypeScript

```javascript
const control = "https://s3-api.aiid.qzz.io";
const auth = {Authorization: `Bearer ${process.env.S3_CENTER_TOKEN}`};

async function download(bucket, key) {
  const grant = await fetch(`${control}/v1/objects/presign`, {
    method: "POST",
    headers: {...auth, "Content-Type": "application/json"},
    body: JSON.stringify({bucket, key, method: "GET", expires_seconds: 10800}),
  });
  if (!grant.ok) throw new Error(await grant.text());
  const {request} = await grant.json();
  const objectResponse = await fetch(request.url, {
    method: request.method,
    headers: request.headers,
  });
  if (!objectResponse.ok) throw new Error(`upstream HTTP ${objectResponse.status}`);
  return objectResponse;
}
```

### Python

```python
import os
import requests

CONTROL = "https://s3-api.aiid.qzz.io"
AUTH = {"Authorization": f"Bearer {os.environ['S3_CENTER_TOKEN']}"}

grant = requests.post(
    f"{CONTROL}/v1/objects/presign",
    headers=AUTH,
    json={"bucket": "assets", "key": "partners/acme/2026/report.pdf",
          "method": "GET", "expires_seconds": 10800},
)
grant.raise_for_status()
request = grant.json()["request"]

with requests.request(request["method"], request["url"],
                      headers=request.get("headers", {}), stream=True) as source:
    source.raise_for_status()
    with open("report.pdf", "wb") as target:
        for chunk in source.iter_content(1024 * 1024):
            target.write(chunk)
```

## 8. 浏览器和 CORS

浏览器从预签名 URL 直传或下载时，请管理员在每个上游 R2 Bucket 配置相同的 CORS 策略。单文件上传至少允许 `PUT`、签名响应返回的 Header，并暴露 Multipart 所需的 `ETag`：

```json
[
  {
    "AllowedOrigins": ["https://app.example.com"],
    "AllowedMethods": ["GET", "HEAD", "PUT"],
    "AllowedHeaders": ["Content-Type", "x-amz-meta-upload-id"],
    "ExposeHeaders": ["ETag"],
    "MaxAgeSeconds": 3600
  }
]
```

第三方网页不应直接持有长期 S3 Center Token。推荐由第三方自己的后端调用控制 API，再把短时上游请求转交给已登录的浏览器。因为对象可能分布在多个 Account/Bucket，CORS 必须在所有可选上游上保持一致。

## 9. 错误、重试和安全约定

错误统一为：

```json
{"error":{"code":"object_not_found","message":"..."}}
```

| HTTP | 常见 `code` | 处理建议 |
|---|---|---|
| 400 | `invalid_request` | 修正字段、Part 清单或过期时间 |
| 401 | `unauthorized` | Token 缺失、错误或无所需 scope |
| 403 | `resource_forbidden` | Bucket/对象前缀不在授权范围 |
| 404 | `object_not_found` / `session_not_found` | 逻辑对象或会话不存在 |
| 409 | `session_conflict` | 大小不符、状态冲突；查询会话后决定重试或重建 |
| 410 | `session_expired` | 使用新的 Idempotency-Key 创建上传 |
| 507 | `insufficient_capacity` | 当前所有 R2/S3 额度组均不可写，稍后重试或联系管理员扩容 |

- 控制 API 的 5xx、网络断开可以指数退避重试；创建上传必须复用同一个 `Idempotency-Key`。
- 上游预签名请求失败时，不要把带签名的完整 URL 写入日志；它在过期前等同临时凭据。
- 不要向上游请求附加未在响应中要求的 `x-amz-*` Header。
- 逻辑 Key 不应包含 `..`，也不要以 `/` 开头。
- 管理员应为每个第三方签发独立 Token，并限制 `scopes`、`buckets` 和 `prefixes`，以便独立轮换与吊销。

## 10. 不支持的标准 S3 行为

索引模式不提供固定 S3 Endpoint，因此 AWS CLI、rclone、boto3、AWS SDK `S3Client` 等不能仅靠填写 Endpoint 直接使用。第三方必须实现本文的两阶段控制流程，或使用后续提供的适配器。

当前控制 API 不实现 Bucket 创建/删除、Bucket Policy、ACL、Versioning、对象复制、永久公共 URL，也不向调用方暴露上游 R2 凭据。若业务必须使用完全不改造的标准 S3 客户端，只能启用 S3 Center 数据代理模式；该模式的数据流量会经过 S3 Center，与本部署目标不同。
