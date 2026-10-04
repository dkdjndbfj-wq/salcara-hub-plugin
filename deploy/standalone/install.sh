#!/usr/bin/env bash
# First installation only. Run a reviewed local checkout, never curl | sh.
# No Docker/Nginx installation, daemon restart, existing-data cleanup or secrets output.
set -Eeuo pipefail
umask 077

readonly version=0.5.0
readonly project=salcara-hub-standalone
readonly image="ghcr.io/dkdjndbfj-wq/salcara-hub-standalone:$version"
readonly volume="${project}_hub-data"
public_url=''
install_dir=''
created_dir=''
finished=false

usage() {
    printf '%s\n' \
        '首次部署 Salcara Hub 0.5.0（Linux；需要已有 Docker、Compose、HTTPS 反代）。' \
        '用法：bash deploy/standalone/install.sh [--public-url URL --install-dir ABSOLUTE_PATH]' \
        '示例：bash deploy/standalone/install.sh --public-url https://relay.example.com/salcara-hub --install-dir /opt/salcara-hub' \
        '无参数时交互填写。已有部署请按 docs/STANDALONE-DOCKER.zh.md 迁移，不要重复运行。'
}

die() { printf '未完成：%s\n' "$*" >&2; exit 1; }

on_exit() {
    local status=$?
    if [[ "$finished" != true && -n "$created_dir" ]]; then
        printf '%s\n' \
            "安装未完成。保留现场：$created_dir" \
            '没有自动删除数据卷、清理旧服务或重试初始化。请检查错误，必要时按部署文档恢复。' >&2
    fi
    return "$status"
}
trap on_exit EXIT

