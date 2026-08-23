明白了。你的目标不是简单做一个“网盘”，而是构建一套：

> **多 Cloudflare R2 账号统一控制面 + 虚拟存储空间 + 容量/成本调度系统 + 对外存储 API。**

从系统层面看，它类似一个小型的对象存储云管平台。底层有很多独立的 Cloudflare Account、R2 Bucket 和额度，但对内部业务与外部客户只暴露一个统一的存储中心。

不过有一个边界必须先定清楚：

* 可以动态接入已有的、经过授权的 Cloudflare 账号。
* 可以管理自有账号、客户自带账号、免费额度账号和按量付费账号。
* 不应自动注册大量免费账号，也不应设计“账号用满就换号”来规避计费。Cloudflare 当前协议明确禁止通过自动化脚本产生多个账号，也禁止以规避服务额度或限制为目的使用服务；未经书面允许，也不能直接出租、转售 Cloudflare 服务访问权。([Cloudflare][1])

因此，产品最好定位成：

> **多账号 R2 管理与存储编排平台**

而不是：

> **无限叠加 Cloudflare 免费额度平台**

---

# 一、先重新定义“免费账号”和“付费账号”

Cloudflare R2 实际上并不是严格分成“免费版 R2”和“付费版 R2”。

每个启用 R2 的 Cloudflare Account 都要完成 R2 订阅流程，然后获得每月包含的免费用量；超过后按量收费：

| 项目               |       每月包含量 |            超出价格 |
| ---------------- | ----------: | --------------: |
| Standard Storage | 10 GB-month | $0.015/GB-month |
| Class A 操作       |      100 万次 |       $4.50/百万次 |
| Class B 操作       |     1000 万次 |       $0.36/百万次 |
| 公网出站流量           |          免费 |              免费 |

免费额度仅适用于 Standard Storage，不适用于 Infrequent Access。R2 Bucket 本身没有 10 GB 的物理容量上限，官方当前给出的单 Bucket 存储量和对象数都是 Unlimited；10 GB 只是免费计费额度，不是硬限制。也就是说，不做系统侧限制的话，账号超过 10 GB 后不会自动停止上传，而可能直接产生费用。([Cloudflare Docs][2])

因此系统中不要设计：

```text
account_plan = free / paid
```

而应设计：

```text
billing_policy =
  FREE_CAPPED
  PAYG_BUDGETED
  PAYG_UNCAPPED
  CUSTOMER_BYOC
  BACKUP_ONLY
```

含义分别是：

| 策略            | 含义                              |
| ------------- | ------------------------------- |
| FREE_CAPPED   | 系统主动控制在免费额度以内                   |
| PAYG_BUDGETED | 可以产生费用，但有月度预算                   |
| PAYG_UNCAPPED | 可作为稳定主存储或溢出节点                   |
| CUSTOMER_BYOC | 客户自带 Cloudflare Account，费用由客户承担 |
| BACKUP_ONLY   | 只用于副本，不接收普通新文件                  |

所谓“免费账号”，实际上就是被你的系统设置为 `FREE_CAPPED` 的 R2 账号。

---

# 二、整体架构：控制面和数据面分开

推荐结构如下：

```text
                           外部客户 / 内部业务
                    Web / SDK / REST / 临时 S3 凭证
                                  │
                     ┌────────────▼────────────┐
                     │ 统一 API Gateway / Auth │
                     │ api.storage.example.com │
                     └────────────┬────────────┘
                                  │
              ┌───────────────────▼───────────────────┐
              │             Storage Control Plane     │
              │                                       │
              │  账号管理         Bucket 管理          │
              │  凭证管理         容量与成本计算        │
              │  存储节点调度     文件位置目录          │
              │  上传下载授权     迁移与副本管理        │
              │  租户配额         告警与审计            │
              └───────────┬───────────────────────────┘
                          │
              ┌───────────▼────────────┐
              │ PostgreSQL / Supabase  │
              │ Object Catalog         │
              │ Usage / Billing / Jobs │
              └───────────┬────────────┘
                          │
                    Queue / Job Runner
                          │
       ┌──────────────────┼────────────────────┐
       │                  │                    │
┌──────▼───────┐  ┌───────▼──────┐    ┌────────▼──────┐
│ CF Account A │  │ CF Account B │    │ CF Account C  │
│ R2 Bucket A  │  │ R2 Bucket B  │    │ R2 Bucket C   │
│ FREE_CAPPED  │  │ PAYG_BUDGETED│    │ CUSTOMER_BYOC │
└──────────────┘  └──────────────┘    └───────────────┘
```

