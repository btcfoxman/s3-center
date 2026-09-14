# 管理端动态接入 Cloudflare R2 账号

管理端现在可以在不修改 YAML、不重启服务的情况下，把已授权的 Cloudflare R2 Bucket 加入存储池。每个接入项使用独立凭据和独立网络出口；同一个 Cloudflare Account 下的多个 Bucket 会聚合到同一张额度卡。

这里的“添加账号”指接入你有权使用的现有 Cloudflare Account/R2 Bucket。系统不会注册 Cloudflare 账号，也不会绕过服务商规则获取额度。

## 推荐：隔离浏览器快速接入

管理员不再需要手工寻找 Account ID、R2 Access Key 和 Secret。登录管理端，在 **Cloudflare 账号额度** 区域点击 **新增 CF 身份**：

1. 首次使用先下载 Connector 2.1 ZIP，解压后运行 `powershell -ExecutionPolicy Bypass -File .\install.ps1`。它只为当前 Windows 用户注册 `s3center://`，不需要管理员权限，并要求安装最新 Google Chrome。若曾安装旧版，也必须运行一次新版安装脚本；旧 Profile 会保留以便恢复，但 2.1 不会再打开它。
2. 填写身份备注，以及该 CF 身份固定使用的代理（HTTP、HTTPS、SOCKS5/SOCKS5H，均可含用户名密码）。
3. 点击 **创建隔离登录**，再点击 **打开隔离 Cloudflare 登录**。
4. Connector 在 `%LOCALAPPDATA%\S3Center\Profiles\<profile-id>\browser-chrome-v1` 启动真正的原生 Chrome 进程，不使用本机默认 Profile、Edge、Playwright、WebDriver 或远程调试。新 Profile 初始不绑定浏览器账号，并关闭同步、首次启动资料导入、扩展和密码保存。代理凭据由 Connector 内存中的随机本机转发端口承接，本机控制页经同一路由访问 Cloudflare trace 校验出口 IP；代理失败会终止，不会回退本机直连。
5. 在本机控制页点击 **打开 Cloudflare 官方页面**，在新标签页登录对应 CF 身份。预填页面的权限和名称已填好；确认创建后，复制只显示一次的 **API Token**，并把 Token 与当前 Cloudflare Account 页面地址粘贴回本机控制页。Connector 会从地址中取得 Account ID，再经随机 `127.0.0.1` 桥接安全导入。
6. 服务端通过同一代理验证这个临时引导 Token，并列出所选 Account 的 R2 Bucket。选择一个 Bucket 后，系统创建只允许该 Bucket 读写的 R2 S3 凭据，等待 Cloudflare IAM 全局生效后验证连接、加密落库，最后自动撤销引导 Token。

每个登录身份拥有独立 Cookie、Profile ID 和代理绑定，不会与另一个 CF 身份共用浏览器状态。自定义 URI 里只有一次性票据，没有代理密码；票据兑换响应禁止缓存且只能读取一次。Profile 元数据只记录脱敏的代理 endpoint，不记录代理认证信息。

> Connector 1.x 曾通过 Playwright 启动浏览器。Cloudflare 的生产验证明确不支持自动化浏览器，可能在登录时显示 `There was a problem with verification`。Connector 2.0 虽已移除 Playwright，但 Edge 仍可能根据 Windows 登录自动绑定本机 Microsoft 账号。Connector 2.1 因此改为全新的 Chrome-only Profile，并使用独立本机控制页；不再加载扩展，也不观察或修改 Cloudflare 登录页。

临时引导 Token 只存在于服务端内存中的短期会话（生产默认 20 分钟），不写数据库或日志。完成选择后，系统使用它创建一个仅限目标 Bucket 的 Account-owned R2 Token，将 R2 Access Key / Secret 和代理整体加密保存，然后通过 Cloudflare API 撤销引导 Token 并从内存清除。创建后端失败时，刚创建的 Bucket Token 会自动撤销，引导 Token 保留到当前会话结束以便重试；若放弃流程，应在 Cloudflare Account API Tokens 页面手动撤销 `S3 Center temporary onboarding`。

