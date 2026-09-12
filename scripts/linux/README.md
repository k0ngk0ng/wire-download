# Linux 验证环境

使用 Debian 12 / glibc 2.36，目标首先为原生 arm64；amd64 需另外构建和运行。
Docker 镜像只用于构建和测试，终端用户安装包不需要 Docker。

为避免覆盖 macOS 构建缓存，把对应源码归档展开到仓库内的独立目录：

```sh
mkdir -p .cache/linux-validation
tar -xzf dist/wire-download-0.1.2-sources.tar.gz -C .cache/linux-validation
docker build -t wirectl-download-build:go1.26.2-bookworm scripts/linux
docker run --rm \
  --mount "type=bind,src=$PWD/.cache/linux-validation/wire-download-0.1.2-sources,dst=/work" \
  -e VERSION=0.1.2 \
  wirectl-download-build:go1.26.2-bookworm
```

构建完成后，需在同一环境安装 Linux 归档并运行 `scripts/test-install.py`。
此外运行 `python3 scripts/test-ed2k.py /absolute/install/prefix --address <容器本机IPv4>`。
它启动两个安装包内的真实 aMule peer 和一个仅接受本机连接的测试服务器，
核对下载状态及文件字节，并在退出时停止 daemon、留下 `result.json` 和日志。
仅构建成功或 HTTP 验收通过不能替代这个 ED2K 验证。

上述 Docker 命令会写入 Docker/OrbStack 管理的镜像、容器存储，不限于仓库目录。
当前用户的文件系统规则要求在首次执行这些操作前获得明确授权。