最重要的原则是：

> 控制请求经过统一系统，图片、视频、音频、压缩包等文件本体尽量直接在客户端与目标 R2 Account 之间传输。

不要把所有视频字节都转发经过你的 Vercel Function、Worker 或主服务器。否则你的“统一控制面”会变成整个系统的带宽瓶颈和单点故障。

---

# 三、核心抽象：虚拟 Bucket 与物理节点

外部用户看到的是：

```text
租户
  └── 虚拟 Bucket
       └── 逻辑对象
```

底层实际是：

```text
Cloudflare Account
  └── R2 Bucket
       └── object key
```

关系可以设计为：

```text
Tenant
  └── VirtualBucket
       └── StoragePool
            ├── StorageNode A
            ├── StorageNode B
            └── StorageNode C
```

其中：

## StorageNode

一个存储节点不是单纯的 Cloudflare Account，而是：

```text
Cloudflare Account
+
R2 Bucket
+
Jurisdiction
+
访问类型
+
计费策略
```

例如：

```text
node-free-01
├── account_id: xxx
├── bucket: storage-private
├── jurisdiction: default
├── access_mode: private
├── billing_policy: FREE_CAPPED
└── hard_limit: 8 GB
```

同一个 Cloudflare Account 可以有多个节点：

```text
Account A
├── public-standard
├── private-standard
├── temp-standard
└── backup-standard
```

## StoragePool

节点按用途组合成 Pool：

```text
pool-public-hot
pool-private
pool-temp
pool-backup
pool-customer-byoc
pool-paid-overflow
```

外部客户只选择：

```text
public
private
temporary
backup
```

不需要知道文件被放到了哪个 Cloudflare Account。

---

# 四、动态账号扩充应该怎么做

“动态扩充”应理解为：

> 动态接入、验证并启用已有 Cloudflare Account。

而不是自动注册新账号。

## 方案一：Cloudflare OAuth 接入

Cloudflare 在 2026 年已经开放自管理第三方 OAuth 客户端，Free、Pro、Business 和 Enterprise 都可使用。用户授权时可以选择允许你的应用访问哪些 Cloudflare Account，以及授予哪些权限。([Cloudflare Docs][3])

接入流程可以是：

```text
用户点击“添加 Cloudflare 账号”
        ↓
跳转 Cloudflare OAuth
        ↓
用户选择一个或多个 Account
        ↓
用户确认权限
        ↓
系统获取 OAuth Token
        ↓
读取 Account、R2 Bucket、Metrics
        ↓
选择或创建 Bucket
        ↓
配置 CORS、Lifecycle、事件通知
        ↓
验证上传、下载、删除
        ↓
节点进入 ACTIVE
```

OAuth 权限建议分为必选和可选。

必选读权限：

```text
Account Settings Read
Workers R2 Storage Read
Account Analytics Read
```

可选写权限：

```text
Workers R2 Storage Write
Billing Read
API Tokens Write
```

其中：

* 只做可视化监控：只申请 Read。
* 需要自动创建 Bucket、配置 CORS 和 Lifecycle：申请 R2 Write。
* 需要自动生成 R2 数据访问 Token：还需要 Token 管理相关权限。
* 需要显示真实账单和费用：申请 Billing Read。

OAuth 是控制面凭证，不一定直接适合作为 S3 数据面凭证。

## 方案二：单独接入 R2 Bucket Token

数据面建议每个节点单独创建一个 Bucket 范围的 R2 Token：

```text
Object Read & Write
Scope: specific bucket only
```

不要使用 Global API Key，也不要为所有账号共用一个大权限 Token。

Cloudflare 当前支持：

* Account API Token：与账号绑定，更适合长期服务集成。
* User API Token：与具体用户绑定，用户被移出账号后可能失效。
* Bucket-scoped Token：只允许访问指定 Bucket。
* 临时 S3 Credentials：从父 Token 派生，支持 Bucket、前缀、对象和 TTL 限制。([Cloudflare Docs][4])

