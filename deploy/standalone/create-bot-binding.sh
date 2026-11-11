#!/usr/bin/env bash
set +x
set -Eeuo pipefail
umask 077

install_dir=''
confirm_revoked=false
while (( $# )); do
    case "$1" in
        --install-dir) (( $# >= 2 )) || { printf '缺少 --install-dir 值。\n' >&2; exit 2; }; install_dir=$2; shift 2 ;;
        --confirm-prior-invite-revoked) [[ "$confirm_revoked" == false ]] || exit 2; confirm_revoked=true; shift ;;
        -h|--help) printf '用法：bash deploy/standalone/create-bot-binding.sh --install-dir ABSOLUTE_PATH [--confirm-prior-invite-revoked]\n仅输出私有绑定文件路径，不输出邀请凭据。确认选项仅供管理员核实并撤销不确定的旧邀请后使用，脚本不会代替服务器核实撤销。\n'; exit 0 ;;
        *) printf '未知参数。\n' >&2; exit 2 ;;
    esac
done

fail() { printf 'Bot 绑定包未生成：%s\n' "$*" >&2; exit 1; }
[[ "$install_dir" =~ ^/[A-Za-z0-9_./-]+$ && "$install_dir" != */../* && "$install_dir" != */./* ]] || fail '安装目录必须是规范绝对路径。'
[[ -d "$install_dir" && ! -L "$install_dir" ]] || fail '安装目录不存在或是符号链接。'
[[ -f "$install_dir/.env" && ! -L "$install_dir/.env" ]] || fail '部署配置不存在或不安全。'
grep -Fxq 'SALCARA_HUB_MODE=bot' "$install_dir/.env" || fail '部署不是 Bot 模式。'
public_url=$(sed -n 's/^SALCARA_HUB_PUBLIC_URL=//p' "$install_dir/.env")
[[ "$public_url" =~ ^https://([A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?)(:443)?/salcara-hub$ ]] || fail '公开 HTTPS 地址无效。'
origin=${public_url%/salcara-hub}
package_file="$install_dir/bot-binding.json"
pending_file="$install_dir/.bot-binding-pending"

valid_package_file() {
    local path=$1
    grep -Fq '"type":"salcara-hub-bot-binding"' "$path" &&
        grep -Eq '"version":1([,}])' "$path" &&
        grep -Fq "\"hubUrl\":\"$public_url\"" "$path" &&
        grep -Eq '"groupId":"[A-Za-z0-9._-]{1,128}"' "$path" &&
        grep -Eq '"invite":"[a-f0-9]{64}"' "$path" &&
        grep -Eq '"signature":"[a-f0-9]{64}"' "$path"
}

command -v flock >/dev/null 2>&1 || fail '缺少 flock，无法安全避免重复签发。'
lock_file="$install_dir/.bot-binding.lock"
[[ ! -e "$lock_file" && ! -L "$lock_file" || -f "$lock_file" && ! -L "$lock_file" ]] || fail '绑定包锁路径不安全。'
exec 9> "$lock_file"
flock -n 9 || fail '另一个 Bot 绑定包操作正在运行。'

if [[ -e "$package_file" || -L "$package_file" ]]; then
    [[ -f "$package_file" && ! -L "$package_file" && "$(stat -c '%a' "$package_file")" == 600 ]] || fail '已有绑定包文件不安全；不会覆盖。'
    valid_package_file "$package_file" || fail '已有绑定包格式或 Hub 地址不匹配；不会覆盖或手改签名 JSON。迁址后的新首次绑定须由管理员签发新邀请；已绑定设备按同身份恢复。'
    [[ "$confirm_revoked" == false ]] || fail '已有绑定文件时不接受重新签发确认；请先在管理页核对原组，不自动覆盖。'
    printf '%s\n' '复用私有绑定文件；仅检查本地格式和地址，未验证签名或邀请是否仍有效。请勿将内容粘贴到聊天或日志。' >&2
    printf '%s\n' "$package_file"
    exit 0
fi

command -v curl >/dev/null 2>&1 || fail '缺少 curl。'
if [[ -e "$pending_file" || -L "$pending_file" ]]; then
    [[ -f "$pending_file" && ! -L "$pending_file" && "$(stat -c '%a' "$pending_file")" == 600 ]] || fail '签发结果标记不安全；请人工核对，不自动更改。'
    [[ "$confirm_revoked" == true ]] || fail '先前绑定包签发结果未知，禁止自动重试。请在管理页核对并安全保存已有包；仅在确认旧邀请已撤销后显式使用 --confirm-prior-invite-revoked。不要直接删除标记或重建数据卷。'
    # Keep the marker until local preparation and login also succeed.
elif [[ "$confirm_revoked" == true ]]; then
    fail '没有待核对的签发请求，不接受重新签发确认。'
fi
[[ -d "$install_dir/secrets" && ! -L "$install_dir/secrets" ]] || fail '初始密钥目录不存在或不安全；请在管理页生成绑定包。'
[[ -f "$install_dir/secrets/admin-initial-login.txt" && ! -L "$install_dir/secrets/admin-initial-login.txt" ]] || fail '初始管理密钥文件不存在或不安全；请在管理页生成绑定包。'
[[ "$(stat -c '%a' "$install_dir/secrets/admin-initial-login.txt")" == 600 ]] || fail '初始管理密钥文件权限必须为 0600。'
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

status=$(curl --disable --silent --show-error --proto '=https' --tlsv1.2 --max-time 20 \
    -H "Origin: $origin" -H 'Content-Type: application/json' \
    -b "$tmp_dir/cookie.txt" -c "$tmp_dir/cookie.txt" \
    --data-binary "@$tmp_dir/request.json" -o "$tmp_dir/login.json" -w '%{http_code}' \
    "$public_url/_admin/v1/auth/login") || fail '管理登录请求失败；检查公开 HTTPS 和反代后重试。'
[[ "$status" == 200 ]] || fail "管理登录返回 HTTP $status；若管理密钥已更换，请在管理页手动生成绑定包。"
csrf=$(sed -n 's/.*"csrf_token":"\([A-Za-z0-9_-]\{32,128\}\)".*/\1/p' "$tmp_dir/login.json")
[[ "$csrf" =~ ^[A-Za-z0-9_-]{32,128}$ ]] || fail '管理登录响应缺少有效会话校验值。'

# The request may commit before curl sees a response. Persist uncertainty before
# sending it; ordinary install/resume must never blindly create another group.
if [[ "$confirm_revoked" == true ]]; then
    rm -- "$pending_file" || fail '无法清除已人工确认的签发结果标记。'
fi
(set -o noclobber; printf 'Bot invite request outcome requires confirmation; no credentials are stored here.\n' > "$pending_file") || fail '无法安全记录签发状态；未发送签发请求。'
chmod 600 -- "$pending_file"
status=$(curl --disable --silent --show-error --proto '=https' --tlsv1.2 --max-time 20 \
    -H "Origin: $origin" -H "X-Salcara-CSRF: $csrf" -H 'Content-Type: application/json' \
    -b "$tmp_dir/cookie.txt" -c "$tmp_dir/cookie.txt" \
    --data-binary '{"confirm":true}' -o "$tmp_dir/package.json" -w '%{http_code}' \
    "$public_url/_admin/v1/bot/invites") || fail 'Bot 绑定包签发结果未知；已保留标记，禁止自动重新签发，请在管理页核对。'
[[ "$status" == 201 ]] || fail "Bot 绑定包签发返回 HTTP $status；保留标记，须在管理页核对后恢复。"
valid_package_file "$tmp_dir/package.json" || fail 'Hub 返回的绑定包不符合预期本地格式；这不是签名验证，已保留结果未知标记。'
chmod 600 -- "$tmp_dir/package.json"
ln -T -- "$tmp_dir/package.json" "$package_file" || fail '绑定包已由 Hub 签发但无法安全保存；保留标记，不覆盖已有文件或重新初始化，请使用管理页检查 Bot 组。'
rm -- "$pending_file" || fail '绑定包已保存，但签发标记清理未完成；再次运行仅复用已有文件。'
printf '%s\n' '绑定包含邀请凭据，已保存为私有文件；请勿粘贴到聊天或日志。传输和真实任务仍需独立验收。' >&2
printf '%s\n' "$package_file"
