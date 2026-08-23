# 结论

可以基于 `s3-orchestrator` 二开，但**不要把原有 S3 Proxy 改没**。最合理的形态是：

> **一个统一元数据与调度核心，两条并存的数据通道。**

```text
通道 A：原有 S3 Gateway
适合 AWS SDK、rclone、Cyberduck、传统 S3 客户端

通道 B：新增 Direct Upload REST API
适合浏览器、App、业务系统、AI 图片/视频上传
```

两条通道共用：

* Cloudflare 账号与 Bucket 管理
* 对象目录
* 节点调度
* 容量预占
* 免费/付费额度策略
* 副本、迁移、排空
* 审计和可视化

`s3-orchestrator` 已经具备 `object_locations`、`backend_quotas`、`backend_usage`、`pending_objects`、Multipart、Cleanup Queue、Rebalance、Scrub、Replica 等基础模块，并通过 AWS SDK v2 对接包括 Cloudflare R2 在内的 S3-compatible 后端，所以最难的“对象目录和多后端编排”不用从零写。([GitHub][1])

真正要重点改造的是三件事：

1. **增加独立的上传会话和容量预占机制。**
2. **让客户端直接上传目标 R2，而不是经过 orchestrator 文件代理。**
3. **将配额从“已完成对象统计”升级成“已使用 + 已预占 + 孤儿对象 + 指标漂移 + GB-month 预测”。**

---

# 一、目标架构

```text
                         浏览器 / App / 业务系统
                                  │
                        POST /v1/uploads/init
                                  │
                  ┌───────────────▼────────────────┐
                  │         Storage Control Plane  │
                  │                                │
                  │ Auth / Tenant Quota            │
                  │ Placement Engine               │
                  │ Capacity Reservation           │
                  │ Cloudflare Account Management  │
                  │ Presigned URL                  │
                  │ Object Catalog                 │
                  └───────────────┬────────────────┘
                                  │
                     原子预占容量、记录 UploadSession
                                  │
                 ┌────────────────▼─────────────────┐
                 │        PostgreSQL Metadata       │
                 │                                  │
                 │ objects / object_locations       │
                 │ upload_sessions                  │
                 │ capacity_reservations            │
                 │ backend_quotas                   │
                 │ account_daily_usage              │
                 └────────────────┬─────────────────┘
                                  │
                         返回目标 R2 签名地址
                                  │
                    ┌─────────────▼──────────────┐
                    │ 客户端直接上传目标 R2 Account │
                    └─────────────┬──────────────┘
                                  │
                       POST /uploads/{id}/complete
                                  │
                       HEAD 验证大小、类型、对象状态
                                  │
                       Reservation -> Committed
```

Cloudflare 官方支持服务端生成 Presigned PUT URL，让客户端直接上传 R2，不向客户端暴露 R2 API 凭证；浏览器直传还需要为 Bucket 配置 CORS。([Cloudflare Docs][2])

原来的 S3 Gateway 保持不变：

```text
AWS SDK / rclone
        │
        ▼
s3-orchestrator S3 Gateway
        │
        ▼
目标 R2 Account
```

新的默认业务路径改成：

```text
业务客户端
        │ 控制请求
        ▼
Storage API
        │ 返回签名地址
        ▼
客户端 ────────────────► 目标 R2
       文件数据直传
```

---

# 二、不要直接复用 `pending_objects`

`s3-orchestrator` 现有普通 PUT 流程大致是：

```text
选择后端
  -> 写入 pending intent
  -> orchestrator 上传对象
  -> 上传成功
  -> 原子写入对象位置和已用额度
  -> 清除 pending intent
```

这套模式成立，是因为 orchestrator 自己掌握文件流和上传结果。现有 `writepath.Coordinator` 会根据已知对象大小选节点，并在后端上传成功后把 Pending Intent 提升为正式对象记录。([GitHub][3])

改成客户端直传后，会出现一个时间窗口：

```text
已经给客户端签发 URL
但客户端可能：
- 尚未上传
- 上传一半
- 上传成功但没回调
- 上传了比申报更大的文件
- 重复使用 URL 覆盖对象
- 上传成功后浏览器关闭
```

因此应新增两个核心概念：

```text
UploadSession
CapacityReservation
```

现有 `pending_objects` 可以继续服务原有代理上传路径；直传路径使用新的状态机，不要硬揉在一起。

---

# 三、数据库改造

## 1. 扩展 `backend_quotas`

建议新增：

```sql
ALTER TABLE backend_quotas
ADD COLUMN raw_limit_bytes BIGINT NOT NULL DEFAULT 0,
ADD COLUMN static_buffer_bytes BIGINT NOT NULL DEFAULT 0,
ADD COLUMN dynamic_buffer_bytes BIGINT NOT NULL DEFAULT 0,
ADD COLUMN reserved_bytes BIGINT NOT NULL DEFAULT 0,
ADD COLUMN orphan_bytes BIGINT NOT NULL DEFAULT 0,
ADD COLUMN observed_bytes BIGINT NOT NULL DEFAULT 0,
ADD COLUMN drift_bytes BIGINT NOT NULL DEFAULT 0,
ADD COLUMN reserved_class_a BIGINT NOT NULL DEFAULT 0,
ADD COLUMN reserved_class_b BIGINT NOT NULL DEFAULT 0,
ADD COLUMN projected_gb_month NUMERIC(20, 6) NOT NULL DEFAULT 0,
ADD COLUMN billing_policy VARCHAR(32) NOT NULL DEFAULT 'FREE_CAPPED',
ADD COLUMN row_version BIGINT NOT NULL DEFAULT 0;
```

