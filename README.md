# Codex 降智测试（CLIProxyAPI 插件）

> 本仓库是 company 二开版，新增自动指纹巡检。公司版构建目前面向 Linux/amd64；从 [公司版 Releases](https://github.com/qinghua362330/cpa-plugin-codex-candy-eval/releases/latest) 获取，插件源为 [registry.company.json](https://raw.githubusercontent.com/qinghua362330/cpa-plugin-codex-candy-eval/main/registry.company.json)。[持续同步与构建说明](docs/company-upstream-sync.md)。下方原作者安装脚本保留作上游参考，运行它们会安装上游版，而不是本二开版。

在 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 管理面板中用**糖果题**、**指纹检测**和**画鹈鹕**测试 Codex 凭证。

![example](docs/images/example.png)

## 安装

### CPA 版本要求

最低要求 CPA [v7.3.3](https://github.com/router-for-me/CLIProxyAPI/releases/tag/v7.3.3)。

### 插件商店

CPA 管理面板（CPAMC 或 CPAMP）插件商店搜索安装 `cpa-codex-candy-eval`，并在面板左侧打开「Codex 降智测试」。

### 人工安装

进入 CPA 工作目录，执行对应的安装命令。macOS 和 Linux：

```sh
curl -fsSL https://raw.githubusercontent.com/haowang02/cpa-plugin-codex-candy-eval/main/install.sh | sh
```

Windows 请先停止 CPA，再在 PowerShell 中运行：

```powershell
irm https://raw.githubusercontent.com/haowang02/cpa-plugin-codex-candy-eval/main/install.ps1 | iex
```

也可以从 [Releases](https://github.com/haowang02/cpa-plugin-codex-candy-eval/releases/latest) 下载对应平台的压缩包，解压后将插件文件重命名为 `cpa-codex-candy-eval-v<版本号>.<扩展名>`，放入对应的 `plugins/<系统>/<架构>/` 目录。

在 CPA 的 `config.yaml` 中启用插件：

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    cpa-codex-candy-eval:
      enabled: true
```

## 使用

在「凭证」标题右侧选择类型，默认「认证文件 · codex」，也可选择「全部凭证」。点击凭证旁的按钮单独测试，或勾选多个凭证批量测试。未勾选时，批量操作针对当前类型下的全部可用凭证。

### 糖果题

选择模型、推理强度和每凭证次数，点击「测试」。推理强度选择 `none` 时由 CPA 决定。

正确答案为 **21**。展开凭证可查看历次回答，正确率按当前选择的模型和推理强度统计。

### 指纹检测

选择模型，点击凭证旁的「测试」，或勾选凭证批量测试。插件从参考库的 36 道数字挑战中随机抽取 3 道，与参考库中的模型比对，显示最接近的模型、家族及归因概率。测试 gpt-6.1-sol 时命中 gpt-6-astra/6.1-sol，测试 deepseek-flash 时命中 deepseek-v4.1-flash-1017，均视为一致。展开凭证可查看历史记录和详情。

### 自动指纹巡检（company.1）

「指纹检测」页新增「自动巡检」，与下方独立的糖果巡检脚本无关：

1. 选择参考库支持的巡检模型，设置间隔（默认 30 分钟）。
2. 选择全部已启用 Codex 认证文件，或勾选凭证后使用「指定凭证」。
3. 保存配置，可先「立即巡检一次」，查看匹配结果，再开启定时巡检。
4. 若要自动停用，先在「本地 CPA 管理连接」填写 CPA 容器内可访问的本机管理地址和管理密钥，再开启「不匹配时自动停用凭证」。默认定时巡检和自动停用均关闭。

每轮串行检测，每个凭证仍使用原有三道挑战；任务不会重叠，超时错过的时间点跳过，不积压补跑。冷却、已禁用、正在测试的凭证会跳过；配置型 API Key 不参与自动禁用。修改配置会取消旧任务；「停止本次」只停止当前任务，不关闭后续定时巡检。

仅完整有效的三题归因结果不匹配时执行停用，错误、超时、取消、有效题数不足不会触发。已有模型别名按相同模型处理。指纹是统计推断，不是上游型号证明，仍可能误判；本版按完整检测的最终结果执行，不额外设置置信度阈值或二次确认。

**停用影响该凭证的所有模型，而不是只停用测试模型。** 不删除凭证，不主动中断在途业务请求，也不会自动恢复。先用少量指定凭证观察，再决定是否开启自动停用；测试会消耗额度，每凭证正常完成需要三次请求，失败可能重试。

配置与最近 200 条巡检事件保存在测试历史文件旁的 `*.automation.json`，刷新或重载后恢复；清空原测试历史不删除巡检配置。页面显示下次执行、最近执行时间和最近 50 条事件。配置损坏或审计记录不能落盘时，不执行自动停用。

自动停用使用 CPA `PATCH /v0/management/auth-files/status`，请求只包含凭证名称、索引和 `disabled: true`，不回写整份 Token JSON。CPA v8.0.51 此接口使用带注册代次校验的 `authManager.Modify`。插件在测试前记录认证文件摘要，停用前核对身份及摘要；测试期间刷新或替换过的凭证保守地跳过，不用旧结论停用新内容。最后一次核对与服务端查找之间仍有短暂竞争窗口，操作期间不应同时人工替换同名认证文件。

本机管理地址只接受数字回环 IP（如 `http://127.0.0.1:8317`），必须是 CPA 容器内监听端口，不是宿主机映射端口。请求不走代理、不跟随重定向。管理密钥由用户显式填写，单独保存在 `*.management.json`（权限 `0600`），不会返回页面、写入巡检事件或保存到浏览器。配置备份含此密钥，应限制访问。撤销密钥或删除该文件后，自动停用会失败关闭，不回退为全量凭证重写。

回滚插件版本不会自动重新启用已停用凭证；应逐个核对后手动恢复。此功能按 CPA v8.0.51 的接口开发，旧版 CPA 需另行验证。不要直接执行上游 `scripts/deploy.sh` 更新生产：该脚本会重启 CPA；本次二开应先备份，再按实际环境安排安装。

### 画鹈鹕

选择模型、推理强度和版本，点击「测试」。

- **原版**：模型写出一个用 SVG 绘制鹈鹕骑自行车动画的 HTML 页面。
- **多合一版**：模型画一张 SVG 图片，鹈鹕骑车在孙悟空驾驶的飞机机翼上，翅膀托着秦始皇骑的北极熊，并喊出糖果题的答案。

列表和历史记录显示缩略画面，点击即可放大查看，动画在放大后播放。

### 记录与额度

- 测试会消耗凭证额度。每次请求都以新会话的完整 Codex CLI 上下文发出，输入约 1.4 万 Token。
- 每个凭证的每种测试都保留最近 20 次记录，刷新或重启后仍可查看。
- 点击标签栏右侧的垃圾桶可清空全部测试记录；凭证列表右上角的垃圾桶只清空当前测试的记录。
- 单次请求有时间限制：指纹检测 3 分钟，糖果题和画鹈鹕 10 分钟。超时的请求会被中断。
- 请求中断、超时、上游服务出错，或回答无法用于测试（如指纹检测的有效数字不足、画鹈鹕的回答中没有代码）时，插件会重新发送该请求，同一请求最多发送 3 次。凭证冷却、额度用尽等请求被拒绝的情况不重试，糖果题答错也不重试。耗时和 Token 包含所有重试。

### 定时巡检

巡检脚本定时用糖果题测试账号，默认跳过 Free 订阅。每个账号先测一次，状态变化须经两次复测确认；降智账号恢复前的复测等待 240 秒。开启自动调整后，脚本改变 priority，并通过 ntfy 或 Bark 通知恢复或降智。部署方法见 [定时糖果巡检](docs/inspection.md)。

也可以补全下面的提示词，发给 Codex、Claude Code 等 AI 助手，让它帮你部署：

```text
请按照 https://raw.githubusercontent.com/haowang02/cpa-plugin-codex-candy-eval/main/docs/inspection.md 中的教程，在这台机器上部署 Codex 降智测试插件的定时巡检。

我的配置（没写的项保持脚本默认值）：
- CPA 地址：
- CPA 管理密钥：
- 巡检频率：（例如每 30 分钟）
- 糖果测试模型和推理强度：（例如 gpt-6.1-sol、low）
- 巡检范围：（例如所有 codex- 开头的认证文件）
- 是否自动调整 priority：（是或否，可指定降智和正常账号的 priority）
- 通知方式：（ntfy 的服务器地址、主题和账号密码，Bark 的推送地址，或不通知）

部署后先手动运行一次，确认巡检能正常完成，再配置 crontab，最后告诉我日志的位置。缺少必要信息时先问我。
```

## 致谢

- [codex-candy-eval](https://github.com/haowang02/codex-candy-eval)
- [LM Fingerpoint Detector](https://github.com/Ikaleio/lm-detector) — 指纹检测使用的数字指纹归因算法与参考库（MIT）
- [LINUX DO](https://linux.do/) - 新的理想型社区
