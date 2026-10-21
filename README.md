# OCI Pull Proxy

[![CI](https://github.com/hi2code/oci-pull-proxy/actions/workflows/ci.yml/badge.svg)](https://github.com/hi2code/oci-pull-proxy/actions/workflows/ci.yml)

带账号密码认证的 OCI 镜像拉取代理。在原镜像地址前加代理域名即可使用，无需配置 `registry-mirrors`。默认支持 Docker Hub、GHCR、Quay，自动申请和续期 HTTPS 证书，镜像缓存上限 40 GiB，仅支持拉取。

镜像：`ghcr.io/hi2code/oci-pull-proxy:latest`，支持 Linux amd64/arm64。

## 部署

准备一台安装 Docker 的 Linux 服务器，将域名 A/AAAA 记录指向它，开放 TCP 80/443，并允许出站 HTTPS。以下以 `x.example.com` 为例，请替换为自己的域名。使用 Cloudflare 橙云时，见下方的 [Cloudflare 模式](#cloudflare-模式)。

复制 `.env.example` 为 `.env`（也可直接创建），填写真实账号和长随机密码：

```dotenv
PROXY_DOMAIN=x.example.com
PROXY_USERNAME=myuser
PROXY_PASSWORD=请替换为长随机密码
```

```sh
# 保留当前用户为所有者，允许容器的 GID 65532 写回 TOKEN。
sudo chgrp 65532 .env
chmod 660 .env
docker run -d \
  --name oci-pull-proxy \
  --restart unless-stopped \
  --stop-timeout 30 \
  --env-file .env \
  --mount type=bind,src="$(pwd)/.env",dst=/config/.env \
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


## 拉取镜像

```sh
sudo docker login x.example.com
sudo docker pull x.example.com/getmeili/meilisearch:nightly
sudo docker pull x.example.com/nginx:latest
sudo docker pull x.example.com/ghcr.io/pgdogdev/pgdog:sha256-dec8fc0568a1e55a01ff77fac2944aa92801db7bdccf313382c417e4d191e434
```

登录时使用 `.env` 中的账号密码。拉取后的本地镜像名保留代理域名前缀，`docker run` 也使用该名称，例如：

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

## Docker Hub token 加速

在客户端 `/etc/docker/daemon.json` 中合并以下配置，替换域名和 token，然后重启 Docker：

```json
{
  "registry-mirrors": ["https://x.example.com/m/你的token"]
}
```

随后直接运行 `docker pull alpine:latest`，无需 `docker login`，镜像名保持原样。此入口只支持 Docker Hub 公共镜像，固定匿名访问 Docker Hub，不使用环境变量配置的上游替换或私有仓库凭据；缓存与原账号密码入口隔离。原登录和多仓库代理用法不变。

在 `.env` 中设置 `TOKEN=你自己的随机字符串`（实际值仅允许 `A–Z a–z 0–9 - _`），或直接留空：

```dotenv
TOKEN=
```

未配置或为空时，程序先读取挂载的 `.env`，复用其中已有的非空 `TOKEN`；仍为空才生成 32 字节密码学安全随机数，以无填充 Base64URL 保存（43 字符）。已有的空 `TOKEN=` 会被填入，没有该项则追加。其他配置和注释保持原样，不再使用单独的 Token 文件。

启动后在 `.env` 中查看 `TOKEN`，服务日志不会打印它。普通重启时，即使容器环境变量仍为空，也会读取已写回的值；重建容器时，`--env-file .env` 会直接加载这个值。非空的进程环境变量 `TOKEN` 优先，不读取或修改 `.env`。

Docker 的 `--env-file` 仅传变量，不能写回文件，所以部署示例还将同一份 `.env` 可写挂载到 `/config/.env`。宿主机文件保留当前用户所有权，使用 `sudo chgrp 65532 .env` 和 `chmod 660 .env` 允许容器组读写。程序保留已有文件权限和挂载文件的 inode；本地新建文件默认权限为 `0600`。文件不可读写、Token 无效或存在重复 `TOKEN` 配置时会启动报错。每份 `.env` 仅供一个运行中的实例使用。

更换 Token：停止代理，将 `.env` 的 `TOKEN` 改为新值，或清空让程序重新生成，然后重建容器（单纯 restart 会保留旧环境变量）。再更新客户端的 mirror URL。不要在运行时删除或替换挂载文件。

本地直接运行时，先将 `.env` 中的其他变量导出到进程环境，并在该文件所在目录启动。程序只会读取和保存 `.env` 中的 `TOKEN`，其他配置仍从进程环境读取。外部反向代理或 CDN 的访问日志应对 mirror URL 中的 Token 脱敏。

## 配置

所有配置统一写在 `.env` 中，由 `docker run --env-file .env` 传入。优先级：显式环境变量 > 默认值；显式空值也会覆盖默认值。代理认证始终启用，用户名、密码不能为空或使用示例占位值。常用配置模板见 [.env.example](.env.example)，其余可选项按需添加。

| 环境变量 | 默认值 / 用途 |
| --- | --- |
| `TOKEN` | 可选；留空自动生成并写回 `.env`，用于 Docker Hub 公共 mirror |
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
| `GHCR_USERNAME` / `GHCR_TOKEN` | GHCR 用户名和 Token；均为空时匿名访问 |
| `DOCKERHUB_USERNAME` / `DOCKERHUB_TOKEN` | Docker Hub 用户名和访问 Token |
| `QUAY_USERNAME` / `QUAY_TOKEN` | Quay 用户名和访问 Token |
| `REGISTRY_<NAME>_DOMAIN` | 自定义仓库域名，可带端口 |
| `REGISTRY_<NAME>_UPSTREAM` | 默认 `https://` 加对应 DOMAIN |
| `REGISTRY_<NAME>_USERNAME` / `REGISTRY_<NAME>_PASSWORD` | 自定义仓库账号和密码或 Token；均为空时匿名访问 |

### 私有镜像与自定义仓库

私有 GHCR 镜像只需在 `.env` 填写：

```dotenv
GHCR_USERNAME=your-github-user
GHCR_TOKEN=your-github-token
```

使用具有 `read:packages` 权限的 GitHub personal access token (classic)，Token 所属账号须有目标包的读取权限；组织要求 SSO 时，还需授权该 Token。用户名填写 Token 所属个人账号，即使镜像位于组织名下。参见 [GitHub 官方认证说明](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry#authenticating-with-a-personal-access-token-classic)。

重建代理容器后，客户端继续使用 `PROXY_USERNAME` / `PROXY_PASSWORD` 登录：

```sh
docker login x.example.com
docker pull x.example.com/ghcr.io/your-org/private-image:latest
```

修改 `.env` 后，按下方“更新与重建”步骤停止并删除旧容器，重新执行对应模式的 `docker run` 命令，继续使用 `--env-file .env`。沿用现有 `.env` 挂载即可。客户端密码不会转发上游，上游凭据由服务端独立配置。各仓库的用户名和 Token/密码必须同时填写或同时留空。

自定义仓库也直接写入 `.env`，例如：

```dotenv
REGISTRY_CORP_DOMAIN=registry.example.com:5000
REGISTRY_CORP_USERNAME=robot
REGISTRY_CORP_PASSWORD=your-registry-token
REGISTRY_LAB_DOMAIN=lab.example.com
REGISTRY_LAB_UPSTREAM=https://backend.example.com
```

程序自动识别 `REGISTRY_<名称>_*` 配置，无需额外列表。`CORP`、`LAB` 只是配置组名称，须以大写字母开头，只包含大写字母、数字和下划线。拉取时使用实际域名，例如 `x.example.com/registry.example.com:5000/team/image:latest`。没有填写 `UPSTREAM` 时默认访问 `https://` 加 `DOMAIN`；没有填写凭据时匿名访问。

域名必须小写，可附加端口（1–65535），也支持 `localhost`；上游 URL 必须使用 HTTPS。重复域名会报错。内置仓库固定使用官方地址，通过 `GHCR_*`、`DOCKERHUB_*`、`QUAY_*` 配置凭据。全部留空的自定义配置组会被忽略；填写任一非空参数时须提供 DOMAIN。

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

已有容器需先停止并重建，沿用原缓存卷，保留原证书卷以便回退；切回 auto/manual 时同步恢复回源方式和端口映射。

程序仅放行内置 Cloudflare 网段，并检查 Host 与 HTTPS 转发头，原镜像接口仍要求账号密码，`/m/…` 入口要求 token。本机直连（包括健康检查）返回 403；额外反向代理或改写来源地址的网络也可能被拒绝。通过公网域名访问 `/readyz` 验证就绪。Cloudflare 到源站的 HTTP 包含认证信息，使用前需接受这一网络边界，并确认套餐的大文件、流量和超时限制适合实际镜像。

## 更新与重建

使用 `docker run` 部署时，先拉取新镜像，再重建容器：

```sh
docker pull ghcr.io/hi2code/oci-pull-proxy:latest
docker stop -t 30 oci-pull-proxy
docker rm oci-pull-proxy
```

重新执行原启动命令，复用相同配置、模式和卷名。修改 `.env` 后同样需要重建，单纯 `docker restart` 不会载入新的环境变量。

## 排障

```sh
docker logs --tail=100 oci-pull-proxy
docker exec oci-pull-proxy /oci-pull-proxy check
docker exec oci-pull-proxy /oci-pull-proxy version
curl -fsS https://x.example.com/readyz
```

`check` 检查本地配置、文件与目录权限，不代表 DNS 或公网访问正常。容器无法启动时，可用原环境变量及卷运行临时容器，在镜像名后追加 `check`，无需映射端口。

每个 `/v2/`、`/m/` 请求会输出多行日志：访问行（方法、路径、状态码、字节数、耗时）加上代理决策行（认证、路由、缓存、上游挑战、token、上游响应）。判断卡在哪一步看最后一行，不要只看 Docker 客户端的提示。密码、bearer token、mirror Token 均不会写入日志（只记录长度），mirror URL 中的 Token 记录为 `<token>`，上游重定向只记录 scheme/host/path（不含签名参数），`/healthz`、`/readyz` 仅在非 2xx 时记录。

未登录代理时（客户端只会看到 `not found`）：

```
2026-10-22T00:59:53+08:00 拒绝请求：代理认证失败：GET /v2/ghcr.io/owner/image/manifests/v1 来自 1.2.3.4（客户端需 docker login x.example.com）
2026-10-22T00:59:53+08:00 GET /v2/ghcr.io/owner/image/manifests/v1 401 24 0s
```

已登录但上游拒绝：

```
2026-10-22T01:00:02+08:00 代理请求：GET /v2/ghcr.io/owner/image/manifests/v1 -> ghcr.io/v2/owner/image/manifests/v1（客户端 1.2.3.4）
2026-10-22T01:00:02+08:00 上游要求认证：ghcr.io https://ghcr.io/v2/owner/image/manifests/v1 -> 401，挑战 bearer realm=https://ghcr.io/token scope="repository:owner/image:pull"
2026-10-22T01:00:02+08:00 获取上游 token：ghcr.io https://ghcr.io/token?scope=repository%3Aowner%2Fimage%3Apull&service=ghcr.io
2026-10-22T01:00:02+08:00 上游 token 被拒绝：ghcr.io 返回 401 Unauthorized，响应体：{"errors":[{"code":"UNAUTHORIZED","message":"authentication required"}]}
2026-10-22T01:00:02+08:00 上游失败：ghcr.io GET https://ghcr.io/v2/owner/image/manifests/v1: token endpoint returned 401 Unauthorized（已耗时 3.3s）
2026-10-22T01:00:02+08:00 GET /v2/ghcr.io/owner/image/manifests/v1 502 24 3.3s
```

正常拉取（含缓存命中）：

```
2026-10-22T01:00:02+08:00 代理请求：GET /v2/library/alpine/manifests/latest -> registry-1.docker.io/v2/library/alpine/manifests/latest（客户端 1.2.3.4）
2026-10-22T01:00:02+08:00 上游 token 获取成功：docker.io（长度 2698）
2026-10-22T01:00:02+08:00 上游响应：docker.io GET https://registry-1.docker.io/v2/library/alpine/manifests/latest -> 200（application/vnd.oci.image.index.v1+json，已耗时 5.8s）
2026-10-22T01:00:02+08:00 GET /v2/library/alpine/manifests/latest 200 9218 5.8s
2026-10-22T01:00:05+08:00 缓存命中：/v2/library/alpine/blobs/sha256:…
```

| 日志 | 含义 |
| --- | --- |
| 没有对应请求行 | 请求没到代理：DNS、Cloudflare、端口或 `--read-only` 下的监听配置 |
| `拒绝请求：代理认证失败` + `401` | 客户端没登录代理或密码错误；Docker 显示成 `not found` |
| `拒绝请求：路由不合法` / `未配置的上游` + `404` | 镜像路径不合法，或显式仓库域名未配置 |
| `上游 token 被拒绝` / `上游失败` + `502` | 请求已转发，上游或 token 服务拒绝；日志里有上游状态码和响应体前 512 字节 |
| `上游响应 … -> 404` | 上游确实没有该仓库或 tag |
| `跟随上游重定向` | 下载 blob 时的对象存储跳转，正常现象 |

| 现象 | 检查项 |
| --- | --- |
| 证书获取或续期失败 | 域名 A/AAAA、TCP 80/443、防火墙、出站 HTTPS、CAA 和服务器时间；不要删除证书卷 |
| 401 | 代理登录账号密码；私有镜像另需配置上游凭据。Docker 会把这个 401 显示成 `not found`，别误判为镜像不存在 |
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

测试使用本地模拟上游与 CA，证书失败重试测试约需一分钟。容器验证使用 `docker build -t oci-pull-proxy:verification .` 和 `python3 scripts/container-smoke.py`。真实 mirror 验收运行 `python3 scripts/mirror-smoke.py`：创建临时特权 Docker-in-Docker daemon，只连接内部测试网络，禁止直接回退 Docker Hub；测试 HTTPS token 拉取、Token 写回 `.env` 及容器重建后的持久化，结束后清理容器与网络，不更改宿主机 daemon 配置。

推送 main 后，GitHub Actions 通过 Go 检查及容器测试才会发布 `latest` 和 `sha-完整提交哈希` 镜像；推送 `v1.2.3` 标签发布 `1.2.3`，不改动 `latest`。首次发布需在 GHCR 包设置中设为 Public，才能匿名拉取。需要可重复部署时固定镜像 digest。
