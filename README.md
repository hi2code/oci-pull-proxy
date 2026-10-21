# OCI Pull Proxy

[![CI](https://github.com/hi2code/oci-pull-proxy/actions/workflows/ci.yml/badge.svg)](https://github.com/hi2code/oci-pull-proxy/actions/workflows/ci.yml)

带账号密码认证的 OCI 镜像拉取代理。在原镜像地址前加代理域名即可使用，无需配置 `registry-mirrors`。默认支持 Docker Hub、GHCR、Quay，自动申请和续期 HTTPS 证书，镜像缓存上限 40 GiB，仅支持拉取。

镜像：`ghcr.io/hi2code/oci-pull-proxy:latest`，支持 Linux amd64/arm64。

## 部署

准备一台安装 Docker 的 Linux 服务器，将域名 A/AAAA 记录指向它，开放 TCP 80/443，并允许出站 HTTPS。以下以 `x.example.com` 为例，请替换为自己的域名。使用 Cloudflare 橙云时，见下方的 [Cloudflare 模式](#cloudflare-模式)。

创建 `.env`，填写真实账号和长随机密码：

```dotenv
PROXY_DOMAIN=x.example.com
PROXY_USERNAME=myuser
PROXY_PASSWORD=请替换为长随机密码`
```

```sh
chmod 600 .env
docker run -d \
  --name oci-pull-proxy \
  --restart unless-stopped \
  --stop-timeout 30 \
  --env-file .env \
  -p 80:8080 -p 443:8443 \
  -v oci-cache:/data/cache \
  -v oci-acme:/data/acme \
  --read-only \
  --tmpfs /tmp:size=64m,noexec,nosuid \
  --cap-drop ALL \
  --security-opt no-new-privileges:true \
  ghcr.io/hi2code/oci-pull-proxy:latest

docker logs -f oci-pull-proxy
```

看到“HTTPS 已就绪”后即可使用。`oci-cache` 保存镜像缓存，`oci-acme` 保存证书和 ACME 账户；更新时复用原卷，不要删除证书卷。同一组目录只供一个实例使用；自定义宿主机目录需保证 UID/GID 65532 可写。

也可在仓库根目录创建 `.env` 后运行 `docker compose up -d`。Compose 的卷名带项目名前缀，从 `docker run` 切换时需明确复用原卷。

## 拉取镜像

```sh
sudo docker login x.example.com
sudo docker pull x.example.com/getmeili/meilisearch:nightly
sudo docker pull x.example.com/nginx:latest
sudo docker pull x.example.com/ghcr.io/pgdogdev/pgdog:sha256-dec8fc0568a1e55a01ff77fac2944aa92801db7bdccf313382c417e4d191e434
```

登录时使用 `.env` 中的账号密码。拉取后的本地镜像名保留代理域名前缀，`docker run` 和 Compose 也使用该名称，例如：

```sh
docker run --rm -p 8080:80 x.example.com/nginx:latest
```

| 代理域名后的路径 | 实际上游镜像 |
| --- | --- |
| `nginx:latest` | Docker Hub `library/nginx:latest` |
| `getmeili/meilisearch:nightly` | Docker Hub `getmeili/meilisearch:nightly` |
| `docker.io/nginx:latest` | Docker Hub `library/nginx:latest` |
| `ghcr.io/owner/image:tag` | GHCR `owner/image:tag` |
| `quay.io/org/image:tag` | Quay `org/image:tag` |

不带仓库域名时默认 Docker Hub，单段镜像名自动补 `library/`。首段包含点、冒号或为 `localhost` 时视为显式仓库，必须已配置，否则返回 404，不会动态访问或回退到 Docker Hub。标签、`@sha256:…` digest 和嵌套路径保持原意。

支持配置凭据后的私有镜像。SHA-256 digest 寻址的镜像层和 manifest 会缓存，tag manifest 实时拉取；同一镜像的简写、`library/` 和 `docker.io/` 写法共享缓存。

## 配置

优先级：显式环境变量 > `CONFIG_FILE` JSON > 默认值。显式空环境变量也会覆盖 JSON。认证必须启用，用户名、密码不能为空或使用示例占位值。

| 环境变量 | 默认值 / 用途 |
| --- | --- |
| `PROXY_DOMAIN` | auto/cloudflare 模式必填，单个域名，不含协议或端口 |
| `PROXY_USERNAME` / `PROXY_PASSWORD` | 代理登录账号密码 |
| `TLS_MODE` | `auto`、`manual` 或 `cloudflare`；配置完整证书路径时默认 manual，否则 auto |
| `TLS_CERT` / `TLS_KEY` | 手动模式的容器内证书和私钥路径 |
| `HTTPS_LISTEN` / `HTTP_LISTEN` | `:8443` / `:8080`；修改时同步调整端口映射 |
| `CACHE_DIRECTORY` / `CACHE_MAX_SIZE_GB` | `/data/cache` / `40` |
| `ACME_DIRECTORY` | `/data/acme`，不能与缓存目录相同或互相嵌套 |
| `ACME_ACCEPT_TOS` | `true`，自动 HTTPS 要求接受 Let's Encrypt 服务条款 |
| `ACME_EMAIL` | 可选，ACME 联系邮箱 |
| `ACME_DIRECTORY_URL` | 留空使用 Let's Encrypt 生产目录 |
| `CONFIG_FILE` | 可选，容器内 JSON 路径；未设置时不读取 JSON |

### 私有镜像与自定义仓库

创建 `config.json`，只填写需要覆盖或新增的条目。例如为 GHCR 配置凭据，并添加自定义仓库：

```json
{
  "registries": {
    "ghcr.io": {
      "upstream": "https://ghcr.io",
      "username": "your-github-user",
      "password_env": "GHCR_TOKEN"
    },
    "registry.example.com:5000": {
      "upstream": "https://registry.example.com:5000"
    }
  }
}
```

将 `GHCR_TOKEN` 加入 `.env`，并在启动命令的镜像名之前增加：

```sh
-e CONFIG_FILE=/etc/oci-pull-proxy/config.json \
-v "$(pwd)/config.json:/etc/oci-pull-proxy/config.json:ro"
```

确保文件存在且 UID 65532 可读。`username` 与 `password_env` 必须成对配置，密码变量不能为空。客户端密码不会转发上游，上游凭据由服务端独立配置。

仓库键必须是小写域名或域名加端口（1–65535），也支持 `localhost`；上游 URL 必须使用 HTTPS。默认键为 `docker.io`、`ghcr.io`、`quay.io`，其中 Docker Hub 上游为 `https://registry-1.docker.io`。JSON 条目覆盖同名默认条目，其他默认条目保留。完整 JSON 示例见 [config.example.json](config.example.json)。

### 手动证书

在 `.env` 中设置以下变量，并在启动命令中增加 `-v "$(pwd)/certs:/certs:ro"`：

```dotenv
TLS_MODE=manual
TLS_CERT=/certs/fullchain.pem
TLS_KEY=/certs/privkey.pem
```

证书和私钥须可被 UID 65532 读取。手动证书需自行续期并重启容器载入，配置域名时证书必须匹配该域名。

### Cloudflare 模式

链路为客户端 HTTPS → Cloudflare → 源站 HTTP:80，不申请源站证书，无需 Cloudflare Token。

1. 将域名指向源站并开启橙云，为该域名选择 Flexible 回源，在边缘将 HTTP 跳转到 HTTPS。
2. 为该域名绕过 Cloudflare 缓存，避免交互式验证码或浏览器登录页影响 Docker 请求。
3. 源站开放 TCP 80，允许出站 HTTPS；应用必须能看到 Cloudflare 的实际 TCP 来源。
4. 使用上面的启动命令，增加 `-e TLS_MODE=cloudflare`，删除 `-p 443:8443` 和 `-v oci-acme:/data/acme`；从 `.env` 删除 `TLS_CERT`、`TLS_KEY`。

使用 Compose 时执行 `docker compose -f compose.cloudflare.yaml up -d`。已有容器需先停止并重建，沿用原缓存卷，保留原证书卷以便回退；切回 auto/manual 时同步恢复回源方式和端口映射。

程序仅放行内置 Cloudflare 网段，并检查 Host 与 HTTPS 转发头，镜像接口仍要求账号密码。本机直连（包括健康检查）返回 403；额外反向代理或改写来源地址的网络也可能被拒绝。通过公网域名访问 `/readyz` 验证就绪。Cloudflare 到源站的 HTTP 包含认证信息，使用前需接受这一网络边界，并确认套餐的大文件、流量和超时限制适合实际镜像。

## 更新与迁移

使用 `docker run` 部署时，先拉取新镜像，再重建容器：

```sh
docker pull ghcr.io/hi2code/oci-pull-proxy:latest
docker stop -t 30 oci-pull-proxy
docker rm oci-pull-proxy
```

重新执行原启动命令，复用相同配置、模式和卷名。修改 `.env` 后同样需要重建，单纯 `docker restart` 不会载入新的环境变量。Compose 部署使用 `docker compose pull && docker compose up -d`，Cloudflare 模式两条命令都加 `-f compose.cloudflare.yaml`。

旧版 `registries` 配置键须迁移，否则启动报错：

| 旧键 | 新键 |
| --- | --- |
| `dockerhub` | `docker.io` |
| `ghcr` | `ghcr.io` |
| `quay` | `quay.io` |

自定义短键改为实际仓库域名，保留 `upstream`、`username` 和 `password_env` 字段。同步更新脚本与 Compose 镜像名：`代理域名/dockerhub/library/nginx:latest` 改为 `代理域名/nginx:latest`，`代理域名/ghcr/team/image:tag` 改为 `代理域名/ghcr.io/team/image:tag`。

旧短前缀不再是别名，`ghcr/team/image` 会按 Docker Hub 仓库路径处理。现有缓存无需主动删除，上游 URL 和规范路径未变的缓存可继续复用。

## 排障

```sh
docker logs --tail=100 oci-pull-proxy
docker exec oci-pull-proxy /oci-pull-proxy check
docker exec oci-pull-proxy /oci-pull-proxy version
curl -fsS https://x.example.com/readyz
```

`check` 检查本地配置、文件与目录权限，不代表 DNS 或公网访问正常。容器无法启动时，可用原环境变量及卷运行临时容器，在镜像名后追加 `check`，无需映射端口。

| 现象 | 检查项 |
| --- | --- |
| 证书获取或续期失败 | 域名 A/AAAA、TCP 80/443、防火墙、出站 HTTPS、CAA 和服务器时间；不要删除证书卷 |
| 401 | 代理登录账号密码；私有镜像另需配置上游凭据 |
| 403（Cloudflare 模式） | 是否经 Cloudflare HTTPS 访问，实际 TCP 来源、Host 和转发协议是否正确 |
| 404 | 镜像路径是否正确，显式仓库域名是否已配置 |
| 502 | 日志中的上游网络、地址或 token 服务错误 |
| 缓存写入失败或拉取中断 | 磁盘空及 UID 65532 的目录写入权限 |
| `/readyz` 返回 503 | 初始化或证书尚未就绪，查看启动日志 |

## 开发与发布

使用 Go 1.27 或更新版本：

```sh
go test ./...
go test -race ./...
go vet ./...
go build ./...
```

测试使用本地模拟上游与 CA，证书失败重试测试约需一分钟。容器验证使用 `docker build -t oci-pull-proxy:verification .` 和 `python3 scripts/container-smoke.py`。

推送 main 后，GitHub Actions 通过 Go 检查及容器测试才会发布 `latest` 和 `sha-完整提交哈希` 镜像；推送 `v1.2.3` 标签发布 `1.2.3`，不改动 `latest`。首次发布需在 GHCR 包设置中设为 Public，才能匿名拉取。需要可重复部署时固定镜像 digest。