稳定的自有节点建议使用：

```text
Account-owned R2 Token
+
Specific Bucket
+
Object Read & Write
```

客户自带账号可以使用：

```text
OAuth 控制凭证
+
客户生成的 Bucket-scoped R2 Token
```

## 账号接入状态机

```text
DISCOVERED
    ↓
AUTHORIZED
    ↓
PROVISIONING
    ↓
VERIFYING
    ↓
ACTIVE
    ↓
DRAINING
    ↓
DISCONNECTED
```

异常状态：

```text
AUTH_EXPIRED
BILLING_FAILED
QUARANTINED
OFFLINE
```

删除账号时不能直接断开，应先进入：

```text
DRAINING
```

此时：

* 不再向该账号分配新对象。
* 保持旧对象可读。
* 后台迁移对象。
* 确认迁移完成后再撤销凭证。

---

# 五、容量管理不能只看“当前用了多少 GB”

R2 使用的是 `GB-month`，其计算方式是计费周期内每日峰值存储量的平均值。比如月初存 30 GB、月末再删除，并不等于完全没有产生使用量。([Cloudflare Docs][5])

因此每个账号至少要计算四个容量指标：

```text
当前物理存储量
本周期已消耗 GB-month
预计周期结束 GB-month
下一完整周期稳定存储量
```

计算模型可以是：

```text
daily_peak_bytes = 当天最大存储量

used_gb_month =
  所有已完成日期的 daily_peak_gb 之和
  ÷ 计费周期天数
```

预计值：

```text
projected_gb_month =
  (
    已完成日期 daily_peak_gb 之和
    +
    当前存储 GB × 剩余天数
  )
  ÷ 计费周期天数
```

新对象的边际消耗：

```text
marginal_gb_month =
  object_size_gb
  × 预计保留天数
  ÷ 计费周期天数
```

例如一个 6 GB 文件，只保留 5 天：

```text
6 × 5 ÷ 30 = 1 GB-month
```

但如果它会跨到下一个完整计费周期，下一周期就是完整的 6 GB-month。

因此调度引擎需要知道文件的：

```text
size
retention_days
expires_at
storage_class
access_frequency
importance
```

---

# 六、系统如何获取真实用量

不能只依赖自己数据库中的对象大小，因为可能存在：

* Cloudflare 后台手工上传。
* 外部 S3 客户端上传。
* 上传完成但系统回调失败。
* 系统删除成功但数据库更新失败。
* Multipart 未完成。
* 账号中存在历史文件。

推荐使用三层数据源。

## 第一层：内部实时账本

每次操作先记录：

```text
upload_reserved
upload_completed
object_deleted
download_url_issued
multipart_created
multipart_completed
```

内部账本用于实时调度，不等 Cloudflare 指标更新。

## 第二层：Cloudflare Metrics

R2 当前提供：

```text
GET /accounts/{account_id}/r2/metrics
```

以及 GraphQL 数据集：

```text
r2OperationsAdaptiveGroups
r2StorageAdaptiveGroups
```

可以查询：

* Bucket 存储量。
* 对象数量。
* Multipart Upload 数量。
* 操作类型。
* 请求数量。
* 成功和失败状态。

GraphQL 指标只保留最近 31 天，因此系统必须按小时或按天同步到自己的数据库，不能把 Cloudflare GraphQL 当长期历史数据库。([Cloudflare Docs][6])

## 第三层：账单用量

Cloudflare 提供账户级 Billable Usage API，不过部分版本仍处于 Alpha、Restricted 或迁移阶段。因此适合用于账单核对，不适合成为调度系统唯一的数据源。([Cloudflare Docs][7])

建议每个指标保存三个状态：

```text
estimated_value
observed_value
billable_confirmed_value
```

例如：

```json
{
  "storageBytes": {
    "estimated": 8589934592,
    "observed": 8591020032,
    "confirmed": null
  }
}
```

## Event Notifications

每个 R2 Bucket 可以配置对象创建、覆盖和删除事件通知，并发送到 Cloudflare Queue；Queue 可以由 Worker 消费，也可以通过 HTTP Pull 从外部系统读取。([Cloudflare Docs][8])

推荐每个账号配置：

```text
R2 Event Notification
        ↓
Account-local Queue
        ↓
HTTP Pull / Node Worker
        ↓
统一控制面
```

