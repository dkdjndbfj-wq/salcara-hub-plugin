#!/usr/bin/env bash
# First installation only. Run a reviewed local checkout, never curl | sh.
# No Docker/Nginx installation, daemon restart, existing-data cleanup or secrets output.
set -Eeuo pipefail
umask 077

mode='station'
source_build=false
version='0.5.3'
version_pattern=''
image=''
project=''
volume=''
public_url=''
install_dir=''
created_dir=''
finished=false
initialized=false

usage() {
    printf '%s\n' \
        '首次部署 Salcara Hub（Linux；需要已有 Docker、Compose、HTTPS 反代）。' \
        'Station / Bot 均使用 0.5.3；安装前须有对应的正式 Release 和镜像。' \
	    'Bot 源码兜底：仅构建干净 Git 提交中的 Bot 源码，使用 --source-build 显式启用。' \
	    '用法：bash deploy/standalone/install.sh [--mode station|bot --source-build --public-url URL --install-dir ABSOLUTE_PATH]' \
        '示例：bash deploy/standalone/install.sh --public-url https://relay.example.com/salcara-hub --install-dir /opt/salcara-hub' \
        '无参数时交互填写。已有部署请按 docs/STANDALONE-DOCKER.zh.md 迁移，不要重复运行。'
}

die() { printf '未完成：%s\n' "$*" >&2; exit 1; }

on_exit() {
    local status=$?
    if [[ "$finished" != true && -n "$created_dir" ]]; then
        printf '%s\n' \
            "安装未完成。保留现场：$created_dir" \
            '没有自动删除数据卷、清理旧服务或重试初始化。' >&2
        if [[ "$mode" == bot && -n "${source_dir:-}" ]]; then
            if [[ "$initialized" == true && -f "$created_dir/resume-bot.sh" ]]; then
                printf '修复公开反代后可续跑（不会重新初始化）：bash %s/resume-bot.sh --install-dir %s\n' \
                    "$created_dir" "$created_dir" >&2
            elif [[ "$initialized" != true ]]; then
                printf '%s\n' '首次初始化没有确认成功；未给出自动续跑命令，以免误用或覆盖数据。先检查保留的目录和数据卷。' >&2
            else
                printf '恢复脚本未能复制；检查保留的安装目录后运行：bash %s/deploy/standalone/resume-bot.sh --install-dir %s\n' \
                    "$source_dir" "$created_dir" >&2
            fi
        else
            printf '%s\n' '请检查错误并按部署文档恢复。' >&2
        fi
    fi
    return "$status"
}
trap on_exit EXIT

