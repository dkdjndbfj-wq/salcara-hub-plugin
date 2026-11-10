#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

install_dir=''
while (( $# )); do
    case "$1" in
        --install-dir) (( $# >= 2 )) || { printf '缺少 --install-dir 值。\n' >&2; exit 2; }; install_dir=$2; shift 2 ;;
        -h|--help) printf '用法：bash deploy/standalone/resume-bot.sh --install-dir ABSOLUTE_PATH\n'; exit 0 ;;
        *) printf '未知参数。\n' >&2; exit 2 ;;
    esac
done

fail() { printf 'Bot 尚未验收：%s\n' "$*" >&2; exit 1; }
[[ "$install_dir" =~ ^/[A-Za-z0-9_./-]+$ && "$install_dir" != */../* && "$install_dir" != */./* ]] || fail '安装目录必须是规范绝对路径。'
[[ -d "$install_dir" && ! -L "$install_dir" ]] || fail '安装目录不存在或是符号链接。'
[[ -f "$install_dir/.env" && ! -L "$install_dir/.env" && -f "$install_dir/compose.release.yml" && ! -L "$install_dir/compose.release.yml" ]] || fail '原部署配置缺失或不安全。'
grep -Fxq 'SALCARA_HUB_MODE=bot' "$install_dir/.env" || fail '原部署不是 Bot 模式。'
grep -Fxq 'SALCARA_HUB_IMAGE_VERSION=0.5.4' "$install_dir/.env" || fail 'Bot 镜像版本不匹配；不会启动或迁移数据。'
grep -Fxq 'SALCARA_HUB_PROJECT=salcara-hub-bot' "$install_dir/.env" || fail 'Bot Compose 项目标识不匹配。'
grep -Fxq 'SALCARA_HUB_DATA_VOLUME=salcara-hub-bot_hub-data' "$install_dir/.env" || fail 'Bot 数据卷标识不匹配。'
build_mode=$(sed -n 's/^SALCARA_HUB_BUILD_MODE=//p' "$install_dir/.env")
build_mode=${build_mode:-registry}
[[ "$build_mode" == registry || "$build_mode" == source ]] || fail '部署构建模式无效。'
source_commit=$(sed -n 's/^SALCARA_HUB_SOURCE_COMMIT=//p' "$install_dir/.env")
local_image_id=$(sed -n 's/^SALCARA_HUB_LOCAL_IMAGE_ID=//p' "$install_dir/.env")
if [[ "$build_mode" == source ]]; then
    [[ "$source_commit" =~ ^[0-9a-f]{40,64}$ ]] || fail '源码提交标识缺失或无效。'
    [[ "$local_image_id" =~ ^sha256:[a-f0-9]{64}$ ]] || fail '本地构建镜像标识缺失或无效。'
fi
image='ghcr.io/dkdjndbfj-wq/salcara-hub-standalone:0.5.4'
public_url=$(sed -n 's/^SALCARA_HUB_PUBLIC_URL=//p' "$install_dir/.env")
[[ "$public_url" =~ ^https://([A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?)(:443)?/salcara-hub$ ]] || fail '公开 HTTPS 地址无效。'
[[ "$(uname -s)" == Linux ]] || fail '续跑仅支持目标 Linux 主机。'
case "${DOCKER_HOST:-}" in ''|unix://*) ;; *) fail '拒绝远程 DOCKER_HOST；请在目标主机本地续跑。' ;; esac
command -v docker >/dev/null 2>&1 || fail '缺少 Docker。'
context=$(docker context show) || fail '无法读取 Docker context。'
endpoint=$(docker context inspect "$context" --format '{{.Endpoints.docker.Host}}') || fail '无法读取 Docker endpoint。'
[[ "$endpoint" == unix://* ]] || fail '仅支持本机 Unix socket Docker context。'
[[ "$(docker info --format '{{.OSType}}')" == linux ]] || fail '本机 Docker Engine 不可用或不是 Linux。'
docker compose version >/dev/null || fail '需要 Docker Compose 插件。'

export SALCARA_HUB_PUBLIC_URL="$public_url" SALCARA_HUB_IMAGE_VERSION=0.5.4 SALCARA_HUB_MODE=bot SALCARA_HUB_DATA_VOLUME=salcara-hub-bot_hub-data SALCARA_HUB_PROJECT=salcara-hub-bot SALCARA_HUB_DISABLE_UPDATES=false
compose() { docker compose --project-name salcara-hub-bot --project-directory "$install_dir" --env-file "$install_dir/.env" -f "$install_dir/compose.release.yml" "$@"; }
compose config --quiet || fail 'Compose 配置验证失败。'
docker volume inspect salcara-hub-bot_hub-data >/dev/null 2>&1 || fail '原 Bot 数据卷不存在；拒绝初始化新卷或伪装成恢复成功。'
if [[ "$build_mode" == source ]]; then
    image_id=$(docker image inspect --format '{{.Id}}' "$image" 2>/dev/null || true)
    if [[ "$image_id" != "$local_image_id" ]]; then
        image_archive="$install_dir/bot-image.tar.gz"
        [[ -f "$image_archive" && ! -L "$image_archive" && "$(stat -c '%a' "$image_archive")" == 600 ]] || fail '源码构建镜像缺失且没有安全的本地恢复归档。'
        command -v gzip >/dev/null 2>&1 || fail '缺少 gzip，无法恢复本地构建镜像。'
        gzip -dc -- "$image_archive" | docker load >/dev/null || fail '无法从本地归档恢复 Bot 镜像。'
        image_id=$(docker image inspect --format '{{.Id}}' "$image" 2>/dev/null || true)
    fi
    [[ "$image_id" == "$local_image_id" ]] || fail '本地 Bot 镜像与已记录构建标识不匹配。'
    built_version=$(docker image inspect --format '{{ index .Config.Labels "org.opencontainers.image.version" }}' "$image") || fail '无法读取本地 Bot 镜像版本标签。'
    [[ "$built_version" == 0.5.4 ]] || fail '本地 Bot 镜像版本标签不匹配。'
else
    compose pull hub || fail '正式 Bot 镜像不可用；原数据卷和部署配置均已保留。'
fi
command -v ss >/dev/null 2>&1 || fail '缺少 ss，无法安全检查 8787 端口。'
running_id=$(compose ps -q hub) || fail '无法检查原 Bot 容器状态。'
if [[ -n "$running_id" ]]; then
    running_project=$(docker inspect --format '{{ index .Config.Labels "com.docker.compose.project" }}' "$running_id") || fail '无法确认正在运行的容器归属。'
    [[ "$running_project" == salcara-hub-bot ]] || fail '8787 对应容器不是 Bot 项目；未停止其他服务。'
else
    listeners=$(ss -H -ltn 'sport = :8787') || fail '无法检查 8787 端口。'
    [[ -z "$listeners" ]] || fail '8787 已被其他服务占用；未停止其他服务。'
fi

# Reattach to the exact existing data volume. This command never runs -init.
if [[ "$build_mode" == source ]]; then
    compose up -d --no-deps --pull never hub || fail '无法使用已验证的本地 Bot 镜像启动原容器；数据卷和配置均已保留。'
else
    compose up -d --no-deps --pull never hub || fail '无法启动原 Bot 容器；数据卷和配置均已保留。'
fi
healthy=false
for attempt in {1..30}; do
    if health=$(curl --fail --silent --show-error --max-time 2 'http://127.0.0.1:8787/healthz' 2>/dev/null) &&
        printf '%s\n' "$health" | grep -Eq '"product"[[:space:]]*:[[:space:]]*"salcara-hub-standalone"' &&
        printf '%s\n' "$health" | grep -Eq '"version"[[:space:]]*:[[:space:]]*"0\.5\.4"' &&
        printf '%s\n' "$health" | grep -Eq '"ok"[[:space:]]*:[[:space:]]*true'; then
        healthy=true; break
    fi
    sleep 1
done
[[ "$healthy" == true ]] || fail '未确认 Bot 版本本地健康；不会生成绑定包。'

public_ping=$(curl --fail --silent --show-error --proto '=https' --tlsv1.2 --max-time 10 "$public_url/v1/ping") || fail '公开 HTTPS /v1/ping 不可用；修复反代后再次续跑。'
grep -Eq '"service"[[:space:]]*:[[:space:]]*"salcara-hub"' <<< "$public_ping" || fail '公开 HTTPS 地址未转发到 Salcara Hub。'
stream_status=$(curl --silent --show-error --proto '=https' --tlsv1.2 --max-time 10 -o /dev/null -w '%{http_code}' "$public_url/v1/bridge/stream?deviceId=00000000-0000-4000-8000-000000000000") || fail '无法探测 Bridge 流式入口。'
[[ "$stream_status" == 400 || "$stream_status" == 403 ]] || fail "Bridge 流式入口返回 HTTP $stream_status。"
event_status=$(curl --silent --show-error --proto '=https' --tlsv1.2 --max-time 10 -o /dev/null -w '%{http_code}' "$public_url/v1/app/events?deviceId=00000000-0000-4000-8000-000000000000&scope=device&wait=1") || fail '无法探测 App 长轮询入口。'
[[ "$event_status" == 400 || "$event_status" == 403 ]] || fail "App 长轮询入口返回 HTTP $event_status。"

mkdir -p -- "$install_dir/secrets"
[[ ! -L "$install_dir/secrets" ]] || fail 'secrets 路径不能是符号链接。'
chmod 700 -- "$install_dir/secrets"
if [[ ! -f "$install_dir/secrets/admin-initial-login.txt" ]]; then
    compose cp hub:/data/admin-initial-login.txt "$install_dir/secrets/admin-initial-login.txt" >/dev/null || fail '无法安全恢复初始管理密钥副本。'
    chmod 600 -- "$install_dir/secrets/admin-initial-login.txt"
fi

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
binding_script="$script_dir/create-bot-binding.sh"
if [[ ! -f "$binding_script" || -L "$binding_script" ]]; then
    source_dir=$(cd -- "$script_dir/../.." && pwd -P)
    binding_script="$source_dir/deploy/standalone/create-bot-binding.sh"
fi
[[ -f "$binding_script" && ! -L "$binding_script" ]] || fail '找不到安全的 Bot 绑定包签发脚本。'
binding=$(bash "$binding_script" --install-dir "$install_dir") || fail '绑定包签发未确认；容器、卷和其他状态均已保留。'
printf '\nBot Hub 已通过本地健康、公开 HTTPS、Bridge 流式和 App 长轮询检查。电脑与手机导入同一绑定 JSON：\n%s\n' "$binding"