这样即使用户绕过平台直接通过 S3 上传，控制面也能收到对象变化事件。

---

# 七、免费额度调度规则

免费节点不要直接配置为 10 GB 硬上限。

建议：

```text
Cloudflare 官方免费额度：10 GB-month
系统稳定容量上限：8 GB～8.5 GB
紧急硬上限：9 GB
```

保留空间用于：

* 指标延迟。
* Metadata。
* 未完成上传。
* 重试和临时副本。
* 外部直接操作。
* 计费取整。

Class A 和 Class B 也不要用满：

```text
Class A 运行上限：80 万次
Class B 运行上限：800 万次
```

Cloudflare 对计费单位会向上取整，比如 100 万零 1 次操作可能按 200 万次计费；因此临界点附近要留出较大缓冲。([Cloudflare Docs][5])

## 节点调度顺序

一次上传的节点选择流程：

```text
1. 筛选 ACTIVE 节点
2. 筛选符合 Jurisdiction 的节点
3. 筛选符合 public/private 类型的节点
4. 筛选允许该 Tenant 使用的节点
5. 检查当前存储容量
6. 检查预计 GB-month
7. 检查 Class A 剩余额度
8. 检查月度成本预算
9. 检查 Token、域名和健康状态
10. 根据得分选择节点
```

简单评分公式：

```text
score =
  免费容量剩余权重
  + 操作额度剩余权重
  + 健康度
  + 稳定性
  - 预计成本
  - 账号风险
  - 容量碎片
```

实际调度建议采用：

```text
Best Fit
```

而不是轮询。

例如节点剩余容量：

```text
A：1 GB
B：4 GB
C：7 GB
```

上传一个 3 GB 文件时应放入 B，而不是轮询到 C。这样可以减少空间碎片。

---

# 八、免费节点和付费节点如何分工

建议不要把所有文件一视同仁。

| 文件类型       | 推荐节点          |
| ---------- | ------------- |
| 临时 AI 生成结果 | FREE_CAPPED   |
| 可重新生成的缩略图  | FREE_CAPPED   |
| 缓存文件       | FREE_CAPPED   |
| 用户正式交付文件   | PAYG_BUDGETED |
| 原始素材       | PAYG 或双副本     |
| 公开热门文件     | 稳定 PAYG 节点    |
| 合同、发票、财务附件 | 至少双副本         |
| 客户自有数据     | CUSTOMER_BYOC |
| 平台全局备份     | BACKUP_ONLY   |

免费账号适合：

```text
低价值
可恢复
有 TTL
可重新生成
存在副本
```

不适合：

```text
唯一原始文件
客户唯一交付件
合同与财务附件
无法重新生成的数据
```

---

# 九、对象不能跨账号分片

不要采用：

```text
video.part1 -> Account A
video.part2 -> Account B
video.part3 -> Account C
```

应该是：

```text
完整对象 Primary -> Account A
完整对象 Replica -> Account B
```

一个 Multipart Upload 必须在创建时绑定到一个 StorageNode，在完成前不能改变节点。

数据库应保存：

```text
object_id
primary_replica_id
```

物理位置放在单独的 replicas 表中：

```text
object
├── replica A: Account A / Bucket A / key
└── replica B: Account B / Bucket B / key
```

迁移流程：

```text
1. 在目标节点写入完整对象
2. 校验大小
3. 校验 SHA-256
4. 创建新 replica
5. 原子切换 primary_replica_id
6. 旧副本保留一段时间
7. 删除旧副本
```

不要只依赖 ETag 做跨账号内容一致性判断，系统自己记录 SHA-256。

---

# 十、对外如何支持

建议提供三种模式，而不是只做一种。

# 模式一：统一 REST API

这是最推荐的主模式。

外部用户看到：

```text
api.storage.example.com
```

常用接口：

```http
POST   /v1/uploads/init
POST   /v1/uploads/{uploadId}/complete
GET    /v1/objects/{objectId}
POST   /v1/objects/{objectId}/download-url
DELETE /v1/objects/{objectId}
GET    /v1/objects
GET    /v1/usage
```

上传初始化：

```json
{
  "bucket": "user-media",
  "filename": "demo.mp4",
  "contentType": "video/mp4",
  "size": 18874368,
  "access": "private",
  "retentionDays": 30
}
```