其中：

| 字段                     | 含义                          |
| ---------------------- | --------------------------- |
| `bytes_used`           | 已正式提交的物理对象                  |
| `reserved_bytes`       | 已签发上传凭证、尚未完成的容量             |
| `orphan_bytes`         | 已经存在于 R2，但尚未归入正式对象或删除未确认    |
| `observed_bytes`       | Cloudflare Metrics 观察到的实际容量 |
| `drift_bytes`          | Cloudflare 实际容量比内部账本多出的部分   |
| `static_buffer_bytes`  | 固定安全空间                      |
| `dynamic_buffer_bytes` | 根据 P95、漂移和上传行为动态计算的空间       |
| `projected_gb_month`   | 当前账期预计 GB-month             |
| `reserved_class_a/b`   | 已签发上传会话预计消耗的请求次数            |

## 2. 新增 `upload_sessions`

```sql
CREATE TABLE upload_sessions (
    id UUID PRIMARY KEY,
    tenant_id UUID NOT NULL,
    virtual_bucket_id UUID NOT NULL,

    logical_key TEXT NOT NULL,
    physical_key TEXT NOT NULL,
    backend_name TEXT NOT NULL,

    mode VARCHAR(16) NOT NULL,          -- single / multipart
    status VARCHAR(24) NOT NULL,

    declared_size BIGINT NOT NULL,
    reserved_bytes BIGINT NOT NULL,
    actual_size BIGINT,

    content_type TEXT,
    checksum_sha256 TEXT,

    provider_upload_id TEXT,
    part_size BIGINT,
    expected_parts INTEGER,

    reserved_class_a BIGINT NOT NULL DEFAULT 0,
    reserved_class_b BIGINT NOT NULL DEFAULT 0,

    idempotency_key TEXT,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at TIMESTAMPTZ,
    last_error TEXT,

    UNIQUE (tenant_id, idempotency_key)
);
```

建议状态：

```text
INITIATED
RESERVED
URL_ISSUED
UPLOADING
VERIFYING
COMMITTED
ABORTING
ABORTED
EXPIRED
ORPHANED
QUARANTINED
FAILED
```

## 3. 新增 `capacity_reservations`

```sql
CREATE TABLE capacity_reservations (
    id UUID PRIMARY KEY,
    upload_session_id UUID NOT NULL UNIQUE,
    backend_name TEXT NOT NULL,

    bytes_reserved BIGINT NOT NULL,
    class_a_reserved BIGINT NOT NULL DEFAULT 0,
    class_b_reserved BIGINT NOT NULL DEFAULT 0,

    status VARCHAR(16) NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    released_at TIMESTAMPTZ
);
```

## 4. 新增上传统计

```sql
CREATE TABLE upload_statistics (
    tenant_id UUID,
    content_family VARCHAR(32),     -- image/video/audio/archive/other
    upload_mode VARCHAR(16),

    sample_count BIGINT NOT NULL,
    ewma_size_bytes NUMERIC(20, 4),
    p50_size_bytes BIGINT,
    p95_size_bytes BIGINT,
    p99_size_bytes BIGINT,

    completion_rate NUMERIC(10, 6),
    declaration_ratio_p95 NUMERIC(10, 6),
    retry_rate NUMERIC(10, 6),

    updated_at TIMESTAMPTZ NOT NULL,

    PRIMARY KEY (tenant_id, content_family, upload_mode)
);
```

## 5. 增加账号级每日使用量

因为 R2 免费额度是账号计费维度，不能只看单个 Bucket：

```sql
CREATE TABLE account_daily_usage (
    cloudflare_account_id TEXT NOT NULL,
    usage_date DATE NOT NULL,

    internal_peak_bytes BIGINT NOT NULL,
    observed_peak_bytes BIGINT,
    class_a_operations BIGINT,
    class_b_operations BIGINT,

    source VARCHAR(20) NOT NULL,
    recorded_at TIMESTAMPTZ NOT NULL,

    PRIMARY KEY (cloudflare_account_id, usage_date, source)
);
```

---

# 四、对象物理 Key 必须改成不可变

直传时绝对不要让客户端直接 PUT 到正式逻辑 Key：

```text
user/video/demo.mp4
```

否则可能发生：

```text
旧 demo.mp4 正常在线
客户端拿到新上传 URL
上传失败或上传到一半
旧对象却已经被覆盖
数据库仍然指向这个 Key
```

建议区分：

```text
逻辑 Key：
videos/demo.mp4

物理 Key：
__objects/{tenant_id}/{object_id}/{version_uuid}
```

例如：

```text
逻辑：
videos/demo.mp4

物理：
__objects/t_123/obj_789/019912cd-6a32-7f30-a010.mp4
```

完成上传并验证后，数据库原子切换：

```text
videos/demo.mp4
    -> 新 physical_key
```

旧 physical_key 进入 Cleanup Queue。

这种模式的好处是：

