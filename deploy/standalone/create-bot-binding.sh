#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

install_dir=''
while (( $# )); do
    case "$1" in
        --install-dir) (( $# >= 2 )) || { printf '缺少 --install-dir 值。\n' >&2; exit 2; }; install_dir=$2; shift 2 ;;
        -h|--help) printf '用法：bash deploy/standalone/create-bot-binding.sh --install-dir ABSOLUTE_PATH\n'; exit 0 ;;
        *) printf '未知参数。\n' >&2; exit 2 ;;
    esac
done

fail() { printf 'Bot 绑定包未生成：%s\n' "$*" >&2; exit 1; }
[[ "$install_dir" =~ ^/[A-Za-z0-9_./-]+$ && "$install_dir" != */../* && "$install_dir" != */./* ]] || fail '安装目录必须是规范绝对路径。'
[[ -d "$install_dir" && ! -L "$install_dir" ]] || fail '安装目录不存在或是符号链接。'
[[ -f "$install_dir/.env" && ! -L "$install_dir/.env" ]] || fail '部署配置不存在或不安全。'
[[ -f "$install_dir/secrets/admin-initial-login.txt" && ! -L "$install_dir/secrets/admin-initial-login.txt" ]] || fail '初始管理密钥文件不存在或不安全。'
[[ "$(stat -c '%a' "$install_dir/secrets/admin-initial-login.txt")" == 600 ]] || fail '初始管理密钥文件权限必须为 0600。'
grep -Fxq 'SALCARA_HUB_MODE=bot' "$install_dir/.env" || fail '部署不是 Bot 模式。'
public_url=$(sed -n 's/^SALCARA_HUB_PUBLIC_URL=//p' "$install_dir/.env")
[[ "$public_url" =~ ^https://([A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?)(:443)?/salcara-hub$ ]] || fail '公开 HTTPS 地址无效。'
origin=${public_url%/salcara-hub}
package_file="$install_dir/bot-binding.json"

valid_package_file() {
    local path=$1
    grep -Fq '"type":"salcara-hub-bot-binding"' "$path" &&
        grep -Eq '"version":1([,}])' "$path" &&
        grep -Fq "\"hubUrl\":\"$public_url\"" "$path" &&
        grep -Eq '"groupId":"[A-Za-z0-9._-]{1,128}"' "$path" &&
        grep -Eq '"invite":"[a-f0-9]{64}"' "$path" &&
        grep -Eq '"signature":"[a-f0-9]{64}"' "$path"
}

if [[ -e "$package_file" || -L "$package_file" ]]; then
    [[ -f "$package_file" && ! -L "$package_file" && "$(stat -c '%a' "$package_file")" == 600 ]] || fail '已有绑定包文件不安全；不会覆盖。'
    valid_package_file "$package_file" || fail '已有绑定包格式或 Hub 地址不匹配；不会覆盖。'
    cat -- "$package_file"
    exit 0
fi

command -v curl >/dev/null 2>&1 || fail '缺少 curl。'
command -v flock >/dev/null 2>&1 || fail '缺少 flock，无法安全避免重复签发。'
lock_file="$install_dir/.bot-binding.lock"
[[ ! -e "$lock_file" && ! -L "$lock_file" || -f "$lock_file" && ! -L "$lock_file" ]] || fail '绑定包锁路径不安全。'
exec 9> "$lock_file"
flock -n 9 || fail '另一个 Bot 绑定包操作正在运行。'
tmp_dir=$(mktemp -d "$install_dir/.bot-binding.XXXXXXXX")
cleanup() {
    local status=$?
    if [[ -n "$tmp_dir" && -d "$tmp_dir" && ! -L "$tmp_dir" ]]; then
        rm -f -- "$tmp_dir/request.json" "$tmp_dir/login.json" "$tmp_dir/cookie.txt" "$tmp_dir/package.json" "$tmp_dir/package.new"
        rmdir -- "$tmp_dir" 2>/dev/null || true
    fi
    return "$status"
}
trap cleanup EXIT

password=''
IFS= read -r password < "$install_dir/secrets/admin-initial-login.txt" || fail '无法读取本地初始密钥。'
[[ ${#password} -ge 16 && "$password" =~ ^[A-Za-z0-9_-]+$ ]] || fail '本地初始密钥格式不匹配。'
printf '{"password":"%s"}\n' "$password" > "$tmp_dir/request.json"
unset password

status=$(curl --silent --show-error --proto '=https' --tlsv1.2 --max-time 20 \
    -H "Origin: $origin" -H 'Content-Type: application/json' \
    -b "$tmp_dir/cookie.txt" -c "$tmp_dir/cookie.txt" \
    --data-binary "@$tmp_dir/request.json" -o "$tmp_dir/login.json" -w '%{http_code}' \
    "$public_url/_admin/v1/auth/login") || fail '管理登录请求失败；检查公开 HTTPS 和反代后重试。'
[[ "$status" == 200 ]] || fail "管理登录返回 HTTP $status；若管理密钥已更换，请在管理页手动生成绑定包。"
csrf=$(sed -n 's/.*"csrf_token":"\([A-Za-z0-9_-]\{32,128\}\)".*/\1/p' "$tmp_dir/login.json")
[[ "$csrf" =~ ^[A-Za-z0-9_-]{32,128}$ ]] || fail '管理登录响应缺少有效会话校验值。'

status=$(curl --silent --show-error --proto '=https' --tlsv1.2 --max-time 20 \
    -H "Origin: $origin" -H "X-Salcara-CSRF: $csrf" -H 'Content-Type: application/json' \
    -b "$tmp_dir/cookie.txt" -c "$tmp_dir/cookie.txt" \
    --data-binary '{"confirm":true}' -o "$tmp_dir/package.json" -w '%{http_code}' \
    "$public_url/_admin/v1/bot/invites") || fail 'Bot 绑定包签发请求失败；保留部署现场后可续跑。'
[[ "$status" == 201 ]] || fail "Bot 绑定包签发返回 HTTP $status。"
valid_package_file "$tmp_dir/package.json" || fail 'Hub 返回的 Bot 绑定包未通过完整性检查。'
chmod 600 -- "$tmp_dir/package.json"
mv -- "$tmp_dir/package.json" "$package_file" || fail '绑定包已由 Hub 签发但无法安全保存；不要再次初始化，请使用管理页检查 Bot 组。'
cat -- "$package_file"