while (( $# )); do
    case "$1" in
        --mode)
            (( $# >= 2 )) || die '--mode 缺少值。'
            [[ "$2" == station || "$2" == bot ]] || die '--mode 必须是 station 或 bot。'
            mode=$2; shift 2 ;;
        --source-build)
            [[ "$source_build" == false ]] || die '--source-build 不能重复。'
            source_build=true; shift ;;
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

if [[ "$mode" == station ]]; then
    project='salcara-hub-standalone'
else
    project='salcara-hub-bot'
fi
[[ "$source_build" == false || "$mode" == bot ]] || die '--source-build 只适用于 Bot 模式。'
version_pattern=${version//./\\.}
image="ghcr.io/dkdjndbfj-wq/salcara-hub-standalone:$version"
volume="${project}_hub-data"

if [[ -z "$public_url" || -z "$install_dir" ]]; then
    [[ -t 0 ]] || die '非交互运行必须提供 --public-url 和 --install-dir。'
    [[ -n "$public_url" ]] || read -r -p '本站公开 URL（https://域名/salcara-hub）：' public_url
    [[ -n "$install_dir" ]] || read -r -p '新安装目录（绝对路径，如 /opt/salcara-hub）：' install_dir
fi
if [[ "$mode" == bot && -z "$public_url" ]]; then die 'Bot 模式必须提供公开 HTTPS URL。'; fi

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
if [[ "$mode" == bot ]] && printf '%s\n' "$existing_volumes" | grep -Fxq 'salcara-hub-standalone_hub-data'; then
    die '检测到 Station 数据卷；Bot 模式使用独立数据卷，禁止直接切换。'
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
build_mode='registry'
source_commit=''
local_image_id=''
if [[ "$source_build" == true ]]; then
    command -v git >/dev/null 2>&1 || die '源码兜底需要 Git。'
    command -v gzip >/dev/null 2>&1 || die '源码兜底需要 gzip 保存可恢复的本地镜像。'
    [[ -f "$source_dir/Dockerfile" && ! -L "$source_dir/Dockerfile" ]] || die '源码兜底缺少可信 Dockerfile。'
    git_root=$(git -C "$source_dir" rev-parse --show-toplevel) || die '无法确认源码仓库根目录。'
    [[ "$(realpath -- "$git_root")" == "$source_dir" ]] || die '源码目录不是 Git 仓库根目录。'
    source_changes=$(git -C "$source_dir" status --porcelain --untracked-files=all) || die '无法检查源码工作树。'
    [[ -z "$source_changes" ]] || die '源码工作树有未提交或未跟踪改动；拒绝构建不完整版本。'
    source_commit=$(git -C "$source_dir" rev-parse --verify HEAD) || die '无法读取源码提交标识。'
    [[ "$source_commit" =~ ^[0-9a-f]{40,64}$ ]] || die '源码提交标识格式无效。'
    for source_file in internal/hub/bot.go internal/hub/bot_test.go internal/standalone/bot.go internal/standalone/enrollment.go; do
        git -C "$source_dir" cat-file -e "$source_commit:$source_file" || die "源码提交缺少 Bot 协议文件：$source_file。"
    done
    build_mode='source'
fi

# Registry mode requires a public stable Release. Source mode is an explicit,
# auditable fallback for a clean, committed Bot source revision.
if [[ "$source_build" == false ]]; then
    release=$(curl --fail --silent --show-error --proto '=https' --tlsv1.2 --max-time 30 \
        "https://api.github.com/repos/dkdjndbfj-wq/salcara-hub-plugin/releases/tags/standalone-v$version") || die '正式 Release 不存在或不可达。'
    grep -Eq "\"tag_name\"[[:space:]]*:[[:space:]]*\"standalone-v${version_pattern}\"" <<< "$release" || die 'Release 标签不匹配。'
    grep -Eq '"draft"[[:space:]]*:[[:space:]]*false' <<< "$release" || die '不是公开正式 Release。'
    grep -Eq '"prerelease"[[:space:]]*:[[:space:]]*false' <<< "$release" || die '不安装预发布版本。'
    docker pull "$image" || die "无法拉取正式镜像 $image；尚未创建部署目录或初始化管理密钥。"
    pulled_version=$(docker image inspect --format '{{ index .Config.Labels "org.opencontainers.image.version" }}' "$image") || die '无法确认拉取镜像的版本标签。'
    [[ "$pulled_version" == "$version" ]] || die "拉取镜像版本不匹配（应为 $version）。"
fi

# Build before creating the installation directory or initializing persistent
# data. The Dockerfile runs tests, vet and race tests before producing runtime.
if [[ "$source_build" == true ]]; then
    git -C "$source_dir" archive --format=tar "$source_commit" |
        docker build --pull --tag "$image" --build-arg "VERSION=$version" - || die 'Bot 源码镜像构建失败；没有创建数据卷或初始化管理密钥。'
    built_version=$(docker image inspect --format '{{ index .Config.Labels "org.opencontainers.image.version" }}' "$image") || die '无法确认本地构建镜像的版本标签。'
    [[ "$built_version" == "$version" ]] || die "本地构建镜像版本不匹配（应为 $version）。"
    local_image_id=$(docker image inspect --format '{{.Id}}' "$image") || die '无法读取本地构建镜像标识。'
    [[ "$local_image_id" =~ ^sha256:[a-f0-9]{64}$ ]] || die '本地构建镜像标识格式无效。'
fi

mkdir -m 700 -- "$install_dir" || die '无法创建专用安装目录。'
created_dir=$install_dir
# Ignore this entire deployment directory if its parent later becomes a Git repo.
printf '*\n' > "$install_dir/.gitignore"
cp -- "$source_dir/compose.release.yml" "$install_dir/compose.release.yml"
cp -- "$source_dir/deploy/standalone/nginx-location.conf" "$install_dir/nginx-location.conf"
if [[ "$mode" == bot ]]; then
    cp -- "$source_dir/deploy/standalone/create-bot-binding.sh" "$install_dir/create-bot-binding.sh"
    cp -- "$source_dir/deploy/standalone/resume-bot.sh" "$install_dir/resume-bot.sh"
fi
printf 'SALCARA_HUB_PUBLIC_URL=%s\nSALCARA_HUB_IMAGE_VERSION=%s\nSALCARA_HUB_MODE=%s\nSALCARA_HUB_DATA_VOLUME=%s\nSALCARA_HUB_PROJECT=%s\nSALCARA_HUB_BUILD_MODE=%s\nSALCARA_HUB_SOURCE_COMMIT=%s\nSALCARA_HUB_LOCAL_IMAGE_ID=%s\nSALCARA_HUB_DISABLE_UPDATES=false\n' \
    "$public_url" "$version" "$mode" "$volume" "$project" "$build_mode" "$source_commit" "$local_image_id" > "$install_dir/.env"
chmod 600 -- "$install_dir/.env" "$install_dir/compose.release.yml" "$install_dir/nginx-location.conf"
if [[ "$mode" == bot ]]; then chmod 600 -- "$install_dir/create-bot-binding.sh" "$install_dir/resume-bot.sh"; fi
if [[ "$build_mode" == source ]]; then
    docker save "$image" | gzip -1 > "$install_dir/bot-image.tar.gz" || die '无法保存源码构建镜像；部署尚未初始化。'
    [[ -s "$install_dir/bot-image.tar.gz" ]] || die '源码构建镜像归档为空；部署尚未初始化。'
    chmod 600 -- "$install_dir/bot-image.tar.gz"
fi

# Explicit arguments avoid Compose picking a parent directory's configuration.
export SALCARA_HUB_PUBLIC_URL="$public_url" SALCARA_HUB_IMAGE_VERSION="$version" SALCARA_HUB_MODE="$mode" SALCARA_HUB_DATA_VOLUME="$volume" SALCARA_HUB_PROJECT="$project" SALCARA_HUB_DISABLE_UPDATES=false
compose() { docker compose --project-name "$project" --project-directory "$install_dir" --env-file "$install_dir/.env" -f "$install_dir/compose.release.yml" "$@"; }
compose config --quiet || die 'Compose 配置验证失败。'

# Check again after image acquisition. Init also refuses to replace an
# existing account; do not run two installers against one host concurrently.
existing_volumes=$(docker volume ls -q) || die '无法再次检查数据卷。'
if printf '%s\n' "$existing_volumes" | grep -Fxq "$volume"; then
    die '检查期间已出现 Hub 数据卷；禁止继续初始化。'
fi
compose run --rm --no-deps hub -init || die '初始化失败；不要删卷或重复初始化，先检查保留的现场。'
initialized=true
compose up -d --no-deps --pull never hub || die 'Hub 启动失败；已保留初始化数据。'

healthy=false
for attempt in {1..15}; do
    if health=$(curl --fail --silent --show-error --max-time 2 'http://127.0.0.1:8787/healthz' 2>/dev/null) &&
       printf '%s\n' "$health" | grep -Eq '"product"[[:space:]]*:[[:space:]]*"salcara-hub-standalone"' &&
       printf '%s\n' "$health" | grep -Eq "\"version\"[[:space:]]*:[[:space:]]*\"${version_pattern}\"" &&
       printf '%s\n' "$health" | grep -Eq '"ok"[[:space:]]*:[[:space:]]*true'; then
        healthy=true; break
    fi
    sleep 1
done
[[ "$healthy" == true ]] || die "未确认 $version 本地健康；不输出部署完成提示。"

if [[ "$mode" == bot ]]; then
    # Bot credentials are useful only when computers and phones can reach the
    # configured public route. Probe each unauthenticated boundary before
    # declaring the deployment ready; an auth failure is expected, a 404 is a
    # misrouted reverse proxy. The proxy snippet separately disables buffering
    # and keeps connections open for SSE and long-poll traffic.
    public_ping=$(curl --fail --silent --show-error --proto '=https' --tlsv1.2 --max-time 10 "$public_url/v1/ping") || die 'Bot 模式的公开 HTTPS 地址无法访问 /v1/ping；不会报告安装完成。'
    grep -Eq '"service"[[:space:]]*:[[:space:]]*"salcara-hub"' <<< "$public_ping" || die '公开 HTTPS 地址没有转发到 Salcara Hub。'
    stream_status=$(curl --silent --show-error --proto '=https' --tlsv1.2 --max-time 10 -o /dev/null -w '%{http_code}' "$public_url/v1/bridge/stream?deviceId=00000000-0000-4000-8000-000000000000") || die 'Bot 模式无法探测公开 Bridge 流式入口。'
    [[ "$stream_status" == 400 || "$stream_status" == 403 ]] || die "公开 Bridge 流式入口返回 $stream_status；请检查反代路径和认证转发。"
    event_status=$(curl --silent --show-error --proto '=https' --tlsv1.2 --max-time 10 -o /dev/null -w '%{http_code}' "$public_url/v1/app/events?deviceId=00000000-0000-4000-8000-000000000000&scope=device&wait=1") || die 'Bot 模式无法探测公开 App 长轮询入口。'
    [[ "$event_status" == 400 || "$event_status" == 403 ]] || die "公开 App 长轮询入口返回 $event_status；请检查反代路径和认证转发。"
fi

mkdir -m 700 -- "$install_dir/secrets"
compose cp hub:/data/admin-initial-login.txt "$install_dir/secrets/admin-initial-login.txt" >/dev/null || die '无法安全导出初始管理密钥。'
chmod 600 -- "$install_dir/secrets/admin-initial-login.txt"
if [[ "$mode" == bot ]]; then
    bash "$install_dir/create-bot-binding.sh" --install-dir "$install_dir" || die 'Hub 已启动，但绑定包签发未确认；保留现场并使用续跑命令。'
fi
finished=true
printf '\n%s\n' \
    "Hub 容器已就绪（$mode 模式，版本 $version）；脚本没有修改 Nginx。" \
    "反代片段：$install_dir/nginx-location.conf" \
    "管理地址 / 管理员 iframe URL：$public_url/admin/" \
    "普通用户说明 iframe URL：$public_url/" \
    "初始管理密钥文件：$install_dir/secrets/admin-initial-login.txt（0600；在自己的安全编辑器中读取）" \
    '自行在安全终端读取（不要把输出发到聊天）：' \
    "cat -- $install_dir/secrets/admin-initial-login.txt" \
    '首次管理员账号：admin；密码为文件中的完整管理密钥一行。后台可更改账号和密码。不要发给 Agent、客服、聊天或写进 URL。' \
    '更换后删除本地初始文件副本；停止/卸载前先备份，不删除数据卷。' \
    '完整步骤及公网验收：docs/STANDALONE-DOCKER.zh.md；Agent 部署：docs/AGENT-DEPLOY.zh.md。'