* 上传失败不影响当前正式对象
* 同一 URL 不会覆盖其他对象
* 支持版本替换
* 可以安全回滚
* 迁移时只切换 Object Catalog
* 不需要在 R2 内做一次 staging Copy，避免额外占用和请求

这会要求扩展 `object_locations`：

```sql
ALTER TABLE object_locations
ADD COLUMN physical_key TEXT,
ADD COLUMN object_version UUID;
```

---

# 五、单文件直传完整流程

## 1. 初始化上传

```http
POST /v1/uploads
Authorization: Bearer xxx
Idempotency-Key: 019912cd...
Content-Type: application/json
```

```json
{
  "virtualBucket": "media",
  "logicalKey": "videos/demo.mp4",
  "filename": "demo.mp4",
  "sizeBytes": 184238221,
  "contentType": "video/mp4",
  "retentionDays": 30,
  "mode": "auto",
  "checksumSha256": null
}
```

这里的 `sizeBytes` 对浏览器上传应当是**必填项**，直接取：

```js
file.size
```

不要在用户已经能提供准确文件大小时，只用“平均上传大小”做硬额度判断。

## 2. 原子选择节点并预占

调度器依次筛选：

```text
status = ACTIVE
billing_policy 符合
允许当前租户使用
支持当前对象大小
账号未超 GB-month 预算
Class A/B 有剩余额度
有效可用容量足够
```

容量判断：

```text
effective_used =
    committed_bytes
  + reserved_bytes
  + orphan_bytes
  + drift_bytes
  + untracked_multipart_bytes
```

可接纳上限：

```text
admission_limit =
    raw_limit_bytes
  - static_buffer_bytes
  - dynamic_buffer_bytes
```

准入：

```text
effective_used + requested_reservation
    <= admission_limit
```

必须在数据库中原子完成：

```sql
UPDATE backend_quotas
SET
    reserved_bytes = reserved_bytes + $1,
    reserved_class_a = reserved_class_a + $2,
    reserved_class_b = reserved_class_b + $3,
    row_version = row_version + 1
WHERE backend_name = $4
  AND status = 'ACTIVE'
  AND (
      bytes_used
      + reserved_bytes
      + orphan_bytes
      + drift_bytes
      + $1
  ) <= (
      raw_limit_bytes
      - static_buffer_bytes
      - dynamic_buffer_bytes
  )
RETURNING *;
```

如果返回 0 行，说明该节点已不能接收，不是先查再更新，而是直接尝试下一个节点。

这是防止并发超额的核心。否则两个请求可能同时看到“还剩 200 MB”，然后各自上传 150 MB，最终超出 100 MB。

## 3. 创建会话和 Reservation

必须和上面的额度更新放在同一个数据库事务中：

```text
BEGIN

锁定 backend_quota
检查并增加 reserved_bytes
创建 upload_session
创建 capacity_reservation

COMMIT
```

事务成功后才能签发 URL。

## 4. 返回 Presigned PUT

```json
{
  "uploadId": "upl_019912cd",
  "objectId": "obj_019912ce",
  "mode": "single",
  "upload": {
    "method": "PUT",
    "url": "https://...r2.cloudflarestorage.com/...",
    "headers": {
      "Content-Type": "video/mp4"
    }
  },
  "expiresAt": "2026-08-24T15:15:00Z"
}
```

建议：

```text
普通文件签名有效期：5～15 分钟
每次使用唯一 physical_key
签入 Content-Type
只给当前对象的 PUT 权限
R2 Token 限定到指定 Bucket
```

Cloudflare 支持 Bucket 范围的 Object Read & Write Token；Presigned URL 可以签入请求所需的 Content-Type。([Cloudflare Docs][4])

## 5. 客户端直传

```ts
const response = await fetch(upload.url, {
  method: "PUT",
  headers: upload.headers,
  body: file,
});

if (!response.ok) {
  throw new Error(`R2 upload failed: ${response.status}`);
}
```

## 6. 客户端完成确认

```http
POST /v1/uploads/upl_019912cd/complete
Idempotency-Key: complete-upl_019912cd
```

```json
{
  "etag": "\"abcdef...\""
}
```

服务端不能相信客户端说“成功”，必须调用目标 R2：

```text
HEAD Object
```

验证：

```text
对象存在
实际大小 == declared_size
Content-Type 符合
physical_key 正确
会话没有过期或已被其他流程提交
checksum 符合（如启用）
```

## 7. Reservation 转正式用量

在一个数据库事务中完成：

```text
reserved_bytes -= reserved_size
bytes_used += actual_size
object_locations 写入
objects 切换 primary location
upload_session -> COMMITTED
reservation -> COMMITTED
旧版本加入 cleanup_queue
```

完成接口必须幂等：

```text
第一次调用：正常提交
第二次调用：返回同一对象结果
并发调用：只能一个事务真正提交
```

---

# 六、为什么平均大小不能直接用作硬限制

你提出“根据文件平均上传大小提前预算”是对的，但它适合用于：

* 剩余可上传文件数预估
* 未来 24 小时容量预测
* 什么时候添加新账号
* 动态安全区计算
* 用户套餐展示

不适合直接用于每次上传的容量预占。

例如平均文件大小为 100 MB：

```text
9 个文件：20 MB
1 个文件：820 MB

平均值仍然是 100 MB
```