同一身份要接入更多 Bucket 时，点击账号卡片的 **登录 / 添加 Bucket**，复用原 Profile 与原代理再次创建临时引导 Token 即可。修改账号代理时，系统会为该身份生成新的 Profile ID，防止旧 Cookie 在不同出口间复用。

预填的临时 Account API Token 包含：

- `Account Settings Read`：识别目标 Account。
- `Workers R2 Storage Write`：发现 R2 Bucket，并允许安全委派 Bucket 级权限。
- `Account API Tokens Write`：创建和撤销 Bucket 级 R2 Token，以及撤销引导 Token 自身。

Cloudflare 的公共 OAuth scope 目录当前不暴露 `Account API Tokens Write`，所以单纯 OAuth 登录无法自动生成长期 R2 S3 凭据；这也是采用“官方预填页面 + 一次性引导 Token”的原因。Cloudflare Access 不是本流程依赖，管理端仍使用自身登录会话与 CSRF 防护；如需企业身份门禁，可在管理域名前额外叠加 Access。

### 登录验证仍失败时

先确认安装输出明确显示 `Connector 2.1`，关闭旧版打开的所有窗口，并从管理端创建一条全新的接入会话（旧 ticket 不能重复使用）。新窗口应为 Chrome，Profile 名称以 `S3 Center` 开头，并且不显示本机浏览器账号。仍失败时先用直连测试；若直连正常而某个代理失败，说明该代理可能在登录过程中轮换 IP、拦截 TLS/挑战脚本，或出口信誉不足，应更换稳定代理。隔离 Profile 不应安装广告拦截、脚本拦截、指纹防护或自动切换代理的扩展。

### 创建桶级凭据时报策略复用错误

Cloudflare 创建 Token 时，`policies[].id` 是服务端生成的响应字段，不应在新建请求中自行提供；否则会返回 `Reusing existing policies is not supported by token creation`。当前实现只提交新的 `effect`、`permission_groups` 与 Bucket `resources`，由 Cloudflare 分配策略 ID。

### 创建后首次验证返回 401

Cloudflare R2 的 IAM 权限变更采用最终一致模型，新创建的 API Key 可能需要数秒、最长约 1 分钟才能在 S3 网关全球生效。当前实现会在同一次接入请求内保留刚生成的凭据，对 `HeadBucket` 的 `401 Unauthorized`/`403 AccessDenied` 自动重试最长 75 秒；期间管理端会保持“正在验证”。只有超时或出现非权限类错误时才判定失败并撤销刚创建的 Bucket Token，因此不要在第一次 401 时重复点击或另建 Token。

## 备选：手动凭据接入

不便安装 Connector 或账号不允许 Token 管理权限时，仍可使用手动模式。

### 接入前准备

1. 在 Cloudflare Dashboard 进入目标 Account 的 R2。
2. 创建目标 Bucket；管理端不会代替 Cloudflare 创建 Bucket。
3. 创建只授权目标 Bucket 的 **Object Read & Write** R2 API Token。
4. 记录 Account ID、R2 Access Key ID 和 Secret Access Key。

管理端需要的是 R2 S3 凭据，不是普通 Cloudflare API Token，也不是 Global API Key。Secret 只在提交时出现，服务端会用 AES-256-GCM 整体加密账号定义后写入数据库，列表和日志都不会返回它。

### 在管理端添加

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

提交后服务端会通过所填代理对目标 Bucket 执行 `HeadBucket`。只有 endpoint、代理、凭据和 Bucket 权限全部验证成功后，定义才会加密保存并立即加入读写调度。手工填入的既有凭据不会套用“新 Token 等待传播”重试；若这里直接返回 401，应检查 Access Key、Secret 和 Account ID。

可用代理示例：

```text
http://203.0.113.10:8080
http://user:password@203.0.113.10:8080
socks5://127.0.0.1:1080
socks5h://user:password@proxy.example:1080
```