while (( $# )); do
    case "$1" in
        --public-url)
            (( $# >= 2 )) || die '--public-url 缺少值。'
            [[ -z "$public_url" ]] || die '--public-url 不能重复。'
            public_url=$2; shift 2 ;;
        --install-dir)
            (( $# >= 2 )) || die '--install-dir 缺少值。'
            [[ -z "$install_dir" ]] || die '--install-dir 不能重复。'
            install_dir=$2; shift 2 ;;
        -h|--help) usage; exit 0 ;;
        *) die '未知参数；使用 --help 查看用法。' ;;
    esac
done

if [[ -z "$public_url" || -z "$install_dir" ]]; then
    [[ -t 0 ]] || die '非交互运行必须提供 --public-url 和 --install-dir。'
    [[ -n "$public_url" ]] || read -r -p '本站公开 URL（https://域名/salcara-hub）：' public_url
    [[ -n "$install_dir" ]] || read -r -p '新安装目录（绝对路径，如 /opt/salcara-hub）：' install_dir
fi

# Deliberately allow a DNS/IPv4 HTTPS origin only; no userinfo, query, fragment,
# shell syntax, alternate path or nonstandard port can reach .env or the output.
[[ "$public_url" =~ ^https://([A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?)(:443)?/salcara-hub/?$ ]] ||
    die 'URL 必须为 https://域名/salcara-hub（可省略或使用端口 443）。'
host=${BASH_REMATCH[1]}
[[ "$host" != *..* && "$host" != localhost && "$host" != 127.* && "$host" != 0.0.0.0 ]] ||
    die '请提供本站真实公开 HTTPS 域名，不使用本地地址。'
public_url=${public_url%/}

[[ "$install_dir" =~ ^/[A-Za-z0-9_./-]+$ ]] || die '安装目录必须为不含空格的绝对路径。'
install_dir=${install_dir%/}
case "$install_dir" in
    ''|/|/opt|/etc|/var|/var/lib|/usr|/usr/local|/tmp|/run|/home|/root)
        die '请选择一个专门的新子目录，不使用系统目录或用户主目录本身。' ;;
esac
[[ "$install_dir" != "${HOME:-}" ]] || die '不能使用用户主目录本身。'
[[ "$install_dir" != */./* && "$install_dir" != */../* && "$install_dir" != */. && "$install_dir" != */.. ]] ||
    die '路径不能包含 . 或 ..。'
[[ ! -e "$install_dir" && ! -L "$install_dir" ]] || die '安装目录已存在；不覆盖、不重试初始化。'

for tool in uname realpath dirname mkdir cp chmod docker curl grep; do
    command -v "$tool" >/dev/null 2>&1 || die "缺少 $tool；请自行安装，脚本不会更改系统。"
done
[[ "$(uname -s)" == Linux ]] || die '请在目标 Linux 主机运行，而不是在桌面电脑远程操作 Docker。'
[[ "$(realpath -m -- "$install_dir")" == "$install_dir" ]] || die '安装路径包含符号链接或未规范化目录。'
parent_dir=$(dirname -- "$install_dir")
[[ -d "$parent_dir" && -w "$parent_dir" ]] || die '安装目录的父目录必须已存在且当前用户可写。'

# Protect against an accidentally selected remote Docker context.
case "${DOCKER_HOST:-}" in ''|unix://*) ;; *) die '拒绝远程 DOCKER_HOST；请在目标主机使用本地 Docker。' ;; esac
context=$(docker context show) || die '无法读取 Docker context。'
endpoint=$(docker context inspect "$context" --format '{{.Endpoints.docker.Host}}') || die '无法读取 Docker endpoint。'
[[ "$endpoint" == unix://* ]] || die '仅支持本机 Unix socket Docker context。'
[[ "$(docker info --format '{{.OSType}}')" == linux ]] || die '本机 Docker Engine 不可用或不是 Linux。'
docker compose version >/dev/null || die '需要 Docker Compose 插件。'

existing_containers=$(docker ps -aq --filter "label=com.docker.compose.project=$project") || die '无法检查已有 Hub。'
[[ -z "$existing_containers" ]] || die '已有本项目容器，请按升级/迁移文档处理。'
existing_volumes=$(docker volume ls -q) || die '无法检查已有数据卷。'
if printf '%s\n' "$existing_volumes" | grep -Fxq "$volume"; then
    die '已有 Hub 数据卷；禁止重新首次安装，请按迁移文档保留数据。'
fi
labelled_volumes=$(docker volume ls -q --filter "label=com.docker.compose.project=$project") || die '无法检查已有项目数据卷。'
[[ -z "$labelled_volumes" ]] || die '已有本项目的数据卷，请先核对原部署。'
if command -v ss >/dev/null 2>&1; then
    listeners=$(ss -H -ltn 'sport = :8787') || die '无法检查 8787 端口。'
    [[ -z "$listeners" ]] || die '8787 已被使用；不停止其他服务。'
fi

source_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd -P)
[[ "$install_dir" != "$source_dir" && "$install_dir" != "$source_dir/"* ]] || die '安装目录不能位于源码仓库内，避免误提交管理密钥。'
[[ -f "$source_dir/compose.release.yml" && ! -L "$source_dir/compose.release.yml" ]] || die '缺少正式 Compose 配置。'
[[ -f "$source_dir/deploy/standalone/nginx-location.conf" && ! -L "$source_dir/deploy/standalone/nginx-location.conf" ]] || die '缺少反代片段。'

# A missing/unpublished release is not an installation success. Network failures
# abort before creating an installation directory or initializing a data volume.
release=$(curl --fail --silent --show-error --proto '=https' --tlsv1.2 --max-time 30 \
    "https://api.github.com/repos/dkdjndbfj-wq/salcara-hub-plugin/releases/tags/standalone-v$version") || die '正式 Release 不存在或不可达。'
grep -Eq '"tag_name"[[:space:]]*:[[:space:]]*"standalone-v0\.5\.0"' <<< "$release" || die 'Release 标签不匹配。'
grep -Eq '"draft"[[:space:]]*:[[:space:]]*false' <<< "$release" || die '不是公开正式 Release。'
grep -Eq '"prerelease"[[:space:]]*:[[:space:]]*false' <<< "$release" || die '不安装预发布版本。'

mkdir -m 700 -- "$install_dir" || die '无法创建专用安装目录。'
created_dir=$install_dir
# Ignore this entire deployment directory if its parent later becomes a Git repo.
printf '*\n' > "$install_dir/.gitignore"
cp -- "$source_dir/compose.release.yml" "$install_dir/compose.release.yml"
cp -- "$source_dir/deploy/standalone/nginx-location.conf" "$install_dir/nginx-location.conf"
printf 'SALCARA_HUB_PUBLIC_URL=%s\nSALCARA_HUB_IMAGE_VERSION=%s\nSALCARA_HUB_DISABLE_UPDATES=false\n' \
    "$public_url" "$version" > "$install_dir/.env"
chmod 600 -- "$install_dir/.env" "$install_dir/compose.release.yml" "$install_dir/nginx-location.conf"

# Explicit arguments avoid Compose picking a parent directory's configuration.
export SALCARA_HUB_PUBLIC_URL="$public_url" SALCARA_HUB_IMAGE_VERSION="$version" SALCARA_HUB_DISABLE_UPDATES=false
compose() { docker compose --project-name "$project" --project-directory "$install_dir" --env-file "$install_dir/.env" -f "$install_dir/compose.release.yml" "$@"; }
compose config --quiet || die 'Compose 配置验证失败。'
compose pull hub || die "无法拉取正式镜像 $image；没有初始化管理密钥。"

# Check again after the network operation. Init also refuses to replace an
# existing account; do not run two installers against one host concurrently.
existing_volumes=$(docker volume ls -q) || die '无法再次检查数据卷。'
if printf '%s\n' "$existing_volumes" | grep -Fxq "$volume"; then
    die '检查期间已出现 Hub 数据卷；禁止继续初始化。'
fi
compose run --rm --no-deps hub -init || die '初始化失败；不要删卷或重复初始化，先检查保留的现场。'
compose up -d --no-deps hub || die 'Hub 启动失败；已保留初始化数据。'

healthy=false
for attempt in {1..15}; do
    if health=$(curl --fail --silent --show-error --max-time 2 'http://127.0.0.1:8787/healthz' 2>/dev/null) &&
       printf '%s\n' "$health" | grep -Eq '"product"[[:space:]]*:[[:space:]]*"salcara-hub-standalone"' &&
       printf '%s\n' "$health" | grep -Eq '"version"[[:space:]]*:[[:space:]]*"0\.5\.0"' &&
       printf '%s\n' "$health" | grep -Eq '"ok"[[:space:]]*:[[:space:]]*true'; then
        healthy=true; break
    fi
    sleep 1
done
[[ "$healthy" == true ]] || die '未确认 0.5.0 本地健康；不输出部署完成提示。'

mkdir -m 700 -- "$install_dir/secrets"
compose cp hub:/data/admin-initial-login.txt "$install_dir/secrets/admin-initial-login.txt" >/dev/null || die '无法安全导出初始管理密钥。'
chmod 600 -- "$install_dir/secrets/admin-initial-login.txt"
finished=true
printf '\n%s\n' \
    'Hub 容器已就绪；公网反代仍需按文档合并、测试并 reload，脚本没有修改 Nginx。' \
    "反代片段：$install_dir/nginx-location.conf" \
    "管理地址 / 管理员 iframe URL：$public_url/admin/" \
    "普通用户说明 iframe URL：$public_url/" \
    "初始管理密钥文件：$install_dir/secrets/admin-initial-login.txt（0600；在自己的安全编辑器中读取）" \
    '自行在安全终端读取（不要把输出发到聊天）：' \
    "cat -- $install_dir/secrets/admin-initial-login.txt" \
    '文件只有完整管理密钥一行，直接复制到管理页；后台可更换管理密钥。不要发给 Agent、客服、聊天或写进 URL。' \
    '更换后删除本地初始文件副本；停止/卸载前先备份，不删除数据卷。' \
    '完整步骤及公网验收：docs/STANDALONE-DOCKER.zh.md；Agent 部署：docs/AGENT-DEPLOY.zh.md。'