如果剩余 300 MB，按平均值会显示还能上传 3 个，但下一个文件可能就是 820 MB。

因此应同时维护：

```text
EWMA：近期平均大小
P50：典型文件大小
P95：保守容量评估
P99：极端容量安全评估
```

推荐用途：

```text
准确 sizeBytes：
每个上传会话的硬预占

EWMA：
日增长速度、预计耗尽时间

P95：
安全空间、账号扩充触发

P99：
异常值告警和风险节点保护
```

统计最好按以下范围拆分：

```text
tenant
content_family
upload_mode
```

例如：

```text
客户 A / video / multipart
客户 A / image / single
客户 B / archive / multipart
```

不要把 5 MB 图片和 600 MB 视频混成一个平均数。

---

# 七、免费 R2 节点如何设置安全额度

R2 当前每个账号每月包含：

* 10 GB-month Standard Storage
* 100 万次 Class A
* 1000 万次 Class B
* 公网出站不收费

Standard Storage 超出后按 `$0.015/GB-month` 计费。R2 的 GB-month 根据计费周期内每日峰值存储量计算，而不是只看月底剩余容量。([Cloudflare Docs][5])

## 注意 10 GB 和 10 GiB

严格按免费预算管理时，建议把理论额度定义成：

```text
10,000,000,000 bytes
```

不要直接配置：

```text
10 * 1024 * 1024 * 1024
= 10,737,418,240 bytes
```

后者是 10 GiB，会比 10 GB 多约 7.37%。

R2 限制文档明确区分 GB 与 GiB；系统内部最好统一以 bytes 存储，再分别转换展示。([Cloudflare Docs][6])

## 冷启动推荐配置

每个 `FREE_CAPPED` 账号先这样配置：

```text
raw_limit_bytes       = 10,000,000,000
static_buffer_bytes   = 1,000,000,000
dynamic_buffer_bytes  = 1,000,000,000
admission_limit       = 8,000,000,000
```

也就是只把每个免费账号的 **8 GB** 作为首期稳定可分配空间。

运行两个完整账期、确认指标漂移和孤儿对象较少后，再调整为：

```text
static_buffer = 1 GB

dynamic_buffer =
    max(
      500 MB,
      2 × P95 文件大小,
      P99 指标漂移,
      最大未确认孤儿对象预估
    )
```

最终一般控制在：

```text
8.0～8.5 GB 可分配
```

不建议免费节点超过：

```text
9 GB
```

除非允许产生小额付费。

## 动态 Buffer 示例

假设：

```text
P95 文件大小：180 MB
P99 指标漂移：220 MB
```

则：

```text
dynamic_buffer =
max(
  500 MB,
  2 × 180 MB,
  220 MB
)
= 500 MB
```

可分配容量：

```text
10 GB - 1 GB - 0.5 GB
= 8.5 GB
```

如果 P95 上升到 600 MB：

```text
dynamic_buffer =
max(
  500 MB,
  1.2 GB,
  220 MB
)
= 1.2 GB
```

可分配容量自动下降为：

```text
7.8 GB
```

---

# 八、剩余文件数和账号扩充预测

后台可以同时展示两个数字：

```text
预计还可上传：
safe_remaining / EWMA

保守还可上传：
safe_remaining / P95
```

例如：

```text
安全剩余容量：2.4 GB
近期平均大小：120 MB
P95 大小：300 MB
```

显示：

```text
预计剩余：约 20 个
保守剩余：约 8 个
```

账号扩充触发条件建议为：

```text
保守剩余文件数 < 20
```

或者：

```text
总安全剩余容量
<
max(
  未来 24 小时预计新增容量 × 1.5,
  3 × P95 文件大小
)
```

24 小时预测：

```text
forecast_24h =
EWMA_成功上传文件大小
×
EWMA_每日成功上传数量
```

例如：

```text
平均文件：120 MB
日均上传：30 个

forecast_24h = 3.6 GB
```

如果全部免费节点只剩 2 GB，就应：

```text
触发账号扩充告警
或自动启用已接入的备用账号
或切换到 PAYG_OVERFLOW 节点
```

---

# 九、GB-month 不能只看当前物理容量

需要每天记录账号级峰值：

```text
account_daily_usage
```

当前周期已消耗：

```text
used_gb_month =
已完成日期的 daily_peak_gb 总和
÷
计费周期天数
```

预计周期结束：

```text
projected_gb_month =
(
  已完成日期峰值总和
  +
  预计剩余日期每日峰值总和
)
÷
计费周期天数
```

新文件的当前周期边际影响：

```text
marginal_gb_month =
file_size_gb
×
预计在本周期保留天数
÷
本周期天数
```

准入时应同时检查两个预算：

```text
1. 当前周期 projected_gb_month
2. 下一完整周期的稳定占用量
```

否则月末新增 9 GB 文件时：

```text
本月 GB-month 看起来很低
但下个月完整周期会稳定占用 9 GB
```

推荐免费节点初始配置：

```text
current_cycle_budget_gb_month = 8.0
next_cycle_stable_budget_gb  = 8.0
```

经过稳定运行后再提高到 8.5。

---

# 十、Class A/B 请求次数也要预占

