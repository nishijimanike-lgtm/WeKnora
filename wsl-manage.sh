#!/usr/bin/env bash
# ==============================================================================
# WeKnora WSL2 运维管理快捷脚本
# 支持在 Windows Git Bash / MINGW64 中直接执行，也支持进入 WSL 后执行
# ==============================================================================

WSL_DISTRO="Ubuntu"
PROJECT_DIR_WSL="/mnt/d/github_nishi/WeKnora"

# 颜色定义
GREEN='\033[0;32m'
BLUE='\033[0;34m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m'

# 判断当前运行环境：如果是在 Windows (Git Bash / MINGW / MSYS)，则转发到 WSL
if [[ "$(uname -s)" =~ (MINGW|MSYS|CYGWIN) ]]; then
    CMD="$*"
    if [ -z "$CMD" ]; then
        CMD="help"
    fi
    exec wsl -d "$WSL_DISTRO" -- bash -c "cd '$PROJECT_DIR_WSL' && ./wsl-manage.sh $CMD"
fi

# ==============================================================================
# 以下逻辑在 WSL (Ubuntu Linux) 内部执行
# ==============================================================================

# 确保 Docker 服务处于运行状态
ensure_docker() {
    if ! docker info >/dev/null 2>&1; then
        echo -e "${YELLOW}[!] Docker 守护进程未启动，正在启动 Docker 服务...${NC}"
        sudo service docker start
        sleep 2
        if ! docker info >/dev/null 2>&1; then
            echo -e "${RED}[✗] Docker 服务启动失败，请检查 WSL 内 Docker 配置。${NC}"
            exit 1
        fi
        echo -e "${GREEN}[✓] Docker 服务已就绪。${NC}"
    fi
}

# 帮助菜单
show_help() {
    echo -e "${GREEN}WeKnora WSL2 容器管理工具${NC}"
    echo "------------------------------------------------------"
    echo "用法: ./wsl-manage.sh [命令] [参数]"
    echo ""
    echo "常用命令:"
    echo "  start               启动所有服务（后台运行并检查健康状态）"
    echo "  stop                停止所有服务（数据安全保留）"
    echo "  restart [服务名]    重启全部服务或指定服务（如: restart app）"
    echo "  status / ps         查看所有容器运行状态与端口映射"
    echo "  logs [服务名]       查看实时日志（如: logs app / logs docreader）"
    echo "  health              检查前端与后端接口健康状态"
    echo "  sh / exec [服务名]  进入指定容器 Shell（默认: app）"
    echo "  pull                拉取最新官方镜像"
    echo "  help                显示本帮助信息"
    echo "------------------------------------------------------"
    echo "访问地址: Web前端: http://localhost:3188 | 后端API: http://localhost:5188"
}

ACTION="${1:-help}"
TARGET="${2}"

case "$ACTION" in
    start|up)
        ensure_docker
        echo -e "${BLUE}[*] 正在启动 WeKnora 容器集群...${NC}"
        docker compose up -d
        echo -e "${BLUE}[*] 检查容器状态...${NC}"
        docker compose ps
        echo ""
        echo -e "${GREEN}[✓] 启动完成！访问入口：${NC}"
        echo -e "    Web 界面 : ${BLUE}http://localhost:3188${NC}"
        echo -e "    后端 API : ${BLUE}http://localhost:5188${NC}"
        ;;

    stop|down)
        ensure_docker
        echo -e "${YELLOW}[*] 正在停止 WeKnora 容器集群...${NC}"
        docker compose down
        echo -e "${GREEN}[✓] 所有容器已安全停止。${NC}"
        ;;

    restart)
        ensure_docker
        if [ -n "$TARGET" ]; then
            echo -e "${BLUE}[*] 正在重启指定服务: ${TARGET}...${NC}"
            docker compose restart "$TARGET"
            echo -e "${GREEN}[✓] 服务 ${TARGET} 已重启。${NC}"
        else
            echo -e "${BLUE}[*] 正在重启所有服务...${NC}"
            docker compose restart
            echo -e "${GREEN}[✓] 所有服务已重启。${NC}"
        fi
        docker compose ps
        ;;

    status|ps)
        ensure_docker
        echo -e "${BLUE}=== WeKnora 容器状态 ===${NC}"
        docker compose ps
        ;;

    logs)
        ensure_docker
        if [ -n "$TARGET" ]; then
            echo -e "${BLUE}[*] 跟踪服务 [${TARGET}] 的日志 (Ctrl+C 退出)...${NC}"
            docker compose logs -f --tail=100 "$TARGET"
        else
            echo -e "${BLUE}[*] 跟踪所有服务日志 (Ctrl+C 退出)...${NC}"
            docker compose logs -f --tail=50
        fi
        ;;

    health)
        echo -e "${BLUE}=== 正在检测服务健康状态 ===${NC}"
        echo -n "后端健康检查 (http://localhost:5188/health): "
        if curl -s -f http://localhost:5188/health >/dev/null; then
            echo -e "${GREEN}[正常 OK]${NC}"
        else
            echo -e "${RED}[异常] (后端未就绪或未启动)${NC}"
        fi

        echo -n "前端 Web 页面 (http://localhost:3188): "
        if curl -s -f http://localhost:3188 >/dev/null; then
            echo -e "${GREEN}[正常 OK]${NC}"
        else
            echo -e "${RED}[异常] (前端未就绪或未启动)${NC}"
        fi
        ;;

    sh|exec)
        ensure_docker
        SERVICE="${TARGET:-app}"
        echo -e "${BLUE}[*] 正在进入容器: ${SERVICE}...${NC}"
        docker compose exec -it "$SERVICE" sh
        ;;

    pull)
        ensure_docker
        echo -e "${BLUE}[*] 正在拉取最新镜像...${NC}"
        docker compose pull
        echo -e "${GREEN}[✓] 镜像拉取完毕，执行 ./wsl-manage.sh start 可应用最新镜像。${NC}"
        ;;

    help|--help|-h)
        show_help
        ;;

    *)
        echo -e "${RED}[✗] 未知命令: $ACTION${NC}"
        echo ""
        show_help
        exit 1
        ;;
esac
