# 管理端动态接入 Cloudflare R2 账号

管理端现在可以在不修改 YAML、不重启服务的情况下，把已授权的 Cloudflare R2 Bucket 加入存储池。每个接入项使用独立凭据和独立网络出口；同一个 Cloudflare Account 下的多个 Bucket 会聚合到同一张额度卡。

这里的“添加账号”指接入你有权使用的现有 Cloudflare Account/R2 Bucket。系统不会注册 Cloudflare 账号，也不会绕过服务商规则获取额度。

## 接入前准备

1. 在 Cloudflare Dashboard 进入目标 Account 的 R2。
2. 创建目标 Bucket；管理端不会代替 Cloudflare 创建 Bucket。
3. 创建只授权目标 Bucket 的 **Object Read & Write** R2 API Token。
4. 记录 Account ID、R2 Access Key ID 和 Secret Access Key。

管理端需要的是 R2 S3 凭据，不是普通 Cloudflare API Token，也不是 Global API Key。Secret 只在提交时出现，服务端会用 AES-256-GCM 整体加密账号定义后写入数据库，列表和日志都不会返回它。

## 在管理端添加

登录 `https://s3-center.aiid.qzz.io/ui/`，在 **Cloudflare 账号额度** 区域点击 **接入 R2 账号**，填写：

| 字段 | 含义 |
|---|---|
| Backend 名称 | 本系统内唯一标识；建议 `r2-team-02`，保存后不可改名 |
| 账号备注 | 便于识别账号归属，不参与连接 |
| Account ID | Cloudflare Account 页面显示的 32 位十六进制 ID |
| Jurisdiction | 一般选 Default；只有 Bucket 创建在 EU、US、FedRAMP jurisdiction 时才选对应值 |
| Bucket | 已存在且 Token 有读写权的 R2 Bucket |
| R2 Access Key / Secret | 上一步创建 Token 后得到的 S3 凭据 |
| 出口代理 | 可选，支持 HTTP、HTTPS、SOCKS5、SOCKS5H；不写协议时按 HTTP |
| 预警线 | 当前存储达到免费存储额度的百分比时转为黄色预警 |

提交后服务端会通过所填代理对目标 Bucket 执行一次 `HeadBucket`。只有 endpoint、代理、凭据和 Bucket 权限全部验证成功后，定义才会加密保存并立即加入读写调度。

可用代理示例：

```text
http://203.0.113.10:8080
http://user:password@203.0.113.10:8080
socks5://127.0.0.1:1080
socks5h://user:password@proxy.example:1080
```

每个 Backend 使用独立连接池和代理，不影响其他账号。同一 Cloudflare Account 下如果接入了多个 Bucket，管理端的 **设置 API 代理** 会把新出口作为账号级策略，一次应用到该账号的全部动态 Backend。管理端只显示脱敏后的 `scheme://host:port`。

### 在线修改账号代理

在 Cloudflare 额度卡中点击 **设置 API 代理**，填写新的代理地址并提交。服务端按以下顺序执行：

1. 使用候选代理为该 Account 下每个动态托管 Bucket 创建独立客户端。
2. 逐个调用 R2 S3 API `HeadBucket` 验证代理、凭据和 Bucket 权限。
3. 全部通过后，加密保存所有 Backend 定义并逐个原子热替换运行时客户端，不需要重启。
4. 任一 Bucket 验证或持久化失败时，整体保留原代理和原客户端。

输入留空并提交表示恢复服务器直连。如果代理包含用户名、密码，修改时必须重新输入完整地址；系统不会把已保存的代理认证信息返回浏览器。静态 YAML Backend 的代理仍需修改 `backends[].proxy_url` 后重新部署。

> 索引模式下，代理只约束 S3 Center 发起的 `HeadBucket`、`HEAD`、`DELETE` 和 Multipart 控制调用。文件 PUT/GET 使用 R2 预签名 URL，由第三方客户端直接连接 R2，不经过 S3 Center 的代理。如果 R2 凭据策略强制固定来源 IP，第三方客户端本身也必须使用获准出口；否则该账号不适合加入零代理数据平面。

## 手动删除存储内容