R2 的 PutObject、CreateMultipartUpload、UploadPart、CompleteMultipartUpload 等属于 Class A；HeadObject 和 GetObject 属于 Class B。免费额度分别是每月 100 万和 1000 万次，而且计费单位存在向上取整，所以不要刚好卡着免费阈值运行。([Cloudflare Docs][5])

建议运行上限：

```text
Class A：800,000
Class B：8,000,000
```

单 PUT 初始化时预占：

```text
Class A reserved = 2
```

其中：

```text
1 次实际 PUT
1 次重试安全预算
```

完成验证预占：

```text
Class B reserved = 2
```

其中：

```text
1 次 HEAD
1 次核对或重试
```

Multipart 的预占：

```text
part_count = ceil(file_size / part_size)

Class A reserved =
    1                  // CreateMultipartUpload
  + part_count         // UploadPart
  + 1                  // CompleteMultipartUpload
  + retry_allowance
```

例如 1 GB 文件，每 Part 64 MB：

```text
part_count = 16
base Class A = 18
```

可以预占：

```text
20～22 次
```

完成后：

```text
reserved 操作数释放
实际操作数进入内部账本
Cloudflare 指标再进行校正
```

---

# 十一、Multipart 必须单独重写

现有 `s3-orchestrator` 的 Multipart 有两个不适合免费额度池的地方：

1. `CreateMultipartUpload` 当前按大小 **0** 选择后端。
2. 完成时会由 orchestrator 读取分片并重新组装最终对象。

这意味着它不会在初始化时为完整文件预留容量，也无法实现真正的客户端到 R2 原生 Multipart 直传。([GitHub][7])

应新增一套 `native_direct_multipart`。

## 初始化

```http
POST /v1/uploads
```

```json
{
  "sizeBytes": 2147483648,
  "mode": "multipart",
  "partSizeBytes": 67108864
}
```

服务端：

```text
1. 按完整 2 GB 预占容量
2. 选择并固定一个 R2 节点
3. 调用目标 R2 CreateMultipartUpload
4. 保存 provider_upload_id
5. 计算 expected_parts
6. 返回 uploadId
```

## 获取 Part 签名

```http
POST /v1/uploads/{uploadId}/parts:presign
```

```json
{
  "partNumbers": [1, 2, 3, 4]
}
```

返回每个 Part 的 Presigned UploadPart URL。

可以批量签 4～10 个 Part，避免一次生成几百个长 URL。

## 完成 Multipart

客户端提交：

```json
{
  "parts": [
    {
      "partNumber": 1,
      "etag": "\"...\""
    },
    {
      "partNumber": 2,
      "etag": "\"...\""
    }
  ]
}
```

服务端：

```text
验证会话
检查 Part 数量
调用 CompleteMultipartUpload
HEAD 最终对象
验证最终 size
Reservation -> Committed
```

这里只是控制请求和 XML Manifest 经过控制面，文件 Part 本身仍是客户端直传 R2。

## 过期处理

Multipart Reaper 定期处理：

```text
过期但未完成
  -> AbortMultipartUpload
  -> 确认终止
  -> 释放容量 Reservation
```

不要仅仅因为会话过期就立即释放 Reservation。必须先确认：

```text
最终对象不存在
并且 Multipart 已 Abort
```

否则会出现“数据库认为空间释放了，但 R2 里 Part 仍然存在”的隐形超额。

R2 支持单次上传最大 5 GiB；更大的对象应使用 Multipart，Multipart 对象最大接近 5 TiB、最多 10,000 个 Part。([Cloudflare Docs][6])

---

# 十二、直传之后如何仍然保持管理能力

直传不等于失控，关键是控制面必须掌握：

```text
谁申请的
申请上传什么
选择了哪个账号
占用了多少额度
签发了哪个 physical_key
URL 什么时候过期
对象是否真正存在
是否已经完成验证
是否成为孤儿对象
```

需要四层核对。

## 第一层：内部上传账本

实时记录：

```text
UploadSession
Reservation
Object
ObjectLocation
```

这是实时调度依据。

## 第二层：完成时 HEAD

客户端回调后立即验证实际对象。

这是提交依据。

## 第三层：R2 Event Notifications

R2 可以在对象创建、覆盖和删除时向 Cloudflare Queue 发送通知。可以用它发现：

* 上传成功但客户端没有调用 Complete
* 绕过系统上传的对象
* 后台手工删除
* 对象覆盖
* 删除事件未进入内部账本

事件通知适合作为补偿和对账通道，而不应作为容量预占的主依据。([Cloudflare Docs][8])

## 第四层：Cloudflare Metrics 对账

Cloudflare GraphQL 提供：

```text
r2StorageAdaptiveGroups
r2OperationsAdaptiveGroups
```

可以观察对象存储量、对象数量和操作量；相关指标只保留有限时间，因此需要按小时或按天同步到自己的数据库。([Cloudflare Docs][9])

定时计算：

```text
internal_known_bytes =
正式对象
+ 已知孤儿对象
+ 已知 Multipart 临时量

drift_bytes =
max(
  0,
  provider_observed_bytes - internal_known_bytes
)
```

调度时把 `drift_bytes` 也视为已占用容量。

---

# 十三、上传完成回调丢失怎么处理

这是直传系统最常见的问题之一：

```text
客户端上传成功
但浏览器关闭
Complete API 没有调用
```

需要 `UploadSessionReaper`：

