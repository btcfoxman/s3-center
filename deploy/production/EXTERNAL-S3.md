# 外部系统接入 S3 Center

本文对应当前生产部署。S3 Center 对外提供标准 AWS Signature V4（SigV4）兼容接口，外部系统不需要理解底层 Cloudflare R2，也不应获得 R2 的任何密钥。

## 可直接填写的连接参数

| 配置项 | 生产值 | 说明 |
|---|---|---|
| Endpoint | `https://s3-api.aiid.qzz.io` | 不要填写管理端域名，也不要追加 `/ui` |
| Bucket | `assets` | Bucket 由服务端预先配置，客户端不能自行创建 |
| Region | `us-east-1` | 本系统接受任意 SigV4 Region；统一使用此值便于管理 |
| Access Key ID | 由管理员分配 | 对应 `buckets[].credentials[].access_key_id` |
| Secret Access Key | 由管理员分配 | 对应 `buckets[].credentials[].secret_access_key` |
| Signature | `AWS v4` / `SigV4` | 必须使用 v4 签名 |
| Addressing style | `path` / `force path style = true` | 必须；对象 URL 形如 `/assets/path/file` |
| TLS / SSL | 开启 | 仅使用 HTTPS |

> 客户端凭据和 R2 后端凭据是两套完全不同的密钥。外部系统只能使用 `S3_ACCESS_KEY_ID` / `S3_SECRET_ACCESS_KEY` 这一类逻辑桶凭据，绝不能使用 `R2_ACCESS_KEY_ID` / `R2_SECRET_ACCESS_KEY`。

## 最小连通性测试

先创建独立 AWS CLI profile：

```bash
aws configure --profile s3-center
# AWS Access Key ID: <管理员分配的 Access Key>
# AWS Secret Access Key: <管理员分配的 Secret Key>
# Default region name: us-east-1
# Default output format: json

aws configure set s3.addressing_style path --profile s3-center
```

然后测试列举、上传、下载和删除：

```bash
endpoint=https://s3-api.aiid.qzz.io

aws --profile s3-center --endpoint-url "$endpoint" s3 ls s3://assets/
aws --profile s3-center --endpoint-url "$endpoint" s3 cp ./health.txt s3://assets/acceptance/health.txt
aws --profile s3-center --endpoint-url "$endpoint" s3 cp s3://assets/acceptance/health.txt ./health.downloaded.txt
aws --profile s3-center --endpoint-url "$endpoint" s3 rm s3://assets/acceptance/health.txt
```

## 常用 SDK 配置

### Python / boto3

```python
import boto3
from botocore.config import Config

s3 = boto3.client(
    "s3",
    endpoint_url="https://s3-api.aiid.qzz.io",
    region_name="us-east-1",
    aws_access_key_id="<ACCESS_KEY_ID>",
    aws_secret_access_key="<SECRET_ACCESS_KEY>",
    config=Config(
        signature_version="s3v4",
        s3={"addressing_style": "path"},
    ),
)

s3.put_object(Bucket="assets", Key="app/data.json", Body=b"{}")
```

### Node.js / AWS SDK v3

```javascript
import { S3Client, PutObjectCommand } from "@aws-sdk/client-s3";

const s3 = new S3Client({
  endpoint: "https://s3-api.aiid.qzz.io",
  region: "us-east-1",
  forcePathStyle: true,
  credentials: {
    accessKeyId: process.env.S3_CENTER_ACCESS_KEY_ID,
    secretAccessKey: process.env.S3_CENTER_SECRET_ACCESS_KEY,
  },
});

await s3.send(new PutObjectCommand({
  Bucket: "assets",
  Key: "app/data.json",
  Body: Buffer.from("{}"),
  ContentType: "application/json",
}));
```

### Go / AWS SDK v2

```go
client := s3.New(s3.Options{
    BaseEndpoint: aws.String("https://s3-api.aiid.qzz.io"),
    Region:       "us-east-1",
    Credentials:  credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
    UsePathStyle: true,
})
```

### Spring Boot / AWS SDK

不同 S3 starter 的字段名略有差异，但最终必须得到以下设置：

```yaml
s3:
  endpoint: https://s3-api.aiid.qzz.io
  bucket: assets
  region: us-east-1
  access-key: ${S3_CENTER_ACCESS_KEY_ID}
  secret-key: ${S3_CENTER_SECRET_ACCESS_KEY}
  path-style-access-enabled: true
```