系统返回：

```json
{
  "uploadId": "upl_01KXYZ",
  "objectId": "obj_01KXYZ",
  "method": "PUT",
  "uploadUrl": "https://<account>.r2.cloudflarestorage.com/...",
  "expiresAt": "2026-08-23T16:30:00Z"
}
```

客户端直接上传 R2。

完成后：

```http
POST /v1/uploads/upl_01KXYZ/complete
```

系统执行：

```text
HEAD Object
检查大小
检查 MIME
检查 checksum
更新用量
将对象改成 ACTIVE
```

Cloudflare Presigned URL 支持临时授权单个 GET、PUT、HEAD 或 DELETE 操作，有效期最长 7 天。([Cloudflare Docs][9])

## 配额预占

初始化上传时：

```text
reserved_bytes += declared_size
```

上传成功：

```text
reserved_bytes -= declared_size
used_bytes += actual_size
```

上传超时：

```text
释放 reserved_bytes
```

如果实际文件大于申报值：

```text
删除对象
标记 FAILED
```

因为 R2 并不会自动替你的系统执行租户容量限制。

---

# 模式二：临时 S3 Session

给需要使用：

* AWS SDK
* rclone
* Cyberduck
* S3 CLI
* 后端程序

的客户下发短期临时凭证：

```json
{
  "endpoint": "https://ACCOUNT_ID.r2.cloudflarestorage.com",
  "bucket": "storage-private",
  "accessKeyId": "...",
  "secretAccessKey": "...",
  "sessionToken": "...",
  "region": "auto",
  "prefix": "tenant/t_123/",
  "expiresAt": "2026-08-23T18:00:00Z"
}
```

Cloudflare R2 临时凭证可以限制：

* Bucket。
* 权限。
* Prefix。
* 具体对象。
* 有效期。

最大 TTL 当前为 604800 秒，即 7 天。([Cloudflare Docs][10])

但有一个重要约束：

> 一个临时 S3 Session 必须固定到一个 Cloudflare Account Endpoint。

不能给客户端一个 S3 Session，然后让同一个 Session 的对象动态分散到多个 Account。

因此适合：

```text
创建 Session 时选择节点
Session 生命周期内固定节点
下一个 Session 可以选择另一节点
```

## 临时 S3 凭证的容量限制问题

临时凭证可以限制路径和权限，但不能直接表达：

```text
最多只能写 2 GB
```

所以严格的 FREE_CAPPED 节点不宜给客户长期、宽范围 S3 写权限。

推荐：

* 普通 Web 用户：使用逐对象 Presigned URL。
* 可信后端客户：使用短期 S3 Session。
* 免费节点：避免长期 S3 凭证。
* 付费/BYOC 节点：可以提供更完整 S3 Session。

---

# 模式三：真正统一的 S3 Gateway

外部客户只配置：

```text
endpoint = https://s3.storage.example.com
```

然后像普通 S3 一样操作虚拟 Bucket。

这需要你的系统自己实现：

* AWS Signature V4 验证。
* ListObjects。
* Multipart Upload。
* Range。
* HEAD。
* CopyObject。
* DeleteObjects。
* Virtual-hosted/path-style。
* 错误码兼容。
* XML 响应。
* AccessKey/Secret 管理。

最大的麻烦是：

> 客户对 `s3.storage.example.com` 完成 SigV4 签名后，系统很难把请求透明重定向到另一个 Cloudflare Account Endpoint。

如果网关代理文件本体，则所有上传和下载字节都经过你的网关。

所以统一 S3 Gateway 有两种落法：

### 落法 A：虚拟 Bucket 固定物理账号

```text
virtual-bucket-a -> Account A
virtual-bucket-b -> Account B
```

创建 Bucket 时调度一次，以后该 Bucket 固定节点。

这是兼容性最好的方案。

### 落法 B：对象级动态调度

```text
同一个虚拟 Bucket 中的对象分布到多个账号
```

此时网关必须维护统一 Object Catalog，并代理或重写大量 S3 请求，实现复杂度高得多。

因此首版建议：

```text
REST API：对象级动态调度
S3 Gateway：虚拟 Bucket 级固定调度
```

这是兼顾扩展性和兼容性的合理做法。