```text
扫描已过期 URL_ISSUED / UPLOADING 会话
        │
        ▼
HEAD physical_key
        │
   ┌────┴────┐
   │         │
存在       不存在
   │         │
检查大小    释放 Reservation
   │
符合声明
   │
自动提交对象
   │
不符合
   │
QUARANTINED / 删除
```

也就是：

```text
过期 != 上传失败
```

不能会话一过期就盲目释放容量。

---

# 十四、恶意超大上传无法只靠 Presigned URL 完全消除

这里有一个必须诚实说明的技术边界。

Cloudflare 文档明确提供 Presigned PUT、有效期和 Content-Type 等签名能力，但没有提供类似 Presigned POST Policy 中 `content-length-range` 的 R2 PUT 大小策略说明。由此推断，不能把普通 Presigned PUT URL 本身当成“最大上传字节数”的绝对防线。([Cloudflare Docs][10])

也就是说，客户端申报：

```text
100 MB
```

理论上可能尝试上传：

```text
500 MB
```

你的系统能在完成后通过 HEAD 发现并删除，但 500 MB 曾经短暂存在。

因此：

## 可信客户端

采用：

```text
准确 sizeBytes
+
容量预占
+
短期 URL
+
唯一 physical_key
+
完成 HEAD
+
1～2 GB 安全 Buffer
```

已经足够稳。

## 不可信第三方客户端

建议：

```text
FREE_CAPPED 节点：
不开放未知大小和宽泛 S3 写权限

PAYG 节点：
允许直传，但设置租户账单上限

严格大小要求：
经过可检查 Content-Length 的流式代理通道
```

换句话说：

> 在恶意客户端模型下，纯直传无法数学意义上保证“从未发生瞬时超额”；安全 Buffer、准确预占、短 URL 和事后校验能把风险控制到很低。要求绝对硬限制的上传，应走代理路径或付费溢出节点。

未知大小的上传应直接处理为：

```text
FREE_CAPPED：拒绝
PAYG_BUDGETED：允许
S3 Gateway：按策略允许
```

不要用平均文件大小给未知流在免费节点上开绿灯。

---

# 十五、代码目录怎么改

建议保留现有目录，新建独立控制面包：

```text
internal/
├── backend/
│   ├── s3.go
│   ├── presign.go                 # 新增
│   └── native_multipart.go        # 新增
│
├── uploadsession/                 # 新增
│   ├── manager.go
│   ├── admission.go
│   ├── placement.go
│   ├── reservation.go
│   ├── single.go
│   ├── multipart.go
│   ├── finalize.go
│   ├── abort.go
│   ├── reaper.go
│   └── statistics.go
│
├── store/
│   ├── core/
│   │   ├── interfaces.go          # 扩展接口
│   │   ├── upload_session.go
│   │   ├── reservation.go
│   │   └── capacity.go
│   ├── postgres/
│   └── sqlite/
│
├── worker/
│   ├── upload_reaper.go
│   ├── multipart_reaper.go
│   ├── metrics_reconciler.go
│   └── event_consumer.go
│
└── proxy/
    ├── object/                    # 原 S3 Gateway 保留
    ├── multipart/
    └── writepath/
```

`s3-orchestrator` 的 Store 层已有较窄的接口划分，`MetadataStore` 组合了 Object、Quota、Multipart 等 Store，适合继续增加 `UploadSessionStore` 和 `ReservationStore`，而不是让 HTTP Handler 直接写数据库。([GitHub][11])

建议接口：

```go
type ReservationStore interface {
    TryReserve(
        ctx context.Context,
        req ReserveRequest,
    ) (*Reservation, error)

    CommitReservation(
        ctx context.Context,
        sessionID uuid.UUID,
        actualSize int64,
        location ObjectLocation,
    ) error

    MarkOrphaned(
        ctx context.Context,
        sessionID uuid.UUID,
        actualSize int64,
        reason string,
    ) error

    ReleaseReservation(
        ctx context.Context,
        sessionID uuid.UUID,
    ) error
}

type UploadSessionStore interface {
    Create(ctx context.Context, session UploadSession) error
    GetForUpdate(ctx context.Context, id uuid.UUID) (*UploadSession, error)
    MarkVerifying(ctx context.Context, id uuid.UUID) error
    MarkCommitted(ctx context.Context, id uuid.UUID) error
    MarkExpired(ctx context.Context, id uuid.UUID) error
    ListExpired(ctx context.Context, limit int) ([]UploadSession, error)
}

type DirectUploadBackend interface {
    PresignPut(
        ctx context.Context,
        req PresignPutRequest,
    ) (*PresignedRequest, error)

    HeadObject(
        ctx context.Context,
        key string,
    ) (*ObjectMetadata, error)

    DeleteObject(
        ctx context.Context,
        key string,
    ) error

    CreateMultipartUpload(
        ctx context.Context,
        req CreateMultipartRequest,
    ) (*MultipartHandle, error)

    PresignUploadPart(
        ctx context.Context,
        req PresignPartRequest,
    ) (*PresignedRequest, error)

    CompleteMultipartUpload(
        ctx context.Context,
        req CompleteMultipartRequest,
    ) error

    AbortMultipartUpload(
        ctx context.Context,
        req AbortMultipartRequest,
    ) error
}
```

---

# 十六、调度策略建议：`risk_aware_pack`

