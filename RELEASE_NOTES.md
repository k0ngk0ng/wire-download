# wirectl download 0.1.1

通过 `wirectl download` 管理 HTTP/HTTPS、torrent、magnet 和 ed2k 下载。独立 daemon 持续下载，终端仪表盘显示进度、速度和任务状态，支持暂停、恢复及重启恢复。

- 提供 macOS/Linux、amd64/arm64 安装包；每个包包含 wirectl、下载插件和 aria2/aMule 引擎，无需另装这些依赖。macOS 要求 13+，Linux 要求 glibc 2.36+。
- 支持在独立 Chromium 浏览器中登录并保存网站 Cookie，通过 `login --remote user@server` 可向无桌面的 Linux 传入会话。此可选功能需要桌面端已有 Chromium 浏览器，使用 `--remote` 时还需要 SSH 客户端；不自动提取 localStorage/Bearer token。
- 下载器的 `logout` 仅删除已保存会话，不注销网站或清除浏览器配置。
- 附带 SHA256SUMS 与对应源码归档；源码归档无需下载安装。

安装说明及命令见 README.md。Release 由四个平台的原生构建与协议集成测试通过后发布。该私有仓库的安装包仅对已授权用户可见。
