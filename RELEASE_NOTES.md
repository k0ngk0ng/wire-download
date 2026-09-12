# wirectl download 0.2.2

新增 shell 补全命令：`wirectl download completion bash|zsh|fish`。生成脚本无需初始化配置或启动 daemon，同时支持 `wirectl download` 和 `wirectl-download`。

- 补全下载与搜索子命令、选项、固定枚举值，以及文件和目录路径。
- README 增加 Bash、Zsh、Fish 加载说明，以及查看当前配置和 eMule 服务器列表的方法。
- 修正 README 中 `wirectl` 源仓库链接为 https://github.com/k0ngk0ng/wirectl。

提供 macOS arm64、Linux arm64 和 Linux amd64 安装包；当前不提供 macOS Intel（amd64）安装包。每个包包含 wirectl、下载插件及 aria2/aMule 引擎，无需额外安装下载或搜索依赖。macOS 要求 13+，Linux 要求 glibc 2.36+。可选浏览器登录需要桌面端已有 Chromium 浏览器；远端传入会话需要 SSH 客户端。

发布流程以三个平台的原生构建、单元测试、实际终端操作、HTTP 登录下载、BT 搜索选中后的真实 peer 传输与重启恢复、电驴搜索及真实 peer 传输作为门槛。公共网站可用性会变化，网站连接失败会在搜索界面显示。

`wire-download` 公开仓库的 GitHub Release 提供安装包、SHA256SUMS 和对应源码归档，任何人均可下载；`wirectl` 源仓库保持私有，但发行包已包含运行所需的 CLI。源码归档无需下载安装。升级前停止 daemon；安装脚本拒绝覆盖现有引擎目录，建议安装至新目录后切换。完整命令和限制见 README.md。