现有 `pack` 适合尽量填满前一个节点，`spread` 适合均摊。对免费额度池，建议增加：

```text
risk_aware_pack
```

流程：

```text
1. 过滤非 ACTIVE 节点
2. 过滤不符合 billing policy 的节点
3. 过滤 projected_gb_month 超预算节点
4. 过滤 Class A/B 超运行预算节点
5. 计算 effective_used
6. 检查完整 Reservation 能否放下
7. 检查放入后是否仍保留动态安全空间
8. 按放入后剩余空间由小到大排序
9. 选择最小非负节点
10. 免费池全部失败则进入 PAYG_OVERFLOW
```

评分可以先不做复杂机器学习：

```text
score =
    remaining_after_upload
  + account_risk_penalty
  + operation_budget_penalty
  + drift_penalty
  + health_penalty
```

选择分数最小的可用节点。

例如：

```text
A 剩余安全容量：500 MB
B 剩余安全容量：1.5 GB
C 剩余安全容量：4 GB

上传文件：1 GB
```

A 无法接收，B 放入后剩 500 MB，C 放入后剩 3 GB，因此选择 B。

这样比 Round Robin 更少产生容量碎片。

---

# 十七、直传模式与现有压缩、加密功能的冲突

现有代理上传路径可以：

```text
读取 body
压缩
计算哈希
应用应用层加密
重试
再写入后端
```

直传后字节不经过 orchestrator，因此这些转换无法在服务器端执行。

需要明确三种策略：

| 文件策略                | 上传路径              |
| ------------------- | ----------------- |
| 普通媒体文件，使用 R2 原生静态加密 | 客户端直传             |
| 客户端先完成压缩或加密         | 客户端直传，申报转换后大小     |
| 必须由平台应用层加密、扫描、转换    | 原 S3 Gateway/代理上传 |

因此每个虚拟 Bucket 可以增加：

```text
upload_mode_policy:
  DIRECT_ONLY
  PROXY_ONLY
  AUTO
```

以及：

```text
transform_policy:
  NONE
  CLIENT_SIDE
  SERVER_SIDE
```

---

# 十八、管理后台需要新增哪些看板

每个账号至少显示：

| 指标                |       示例 |
| ----------------- | -------: |
| Cloudflare 理论免费额度 | 10.00 GB |
| 系统安全可分配额度         |  8.00 GB |
| 已提交对象             |  6.20 GB |
| 上传预占              |  0.45 GB |
| 孤儿对象              |  0.08 GB |
| 指标漂移              |  0.03 GB |
| 当前安全剩余            |  1.24 GB |
| EWMA 文件大小         |   112 MB |
| P95 文件大小          |   286 MB |
| 预计剩余文件数           |       11 |
| 保守剩余文件数           |        4 |
| 预计周期末 GB-month    |     7.41 |
| Class A 运行利用率     |      28% |
| Class B 运行利用率     |      16% |
| 节点状态              |   ACTIVE |

全局显示：

```text
免费池总理论容量
免费池安全容量
已提交容量
已预占容量
付费溢出容量
未来 24 小时预计增长
预计账号耗尽日期
待接入账号数量
需要排空的节点
异常漂移节点
```

---

# 十九、开发落地顺序

## 阶段一：先做容量预占，不改数据路径

先在原 S3 Proxy 上传中加入：

```text
reserved_bytes
原子 TryReserve
CommitReservation
ReleaseReservation
动态 Buffer
账号级 GB-month 预算
```

这样先验证配额算法，不同时改上传链路。

验收：

```text
100 个并发上传不能把节点预占到上限之外
失败上传会释放额度
进程崩溃后能通过 Reaper 恢复
```

## 阶段二：单文件客户端直传

加入：

```text
POST /v1/uploads
POST /v1/uploads/{id}/complete
POST /v1/uploads/{id}/abort
GET  /v1/uploads/{id}
```

首版限制：

```text
必须提供准确 sizeBytes
只允许不可变 physical_key
不允许直传覆盖
不允许未知大小
默认只做单 PUT
```

为了快速上线，可以先规定：

```text
单文件直传上限：100 MB 或 256 MB
```

这不是 R2 的限制，而是首版风险控制。

## 阶段三：原生 Multipart 直传

加入：

```text
CreateMultipartUpload
Part URL 批量签发
CompleteMultipartUpload
AbortMultipartUpload
Multipart Reaper
完整文件容量预占
```

## 阶段四：Cloudflare 对账闭环

加入：

```text
R2 Event Notifications
GraphQL Metrics
内部账本和实际容量 Drift
GB-month 每日快照
Class A/B 核对
孤儿对象扫描
```

## 阶段五：多账号和付费溢出

节点策略：

```text
FREE_CAPPED
PAYG_BUDGETED
PAYG_OVERFLOW
CUSTOMER_BYOC
BACKUP_ONLY
```

加入：

```text
免费节点达到安全上限后自动停止分配
备用账号自动启用
付费节点自动溢出
节点 DRAINING
跨账号迁移
```

---

# 二十、必须通过的测试

正式接入真实文件前，至少覆盖这些情况：