每个 Backend 使用独立连接池和代理，不影响其他账号。同一 Cloudflare Account 下如果接入了多个 Bucket，管理端的 **设置 API 代理** 会把新出口作为账号级策略，一次应用到该账号的全部 Backend。管理端只显示脱敏后的 `scheme://host:port`。

### 在线修改账号代理

在 Cloudflare 额度卡中点击 **设置 API 代理**，填写新的代理地址并提交。服务端按以下顺序执行：

1. 使用候选代理为该 Account 下每个 Bucket 创建独立客户端。
2. 逐个调用 R2 S3 API `HeadBucket` 验证代理、凭据和 Bucket 权限。
3. 全部通过后，加密保存所有 Backend 定义并逐个原子热替换运行时客户端，不需要重启。
4. 任一 Bucket 验证或持久化失败时，整体保留原代理和原客户端。

输入留空并提交表示恢复服务器直连。如果代理包含用户名、密码，修改时必须重新输入完整地址；系统不会把已保存的代理认证信息返回浏览器。

YAML 中的初始 Backend 也可以在线修改。首次成功修改时，系统会把该定义作为加密托管定义写入数据库；之后启动时数据库版本覆盖同名 YAML 种子，因此代理不会在重启后丢失。页面会把它标记为“动态托管”。

> 索引模式下，代理只约束 S3 Center 发起的 `HeadBucket`、`HEAD`、`DELETE` 和 Multipart 控制调用。文件 PUT/GET 使用 R2 预签名 URL，由第三方客户端直接连接 R2，不经过 S3 Center 的代理。如果 R2 凭据策略强制固定来源 IP，第三方客户端本身也必须使用获准出口；否则该账号不适合加入零代理数据平面。

## 暂停和恢复账号写入

Cloudflare 额度卡提供 **暂停写入 / 恢复写入**。这是 Account 级开关：同一 Account ID 下的所有已接入 Bucket 会一起切换，不需要重启服务。

暂停后的行为：

- 新建直传、标准 S3 写入和自动副本选址不会再把该账号作为目标；其他可写账号继续服务。
- 已有对象仍可列举、生成临时读取链接和删除，暂停不会迁移或复制任何现有对象。
- 暂停状态与账号凭据一起加密保存，容器或服务器重启后仍然生效；恢复后重新加入调度，但仍需通过健康、额度和路由策略检查。
- 暂停前已经返回给调用方的 R2 预签名 URL 无法由 S3 Center 提前撤销，会一直有效到响应中的 `expires_at`。需要立即阻断这些 URL 时，必须在 Cloudflare 撤销对应 R2 Token，但这也会同时中断该账号上的正常读取和删除。
- 账号暂停期间继续通过“登录 / 添加 Bucket”接入的新 Bucket 会继承暂停状态，不会意外进入调度。

当所有账号均暂停或没有其他账号能容纳声明大小时，新建上传返回 `507 insufficient_capacity`。调用方应稍后重试，不应绕过控制 API 直接写入某个 R2。

## 临时读取链接有效期

管理端顶部的 **临时链接** 按钮显示当前时效，并可在弹窗中设置 1–168 个整小时。产品未配置时默认 3 小时，本生产部署的初始配置为 12 小时；管理端保存的值写入 PostgreSQL、立即生效并覆盖 YAML 初始值，重新部署或重启不会丢失。

修改只影响之后通过 `/v1/objects/presign` 签发的 GET/HEAD URL，不改变上传 PUT 或 Multipart 会话，也不会改变已经签发链接的截止时间。文件仍由调用方直连实际 R2/S3；准确截止时间以每次响应的 `request.expires_at` 为准。

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

## 按创建时间自动清理

管理端 **按创建时间自动清理** 区域可以为一个逻辑 Bucket 或其目录设置小时级保留期：

1. 选择逻辑 Bucket，例如 `assets`。
2. 可选填写目录前缀，例如 `temp/render-cache`；留空表示整个 Bucket。
3. 填写创建后保留小时数（1–87,600）。
4. 确认永久删除影响后点击 **创建并启用策略**。

