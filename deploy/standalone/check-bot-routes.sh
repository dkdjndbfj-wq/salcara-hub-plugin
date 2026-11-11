#!/usr/bin/env bash
# Unauthenticated diagnostics only; never enroll a device for this check.
set +x
set -Eeuo pipefail
umask 077

install_dir=''
while (( $# )); do
    case "$1" in
        --install-dir) (( $# >= 2 )) || exit 2; [[ -z "$install_dir" ]] || exit 2; install_dir=$2; shift 2 ;;
        -h|--help) printf '用法：bash check-bot-routes.sh --install-dir ABSOLUTE_PATH（仅无凭据路由探测，不是端到端验收）\n'; exit 0 ;;
        *) printf '未知参数。\n' >&2; exit 2 ;;
    esac
done
fail() { printf 'Bot 路由探测未完成：%s\n' "$*" >&2; exit 1; }
[[ "$install_dir" =~ ^/[A-Za-z0-9_./-]+$ && "$install_dir" != */../* && "$install_dir" != */./* ]] || fail '安装目录必须是规范绝对路径。'
[[ -d "$install_dir" && ! -L "$install_dir" && "$(realpath -- "$install_dir")" == "$install_dir" ]] || fail '安装目录不存在或不安全。'
[[ -f "$install_dir/.env" && ! -L "$install_dir/.env" ]] || fail '部署配置不存在或不安全。'
grep -Fxq 'SALCARA_HUB_MODE=bot' "$install_dir/.env" || fail '部署不是 Bot 模式。'
public_url=$(sed -n 's/^SALCARA_HUB_PUBLIC_URL=//p' "$install_dir/.env")
[[ "$public_url" =~ ^https://([A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?)(:443)?/salcara-hub$ ]] || fail '公开 HTTPS 地址无效。'
report="$install_dir/bot-deployment-checks.json"
[[ ! -e "$report" && ! -L "$report" || -f "$report" && ! -L "$report" ]] || fail '检查报告路径不安全。'
tmp_report=$(mktemp "$install_dir/.bot-route-check.XXXXXXXX") || fail '无法创建私有检查报告。'
state=running
ping=not_tested
stream_status=0
event_status=0
write_report() {
    # Values are fixed enums, numeric HTTP codes or the credential-free URL.
    # No response body, invitation, device ID or authentication data is saved.
    printf '{"schema":1,"scope":"unauthenticated-public-route-only","checkedAt":"%s","publicUrl":"%s","routeProbeStatus":"%s","httpsPing":"%s","bridgeRouteHTTP":%s,"appEventsRouteHTTP":%s,"authenticatedSSE":"not_tested","authenticatedLongPoll":"not_tested","proxyBuffering":"not_tested","taskRoundTrip":"not_tested","readyForTasks":false}\n' \
        "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$public_url" "$state" "$ping" "$stream_status" "$event_status" > "$tmp_report"
    chmod 600 -- "$tmp_report"
    mv -fT -- "$tmp_report" "$report"
}
finish() {
    local status=$?
    if [[ "$state" != expected_http_responses ]]; then state=failed; write_report || true; fi
    [[ ! -f "$tmp_report" ]] || rm -f -- "$tmp_report"
    return "$status"
}
trap finish EXIT
# Invalidate a previous successful probe before contacting the public route.
write_report
public_ping=$(curl --disable --fail --silent --show-error --proto '=https' --tlsv1.2 --max-time 10 "$public_url/v1/ping") || fail '公开 HTTPS /v1/ping 不可用；修复反代后再次续跑。'
grep -Eq '"service"[[:space:]]*:[[:space:]]*"salcara-hub"' <<< "$public_ping" || fail '公开 HTTPS 未返回预期的 Hub 服务标识。'
ping=expected_service
stream_status=$(curl --disable --silent --show-error --proto '=https' --tlsv1.2 --max-time 10 -o /dev/null -w '%{http_code}' "$public_url/v1/bridge/stream?deviceId=00000000-0000-4000-8000-000000000000") || { stream_status=0; fail '无法探测 Bridge 路由。'; }
[[ "$stream_status" =~ ^[1-5][0-9]{2}$ ]] || { stream_status=0; fail 'Bridge 路由没有有效 HTTP 状态。'; }
[[ "$stream_status" == 400 || "$stream_status" == 403 ]] || fail "Bridge 无凭据路由返回 HTTP $stream_status，未达到预期认证拒绝。"
event_status=$(curl --disable --silent --show-error --proto '=https' --tlsv1.2 --max-time 10 -o /dev/null -w '%{http_code}' "$public_url/v1/app/events?deviceId=00000000-0000-4000-8000-000000000000&scope=device&wait=1") || { event_status=0; fail '无法探测 App events 路由。'; }
[[ "$event_status" =~ ^[1-5][0-9]{2}$ ]] || { event_status=0; fail 'App events 路由没有有效 HTTP 状态。'; }
[[ "$event_status" == 400 || "$event_status" == 403 ]] || fail "App events 无凭据路由返回 HTTP $event_status，未达到预期认证拒绝。"
state=expected_http_responses
write_report
printf '仅完成无凭据路由探测（400/403 也可能来自代理）；不是端到端验收。\nSSE 持续接收、长轮询唤醒、代理无缓冲、真实任务往返：均未验收。\n无秘密检查记录：%s\n' "$report"