真正的统一 S3 Gateway 建议部署在：

```text
Go/Rust 服务
+
独立 VPS / Kubernetes
+
流式代理
```

不要首选 Vercel Function 或普通 Worker 来代理大文件。

---

# 十一、公开文件和私有文件的统一 URL

## 私有文件

访问：

```text
GET https://api.storage.example.com/v1/objects/obj_123/download-url
```

返回短期 Presigned URL。

需要注意，R2 Presigned URL 只能使用：

```text
ACCOUNT_ID.r2.cloudflarestorage.com
```

不能使用 R2 Custom Domain。([Cloudflare Docs][9])

## 公开文件

对外使用稳定逻辑 URL：

```text
https://files.example.com/o/obj_123
```

控制面查询对象位置后返回：

```http
302 Location: https://node03.example-node.com/path/hash.mp4
```

这样对象从 Account A 迁移到 Account B 后：

```text
files.example.com/o/obj_123
```

仍然不变。

每个账号的 R2 Bucket 可以绑定自己的节点域名：

```text
node01.storage-a.com
node02.storage-b.com
node03.storage-c.com
```

不要用 `r2.dev` 承载正式外部服务。Cloudflare 明确说明 `r2.dev` 是开发用途，存在可变限速；生产使用应绑定 Custom Domain。([Cloudflare Docs][11])

---

# 十二、核心数据库结构

建议至少有以下表。

## cloudflare_connections

一条 OAuth 或 API Token 连接记录：

```text
id
owner_type
owner_id
auth_type
oauth_token_ref
refresh_token_ref
scopes
status
expires_at
last_verified_at
```

注意只保存：

```text
vault_ref
```

不要在普通数据库列中明文存放 Token。

## cloudflare_accounts

```text
id
connection_id
cloudflare_account_id
account_name
billing_policy
monthly_budget_usd
operational_storage_limit
operational_class_a_limit
operational_class_b_limit
jurisdiction
status
risk_level
last_sync_at
```

## r2_buckets

```text
id
account_id
bucket_name
jurisdiction
storage_class
access_mode
custom_domain
cors_status
lifecycle_status
event_notification_status
status
```

唯一约束：

```text
UNIQUE(account_id, jurisdiction, bucket_name)
```

## storage_nodes

```text
id
bucket_id
pool_id
credential_ref
priority
weight
status
current_bytes
reserved_bytes
projected_gb_month
class_a_used
class_b_used
estimated_cost
last_health_check_at
```

## storage_pools

```text
id
name
type
placement_policy
replica_count
allow_free_nodes
allow_paid_nodes
```

## virtual_buckets

```text
id
tenant_id
name
pool_id
access_mode
quota_bytes
used_bytes
reserved_bytes
status
```

## objects

```text
id
tenant_id
virtual_bucket_id
original_filename
mime_type
size_bytes
sha256
access_mode
retention_until
status
primary_replica_id
created_at
deleted_at
```

## object_replicas

```text
id
object_id
node_id
physical_bucket
object_key
size_bytes
sha256
etag
is_primary
status
last_verified_at
```

## usage_snapshots

```text
id
account_id
bucket_id
storage_bytes
object_count
class_a_operations
class_b_operations
estimated_gb_month
estimated_cost
source
recorded_at
```

`source`：

```text
INTERNAL
GRAPHQL
R2_METRICS
BILLING_API
INVOICE
```

## placement_reservations

防止并发上传超额：

```text
id
tenant_id
node_id
upload_id
reserved_bytes
expires_at
status
```

## migration_jobs

```text
id
object_id
source_node_id
target_node_id
status
attempts
bytes_copied
last_error
created_at
completed_at
```

---

# 十三、管理后台应该展示什么

## 全局总览

```text
接入账号数量
ACTIVE 节点数量
当前总存储量
运行容量上限
预计周期末 GB-month
预计 Cloudflare 费用
免费额度利用率
付费额度利用率
Class A 剩余
Class B 剩余
异常账号数量
迁移中对象数量
```

不要只显示：

```text
总容量：100 GB
已使用：60 GB
```

应该区分：

```text
物理当前容量
本周期计费用量
运行可分配容量
付费可扩展容量
副本占用
租户逻辑容量
```

例如：

