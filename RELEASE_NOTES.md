# wirectl download 0.2.1

新增电驴、BT 和磁力关键词搜索，使用 `wirectl download search <关键词>`。交互终端显示各来源进度及结果，选择结果后按 Enter 下载；也支持 JSON 输出与按结果 ID 下载。

- 电驴通过 aMule EC 协议获取准确的文件哈希、大小和来源，支持当前服务器、全局服务器与 Kad 搜索。多份 HTTPS `server.met` 并发获取、验证并按 IP/端口合并。
- 默认网站索引：Nyaa、Anime Tosho、动漫花园、BTDig、LinuxTracker。支持按 info hash 去重、保留磁力与种子地址、来源失败时保留其他结果；可添加 RSS/Torznab 来源并管理启停。Internet Archive API 为可选来源，默认关闭。
- 搜索任务可取消；与后台下载独立运行。搜索结果仅保存在 daemon 内存中，重启后清除。
- 新配置默认下载到 `~/Downloads`，升级保留已有下载目录及配置。

提供 macOS arm64、Linux arm64 和 Linux amd64 安装包；当前不提供 macOS Intel（amd64）安装包。每个包包含 wirectl、下载插件及 aria2/aMule 引擎，无需额外安装下载或搜索依赖。macOS 要求 13+，Linux 要求 glibc 2.36+。可选浏览器登录需要桌面端已有 Chromium 浏览器；远端传入会话需要 SSH 客户端。

发布流程以三个平台的原生构建、单元测试、实际终端操作、HTTP 登录下载、BT 搜索选中后的真实 peer 传输与重启恢复、电驴搜索及真实 peer 传输作为门槛。公共网站可用性会变化，网站连接失败会在搜索界面显示。

`wire-download` 公开仓库的 GitHub Release 提供安装包、SHA256SUMS 和对应源码归档，任何人均可下载；`wirectl` 源仓库保持私有，但发行包已包含运行所需的 CLI。源码归档无需下载安装。升级前停止 daemon；安装脚本拒绝覆盖现有引擎目录，建议安装至新目录后切换。完整命令和限制见 README.md。