若直接构造 AWS SDK `S3Client`，需同时配置 `endpointOverride(...)` 和 `S3Configuration.pathStyleAccessEnabled(true)`。

### rclone

```ini
[s3-center]
type = s3
provider = Other
access_key_id = <ACCESS_KEY_ID>
secret_access_key = <SECRET_ACCESS_KEY>
endpoint = https://s3-api.aiid.qzz.io
region = us-east-1
force_path_style = true
```

## 每个外部系统使用独立凭据

当前生产配置已有一组 `assets` 桶凭据，一个可信系统可以直接使用。多个系统长期接入时，建议一套系统一对密钥，便于单独轮换和吊销。此操作不需要修改程序代码：

1. 在 `deploy/production/config.yaml` 的 `assets.credentials` 下增加一项，使用新的环境变量名。

   ```yaml
   buckets:
     - name: assets
       credentials:
         - access_key_id: "${S3_ACCESS_KEY_ID}"
           secret_access_key: "${S3_SECRET_ACCESS_KEY}"
         - access_key_id: "${S3_ORDER_SERVICE_ACCESS_KEY_ID}"
           secret_access_key: "${S3_ORDER_SERVICE_SECRET_ACCESS_KEY}"
   ```

2. 将两个新变量安全写入服务器部署环境，不要提交真实密钥到 Git。
3. 重新部署，或更新配置后向进程发送 `SIGHUP`。Bucket 凭据支持热加载。
4. 外部系统切换成功后再删除旧凭据；轮换期可让新旧密钥并存。

所有 Access Key ID 在全部虚拟桶之间必须全局唯一。一对凭据只会映射到一个桶。

## 哪些场景需要额外改造

| 业务要求 | 当前是否支持 | 处理方式 |
|---|---|---|
| 标准上传、下载、列举、删除、同桶复制、Multipart | 支持 | 仅配置 Endpoint 与凭据 |
| SDK 生成预签名 URL | 支持 | 无服务端改造，最长 7 天 |
| 每个系统独立吊销密钥 | 支持 | 同一桶增加多组 credentials |
| 系统之间数据完全隔离 | 支持 | 为每个系统配置独立虚拟 Bucket 和凭据 |
| 同一 Bucket 内只读、只写或按前缀授权 | 暂不支持 | 需要新增 credential policy / prefix scope 权限模型 |
| S3 ACL、Bucket Policy、Object Versioning | 暂不支持 | 需要实现相应 S3 API；不能只靠配置解决 |
| 客户端强制 `bucket.endpoint` 虚拟主机风格 | 当前生产域名不支持 | 优先改客户端为 path-style；否则需新增通配 DNS、证书、Tunnel 路由及主机名桶解析 |
| 客户端运行 CreateBucket / DeleteBucket | 不支持 | 关闭客户端自动建桶，使用预配置的 `assets` |
| 单次上传超过当前 256 MiB 配置 | 需评估 | 客户端优先 Multipart；必要时同步提高 `server.max_object_size` 与直传上限 |

## 网络与 Cloudflare

- `s3-api.aiid.qzz.io` 可以公网访问，但每个 S3 请求仍必须通过 SigV4 验证。
- 不要在 S3 API 域名前启用 Cloudflare Access 登录页。浏览器跳转或 Access Cookie 会破坏普通 SDK 的 SigV4 请求流程。
- 如需保护管理页面，可以只对 `s3-center.aiid.qzz.io` 配置 Access；这与 S3 API 域名相互独立，并非系统运行必需项。
- 管理端账号、Admin API Token、直传 Token 和 S3 客户端凭据互不通用。

## 兼容性边界

本系统实现常见对象存储工作流，但不是 AWS S3 全量替代。接入第三方产品前，确认它不会强依赖 Bucket ACL/Policy、版本控制、跨桶复制或自动创建 Bucket。通用 SDK、备份工具和支持“自定义 S3 Endpoint”的业务系统通常可以直接使用。

更完整的通用客户端说明见 [`docs/user-guide.md`](../../docs/user-guide.md)，认证与轮换机制见 [`docs/authentication.md`](../../docs/authentication.md) 和 [`docs/operations.md`](../../docs/operations.md#rotating-client-credentials)。