```text
当前物理数据：62 GB
副本数据：14 GB
逻辑用户数据：48 GB
FREE_CAPPED 可分配：11 GB
PAYG 可分配：无限
本周期预计费用：$1.28
```

## 账号矩阵

| 账号       | 策略            |   当前存储 | 预计 GB-month | A 操作 | B 操作 |  预计费用 | 状态       |
| -------- | ------------- | -----: | ----------: | ---: | ---: | ----: | -------- |
| CF-01    | FREE_CAPPED   | 7.2 GB |         7.5 |  21% |  13% |    $0 | ACTIVE   |
| CF-02    | FREE_CAPPED   | 8.4 GB |         8.8 |  32% |  18% |    $0 | DRAINING |
| CF-03    | PAYG_BUDGETED |  82 GB |        79.2 |   4% |   7% | $1.04 | ACTIVE   |
| Client-A | BYOC          | 310 GB |         305 |  12% |  24% |  客户承担 | ACTIVE   |

## 节点告警

```text
存储达到 70%
存储达到 85%
预计 GB-month 超额
Class A 达到 80%
Class B 达到 80%
Token 即将过期
OAuth 被撤销
Billing API 不可用
R2 Metrics 超过 1 小时未同步
Bucket CORS 漂移
Lifecycle 配置漂移
事件通知中断
节点健康检查失败
```

---

# 十四、对外租户容量与费用怎么计算

外部客户不应该看到：

```text
你的文件在 CF Account 03
```

客户只看到：

```text
套餐容量
已使用容量
请求次数
对象数量
下载量
到期时间
```

租户配额分为：

```text
storage_quota_bytes
object_quota
monthly_upload_operations
monthly_download_operations
max_object_size
max_presigned_ttl
retention_days
```

客户使用 100 GB，不代表底层正好占用 100 GB，因为还包括：

* 副本。
* 临时上传。
* Multipart。
* 删除延迟。
* Metadata。
* 迁移临时双份。
* 版本或回收站。

因此建议定义：

```text
Logical Usage
Physical Usage
Billable Usage
```

客户计费通常基于 Logical Usage：

```text
用户有效对象大小
```

平台成本基于 Physical Usage：

```text
所有节点对象
+
副本
+
临时对象
```

两者不能混在一起。

---

# 十五、商业对外服务的边界

对外支持有三种商业模式。

## 1. 平台托管存储

客户使用你的：

```text
API
SDK
对象管理
权限
分享
生命周期
```

R2 只是你的底层基础设施。

这比直接卖“Cloudflare R2 额度”更合理，但仍应仔细确认服务协议和内容合规义务。

## 2. BYOC / Bring Your Own Cloudflare

客户连接自己的 Cloudflare Account：

```text
客户承担 Cloudflare 费用
你提供统一管理、API、可视化、调度和权限
```

这是风险最低、长期最适合 SaaS 化的模式。

你可以收费：

```text
管理费
账号数
Bucket 数
用户席位
自动迁移
监控告警
API 请求
高级权限
备份策略
```

而不是转售底层存储。

## 3. 真正转售存储

例如你直接出售：

```text
100 GB R2 存储套餐
1 TB R2 存储套餐
```

并向第三方提供类似原生 S3 的服务。

Cloudflare Self-Serve 协议当前写明，除非获得 Cloudflare 书面允许，用户无权出租、租赁、出借或销售服务访问权。准备正式商业化为通用对象存储服务时，应考虑 Cloudflare Partner、MSSP、Distributor 或 Enterprise 相关协议，而不是只依赖普通 Self-Serve 账号。([Cloudflare][1])

---

# 十六、最推荐的首版技术栈

结合你的使用场景，建议：

```text
前端：
Next.js
部署到 Vercel 或 Cloudflare Pages

统一 API：
TypeScript/NestJS 或 Go
部署到自有 Docker 服务器

边缘入口：
Cloudflare Worker
只负责鉴权、限流、302 和轻量签名接口

元数据库：
PostgreSQL / Supabase Postgres

任务队列：
Cloudflare Queues
或 Redis + BullMQ

后台任务：
Docker Job Worker

对象存储：
多个 Cloudflare R2 Account

凭证：
数据库只保存加密后的 credential_ref
主密钥放服务端 Secret
```

为什么主控制面不完全放 Worker：