管理端的 **对象浏览器** 按逻辑 Bucket 和目录展示索引。每个对象都有 **删除** 操作，每个目录都有 **删除目录** 操作；目录删除需要输入 `DELETE` 二次确认。

删除不是只改数据库：控制面先从索引解析对象的实际 Backend 和物理 Key，再通过该 Backend 的账号代理调用 R2/S3 `DeleteObject`。如果一个逻辑对象存在多份副本，会向每个实际 Backend 发出删除请求。成功后释放内部额度；上游暂时失败的副本进入 Cleanup Queue 自动重试。

运维系统也可以直接调用 Admin API：

```bash
# 删除单个逻辑对象
curl -X DELETE -H "X-Admin-Token: $ADMIN_API_TOKEN" \
  "https://s3-api.aiid.qzz.io/admin/api/objects/assets/path/report.pdf"

# 删除整个逻辑目录（prefix 必须非空并建议以 / 结尾）
curl -X DELETE -H "X-Admin-Token: $ADMIN_API_TOKEN" \
  "https://s3-api.aiid.qzz.io/admin/api/objects?prefix=assets/path/"
```

目录是对象 Key 的前缀，不是 R2 中的真实文件夹。该操作只删除 S3 Center 索引内的对象；绕过本系统写入且尚未同步到索引的上游对象不会被误删。

## 额度卡如何理解

- Cloudflare R2 Standard 免费存储是每个 Account 每月 10 GB-month。页面把同一 Account ID 下所有已接入 Bucket 的本地已知对象与 orphan bytes 相加，展示当前水位。
- 10 GB-month 按月内每日峰值平均计费，因此“当前 4 GB”不等于整月一定只用 4 GB；卡片是及时预警指标，不是 Cloudflare 账单替代品。
- 系统默认保留 2 GB 安全缓冲，可调度写入上限为 8 GB；达到安全线后系统停止向该 Account 分配新对象，避免元数据漂移或月内峰值把用量推过免费线。
- 请求次数是经过 S3 Center 且由本地计数器观察到的操作数。Cloudflare Dashboard、其他客户端以及浏览器直传不会计入，Class A/Class B 免费请求额度应以 Cloudflare Billing 页面为准。
- 红色表示当前本地已知存储已经达到或超过 10 GB 免费线；黄色表示达到自定义预警线。

## 运行配置与灾难恢复

生产配置需启用：

```yaml
managed_backends:
  enabled: true
  master_key: "${DYNAMIC_BACKEND_MASTER_KEY}"
  default_warning_percent: 80
```

`DYNAMIC_BACKEND_MASTER_KEY` 至少 32 字节，必须只保存在服务器 Secret/`.env` 中，不得提交到 Git。它是解密所有动态账号定义的根密钥：

- 首次启用时生成高熵随机值，例如 `openssl rand -hex 32`。
- 数据库备份必须与该密钥备份配套；丢失密钥后动态账号凭据无法恢复。
- 不要直接替换现有密钥。当前版本没有在线密钥轮换流程，替换后服务会因无法解密已有定义而拒绝启动。

数据库迁移会自动创建 `managed_backends` 表。动态定义在重启后自动解密、验证并恢复到运行时；启动期间不会再次联网执行 `HeadBucket`，但正常健康检查和首次业务请求仍会反映连接错误。

## 当前边界

- 当前管理端支持新增、查看以及按账号在线修改代理；暂不提供在线修改 R2 凭据或直接删除账号。需要回收账号时，应先在 Cloudflare 撤销 Token，并按维护流程迁移对象后更新数据库/配置；不要直接删除仍承载对象的 Backend。
- 页面不调用 Cloudflare Billing API，因此不会显示 Cloudflare 全账号的权威 Class A/Class B 或来自其他工具的用量。
- 一个 Cloudflare Account 的免费额度由 Account 下的所有 R2 Bucket 共享。为同一个 Account 新增 Bucket 不会新增 10 GB；只有接入另一个独立且合规授权的 Account 才会形成另一张免费额度卡。

Cloudflare 官方参考：[S3 API 凭据与 endpoint](https://developers.cloudflare.com/r2/api/tokens/)、[R2 定价与免费额度](https://developers.cloudflare.com/r2/pricing/)。
