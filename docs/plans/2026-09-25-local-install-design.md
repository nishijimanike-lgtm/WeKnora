# WeKnora 本地 Docker 安装与部署设计方案

- **日期**：2026-09-25
- **环境**：Windows 11 + WSL2 (Ubuntu 24.04, Docker Engine 29.1.3 + Docker Compose 2.40.3)
- **目标**：在本地容器化完整部署 WeKnora 平台，避开端口冲突并稳定运行。

---

## 1. 架构与组件规划

* **前端容器 (`WeKnora-frontend`)**：
  * 基于 Vue 3 + Nginx，映射宿主机端口 **`3188`** (`3188:80`)。
  * 反向代理 `/api/` 请求到后端容器 `app:8080`。
* **后端容器 (`WeKnora-app`)**：
  * Go 1.26 主服务，映射宿主机端口 **`5188`** (`5188:8080`)。
  * 提供 RESTful API、Worker 队列分发以及与各大 LLM 供应商的交互。
* **文档解析容器 (`docreader`)**：
  * Python gRPC 微服务，负责多种格式（PDF、Office、Markdown、图片等）解析切片。
* **存储与数据设施**：
  * `postgres`：ParadeDB (PostgreSQL 17 + pg_search + pgvector 1024 维)。
  * `redis`：任务队列与运行时缓存。

---

## 2. 端口与网络映射

| 服务组件 | 容器内端口 | 宿主机映射端口 | 访问方式 |
| :--- | :--- | :--- | :--- |
| **Web UI 前端** | 80 | **3188** | `http://localhost:3188` |
| **后端 API 服务** | 8080 | **5188** | `http://localhost:5188` |
| **Postgres / ParadeDB** | 5432 | 5432 (仅内部) | 容器网络 `WeKnora-network` |
| **Redis** | 6379 | 6379 (仅内部) | 容器网络 `WeKnora-network` |
| **Docreader gRPC** | 50051 | 50051 (仅内部) | 容器网络 `WeKnora-network` |

---

## 3. 环境配置 (.env)

从 `.env.example` 复制初始化 `.env` 并配置关键变量：
* `FRONTEND_PORT=3188`
* `APP_PORT=5188`
* `AUTO_MIGRATE=true`
* `AUTO_RECOVER_DIRTY=true`
* 生成安全的 `JWT_SECRET` 与 `SYSTEM_AES_KEY`

---

## 4. 异常处理与测试验收

* **启动与依赖检查**：
  * 通过 `docker compose pull` 拉取官方镜像（ParadeDB, Redis, WeKnora app, WeKnora frontend, docreader）。
  * 启动后依赖 `app` 的健康检查 `http://localhost:8080/health`，确保依赖准备就绪后前端再接入。
* **验收标准**：
  1. `docker compose ps` 显示所有核心服务均为 `Up (healthy)`。
  2. 宿主机调用 `http://localhost:5188/health` 获得 200 OK 响应。
  3. 宿主机浏览器访问 `http://localhost:3188` 成功打开 WeKnora 登录/工作区界面。