策略保存在 PostgreSQL，服务重启、容器重建和 GitHub Actions 重新部署都不会丢失。可以随时暂停、恢复或删除策略；删除策略本身不会删除尚存对象，恢复策略时已经过期的对象可能在下一轮扫描立即被清理。

生命周期任务每 5 分钟扫描一次，以 S3 Center 索引中的逻辑对象 `created_at` 为基准。到期删除会定位对象的全部实际 R2/S3 副本，通过各 Backend 配置的代理直接调用上游删除 API，并同步释放索引额度。失败的物理删除进入 Cleanup Queue 重试，文件内容不会下载或经过 S3 Center。覆盖写入会产生新的对象版本与创建时间，因此从覆盖成功时重新计算保留期。

该功能只处理已进入 S3 Center 索引、且匹配所选逻辑 Bucket/目录的对象，不会扫描或删除绕过系统写入的未索引对象。对象一旦在上游删除，之前签发的临时访问 URL 即使尚未到期也会返回上游 `404`；若上游删除暂时失败，则 URL 可能在重试成功前继续可用。

管理员也可以继续使用兼容的静态 YAML 规则：

```yaml
lifecycle:
  batch_size: 100
  rules:
    - prefix: "assets/temp/"
      expiration_hours: 12
    - prefix: "assets/archive/"
      expiration_days: 30
```

每条静态规则只能设置 `expiration_hours` 或 `expiration_days` 其中之一；同一完整前缀不能同时由 YAML 和管理端重复管理。

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

cloudflare_oauth:
  enabled: true
  redirect_url: "https://s3-center.example.com/ui/"
  session_ttl: "20m"
  connector_scheme: s3center
```

配置段沿用 `cloudflare_oauth` 名称以兼容已有部署，但当前快速接入不需要 OAuth Client ID、Client Secret、发布者域名验证或 Cloudflare Access。`redirect_url` 用作可信公网基址来生成一次性 Connector ticket/report URL，不能从请求的 `Host` 头动态推导。

`DYNAMIC_BACKEND_MASTER_KEY` 至少 32 字节，必须只保存在服务器 Secret/`.env` 中，不得提交到 Git。它是解密所有动态账号定义的根密钥：

- 首次启用时生成高熵随机值，例如 `openssl rand -hex 32`。
- 数据库备份必须与该密钥备份配套；丢失密钥后动态账号凭据无法恢复。
- 不要直接替换现有密钥。当前版本没有在线密钥轮换流程，替换后服务会因无法解密已有定义而拒绝启动。

数据库迁移会自动创建 `managed_backends` 表。动态定义在重启后自动解密、验证并恢复到运行时；启动期间不会再次联网执行 `HeadBucket`，但正常健康检查和首次业务请求仍会反映连接错误。

## 当前边界

- 当前管理端支持隔离浏览器快速新增、手动凭据新增、查看、按账号暂停/恢复写入以及在线修改代理；暂不提供直接删除账号。需要回收账号时，应先在 Cloudflare 撤销 Token，并按维护流程迁移对象后更新数据库/配置；不要直接删除仍承载对象的 Backend。
- 页面不调用 Cloudflare Billing API，因此不会显示 Cloudflare 全账号的权威 Class A/Class B 或来自其他工具的用量。
- 一个 Cloudflare Account 的免费额度由 Account 下的所有 R2 Bucket 共享。为同一个 Account 新增 Bucket 不会新增 10 GB；只有接入另一个独立且合规授权的 Account 才会形成另一张免费额度卡。

Cloudflare 官方参考：[受支持的 Challenge 浏览器](https://developers.cloudflare.com/cloudflare-challenges/reference/supported-browsers/)、[API Token 预填模板 URL](https://developers.cloudflare.com/fundamentals/api/how-to/account-owned-token-template/)、[通过 API 创建 Token](https://developers.cloudflare.com/fundamentals/api/how-to/create-via-api/)、[S3 API 凭据与 endpoint](https://developers.cloudflare.com/r2/api/tokens/)、[R2 定价与免费额度](https://developers.cloudflare.com/r2/pricing/)。
