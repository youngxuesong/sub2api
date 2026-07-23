# “龟兹叶子” Sub2API 生产部署设计

## 目标

在 `47.119.114.46` 上部署 Sub2API，通过 `http://47.119.114.46` 的 80 端口提供服务，并将系统原生站点名称设置为“龟兹叶子”。

## 已确认环境

- Ubuntu 26.04，x86_64
- 根磁盘 40GB，当前约 35GB 可用
- 内存 1.6GB，无 Swap
- 80 端口空闲，当前只有 SSH 监听 22 端口
- Docker 与 Docker Compose 尚未安装
- 无既有容器或 Sub2API 部署目录
- SSH 主机密钥指纹：`SHA256:4TLoJ6DnYxXyrvKS/yBl4U9gcnskxcKpegoFIzkGGBk`

## 部署架构

采用项目官方 Docker Compose 架构：

- Sub2API 应用容器绑定宿主机 80 端口
- PostgreSQL 仅在 Compose 内部网络开放
- Redis 仅在 Compose 内部网络开放
- 应用数据、数据库数据和 Redis 数据使用持久化卷
- 服务目录使用 `/opt/sub2api-deploy`
- 容器均设置自动重启策略

不修改 Sub2API 源码。站点名称通过项目原生 `site_name` 设置保存为“龟兹叶子”，从而覆盖首页品牌文字、页面标题及相关系统文案。

## 配置与安全

- 为 PostgreSQL、JWT、TOTP 和初始管理员生成互不相同的随机强密钥
- 管理员密码仅在最终交付时提供，不写入版本库
- 数据库和 Redis 不暴露公网端口
- 时区设为 `Asia/Shanghai`
- 保留 SSH 22 端口，不修改现有 SSH 配置
- 首次部署暂以 HTTP/IP 访问；没有域名时不配置 TLS

## 部署流程

1. 复核 SSH 主机指纹和服务器资源。
2. 安装 Docker Engine 与 Compose 插件。
3. 在 `/opt/sub2api-deploy` 创建固定版本的 Compose 配置与权限受限的环境文件。
4. 拉取镜像并启动 PostgreSQL、Redis、Sub2API。
5. 等待容器健康检查通过。
6. 使用系统原生设置将 `site_name` 更新为“龟兹叶子”。
7. 重启或刷新必要缓存，并执行公网验收。

## 验收标准

- `http://47.119.114.46/health` 返回 HTTP 200
- 首页可正常加载
- 公开设置接口返回 `site_name: "龟兹叶子"`
- 首页品牌和 HTML 页面标题显示“龟兹叶子”
- PostgreSQL、Redis、Sub2API 容器均处于运行/健康状态
- 最近日志无持续启动错误、数据库连接错误或端口冲突
- 服务器重启策略已配置，数据卷存在

## 回滚

部署前服务器没有既有 80 端口业务。若部署失败：

1. 停止新建的 Compose 服务，释放 80 端口。
2. 保留数据卷和 `/opt/sub2api-deploy` 配置，以便排查或重试。
3. 不卸载 Docker，除非用户另行要求。
4. 若仅品牌设置失败，恢复默认站点名称或重新写入 `site_name`，无需回滚应用。

该部署不包含域名、HTTPS、邮件、支付、上游模型账户或防火墙策略配置。