* 多账号指标同步属于后台任务。
* 文件迁移属于长任务。
* 需要处理重试、队列、批量核对。
* 可能需要使用 rclone 或 S3 Streaming。
* Worker 更适合作为边缘入口，而不是全功能云管后台。

---

# 十七、合理的开发阶段

## 第一阶段：内部可用 MVP

接入 2～5 个账号：

```text
手动添加账号
Bucket-scoped Token
账号和 Bucket 可视化
当前容量同步
内部对象目录
Presigned PUT/GET
FREE_CAPPED / PAYG 两种策略
基本 Best-Fit 调度
节点启停
对象迁移
```

第一阶段不要做统一 S3 Gateway。

## 第二阶段：自动化控制面

增加：

```text
Cloudflare OAuth
自动发现账号
自动发现 Bucket
GraphQL 用量同步
Billing 用量核对
事件通知
账号健康检查
容量预测
自动 DRAINING
自动迁移
副本管理
租户配额
```

## 第三阶段：正式对外

增加：

```text
多租户
平台 AccessKey
REST SDK
短期 S3 Session
Webhook
审计日志
套餐和账单
BYOC
公开 URL
自定义域名
内容风控
滥用处理
```

## 第四阶段：S3 兼容层

只在确实存在客户需求时建设：

```text
统一 S3 Gateway
SigV4
ListObjects
Multipart
Range
虚拟 Bucket
固定 Bucket 到节点映射
```

---

# 最终推荐形态

这套系统最终应形成四层：

```text
第一层：Cloudflare Account 资源层
管理账号、Bucket、Token、额度、账单、健康状态

第二层：Storage Pool 编排层
把多个账号组合成免费池、付费池、备份池、BYOC 池

第三层：Virtual Storage 逻辑层
租户、虚拟 Bucket、逻辑对象、配额、权限、生命周期

第四层：External Access 接口层
REST API、Presigned URL、临时 S3 凭证、SDK、Webhook
```

其中最关键的三个技术决策是：

1. **对象目录必须独立于 Cloudflare，不能靠跨账号 ListObjects 拼出来。**
2. **文件本体尽量直传 R2，控制面只负责选节点和签发权限。**
3. **免费容量是调度策略，不是 Cloudflare 提供的硬容量限制。**

首版最合理的产品边界是：

```text
多个已有 CF Account
+
统一可视化
+
账号/Bucket/凭证管理
+
实时与账单用量核对
+
REST 直传
+
对象级动态调度
+
付费节点自动溢出
+
BYOC 扩展能力
```

这已经能覆盖你说的统一额度管控、账号数据管理、动态账号扩充和对外支持，而且不会一上来掉进“全量 S3 网关”和“跨账号文件代理”这两个大坑。

[1]: https://www.cloudflare.com/terms/ "Self-Serve Subscription Agreement | Cloudflare"
[2]: https://developers.cloudflare.com/r2/get-started/?utm_source=chatgpt.com "Get started · Cloudflare R2 docs"
[3]: https://developers.cloudflare.com/fundamentals/oauth/create-an-oauth-client/?utm_source=chatgpt.com "Create your OAuth client - Cloudflare Fundamentals"
[4]: https://developers.cloudflare.com/r2/api/tokens/ "Authentication · Cloudflare R2 docs"
[5]: https://developers.cloudflare.com/r2/pricing/ "Pricing · Cloudflare R2 docs"
[6]: https://developers.cloudflare.com/api/resources/r2/ "R2 | Cloudflare API"
[7]: https://developers.cloudflare.com/api/python/resources/billing/?utm_source=chatgpt.com "Billing | Cloudflare API"
[8]: https://developers.cloudflare.com/r2/buckets/event-notifications/?utm_source=chatgpt.com "Event notifications - R2 - Cloudflare Docs"
[9]: https://developers.cloudflare.com/r2/api/s3/presigned-urls/?utm_source=chatgpt.com "Presigned URLs · Cloudflare R2 docs"
[10]: https://developers.cloudflare.com/r2/api/s3/temporary-credentials/?utm_source=chatgpt.com "Temporary credentials · Cloudflare R2 docs"
[11]: https://developers.cloudflare.com/r2/platform/limits/?utm_source=chatgpt.com "Limits · Cloudflare R2 docs"
