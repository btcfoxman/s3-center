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

代理是 Backend 级配置：该账号由 S3 Center 发起的上传、下载、检查和维护操作都走自己的连接池与代理，不影响其他账号。管理端只显示脱敏后的 `scheme://host:port`。

> 浏览器直传使用 R2 预签名 URL，数据从浏览器直接到 R2，不经过 S3 Center 的服务端代理。如果该账号必须绑定固定代理 IP，请禁用/避免直传，让客户端通过 S3 Center 的 S3 API 上传，或让客户端本身具备相同网络出口。

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

- 当前管理端支持新增和查看，不提供在线编辑凭据、修改代理或删除账号。需要回收账号时，应先在 Cloudflare 撤销 Token，并按维护流程迁移对象后更新数据库/配置；不要直接删除仍承载对象的 Backend。
- 页面不调用 Cloudflare Billing API，因此不会显示 Cloudflare 全账号的权威 Class A/Class B 或来自其他工具的用量。
- 一个 Cloudflare Account 的免费额度由 Account 下的所有 R2 Bucket 共享。为同一个 Account 新增 Bucket 不会新增 10 GB；只有接入另一个独立且合规授权的 Account 才会形成另一张免费额度卡。

Cloudflare 官方参考：[S3 API 凭据与 endpoint](https://developers.cloudflare.com/r2/api/tokens/)、[R2 定价与免费额度](https://developers.cloudflare.com/r2/pricing/)。