| 场景                        | 预期                 |
| ------------------------- | ------------------ |
| 100 个请求并发预占最后 100 MB      | 只能允许符合容量的请求成功      |
| 同一 Init 重试                | 返回同一 UploadSession |
| 同一 Complete 调用两次          | 幂等返回同一对象           |
| URL 签发后客户端不上传             | 到期后释放 Reservation  |
| 上传成功但 Complete 丢失         | Reaper HEAD 后自动提交  |
| 上传比声明大                    | 不提交，进入隔离/删除        |
| 上传比声明小                    | 按策略拒绝或按实际值提交       |
| URL 被重复使用                 | 只允许一个最终版本进入对象目录    |
| DB 在上传后、提交前崩溃             | 能自动发现并恢复           |
| R2 Token 被撤销              | 节点转 OFFLINE，不再分配   |
| Multipart 放弃              | Abort 后才释放额度       |
| Event 延迟或重复               | 消费处理幂等             |
| Cloudflare Metrics 大于内部账本 | Drift 计入已用空间       |
| 免费节点不足                    | 自动选择 PAYG_OVERFLOW |
| 一个账号多个 Bucket             | 账号级免费预算统一汇总        |
| 对象替换上传失败                  | 原对象仍然正常可读          |
| 节点排空                      | 新对象不再写入，旧对象正常迁移    |

生产多实例应使用 PostgreSQL；`s3-orchestrator` 的文档也将 SQLite 定位为单实例使用，而 PostgreSQL 用于多实例协调。([GitHub][12])

---

# 最推荐的首版参数

```yaml
storage_policy:
  raw_free_limit_bytes: 10000000000

  static_buffer_bytes: 1000000000
  dynamic_buffer_floor_bytes: 1000000000

  max_admission_bytes: 8000000000

  class_a_operational_limit: 800000
  class_b_operational_limit: 8000000

  direct_upload:
    require_declared_size: true
    allow_unknown_size_on_free_nodes: false
    immutable_physical_keys: true
    presigned_ttl_seconds: 600
    initial_single_upload_max_bytes: 268435456

  expansion:
    conservative_slots_threshold: 20
    forecast_multiplier: 1.5

  overflow:
    enabled: true
    target_policy: PAYG_BUDGETED
```

核心准入逻辑最终可以概括为：

```text
实际可调度占用 =
正式对象
+ 上传预占
+ 孤儿对象
+ Cloudflare 指标漂移
+ 未确认 Multipart

只有当：

实际可调度占用
+ 本次准确文件大小
<=
10 GB
- 固定安全空间
- 动态安全空间

并且：

预计 GB-month 未超预算
Class A/B 未超运行预算

才签发上传 URL。
```

最关键的一条是：

> **平均文件大小只负责预测未来，准确 `sizeBytes` 负责当前上传的硬预占，P95/P99 负责安全 Buffer。**

这样二开之后，`s3-orchestrator` 不再只是“把文件代理到多个 S3”，而会变成：

```text
统一对象目录
+
Cloudflare 多账号存储编排
+
实时容量预占
+
客户端直传
+
免费额度风险控制
+
付费节点自动溢出
+
完整可视化与对账
```

而且原来的 S3 Gateway 仍然保留，可以继续兼容 rclone、AWS SDK 和传统 S3 客户端，不会为了直传把已有能力推倒重来。

[1]: https://github.com/afreidah/s3-orchestrator/blob/main/docs/architecture.md "s3-orchestrator/docs/architecture.md at main · afreidah/s3-orchestrator · GitHub"
[2]: https://developers.cloudflare.com/r2/objects/upload-objects/?utm_source=chatgpt.com "Upload objects - R2 - Cloudflare Docs"
[3]: https://github.com/afreidah/s3-orchestrator/blob/main/internal/proxy/writepath/coordinator.go "s3-orchestrator/internal/proxy/writepath/coordinator.go at main · afreidah/s3-orchestrator · GitHub"
[4]: https://developers.cloudflare.com/r2/api/tokens/?utm_source=chatgpt.com "Authentication · Cloudflare R2 docs"
[5]: https://developers.cloudflare.com/r2/pricing/?utm_source=chatgpt.com "R2 pricing - Cloudflare Docs"
[6]: https://developers.cloudflare.com/r2/platform/limits/?utm_source=chatgpt.com "Limits · Cloudflare R2 docs"
[7]: https://github.com/afreidah/s3-orchestrator/blob/main/internal/proxy/multipart/manager.go "s3-orchestrator/internal/proxy/multipart/manager.go at main · afreidah/s3-orchestrator · GitHub"
[8]: https://developers.cloudflare.com/r2/buckets/event-notifications/?utm_source=chatgpt.com "Event notifications - R2 - Cloudflare Docs"
[9]: https://developers.cloudflare.com/r2/platform/metrics-analytics/?utm_source=chatgpt.com "Metrics and analytics · Cloudflare R2 docs"
[10]: https://developers.cloudflare.com/r2/api/s3/presigned-urls/?utm_source=chatgpt.com "Presigned URLs · Cloudflare R2 docs"
[11]: https://github.com/afreidah/s3-orchestrator/blob/main/internal/store/core/interfaces.go "s3-orchestrator/internal/store/core/interfaces.go at main · afreidah/s3-orchestrator · GitHub"
[12]: https://github.com/afreidah/s3-orchestrator "GitHub - afreidah/s3-orchestrator: Multi-backend S3-compatible proxy with quota management, replication, and rebalancing · GitHub"
