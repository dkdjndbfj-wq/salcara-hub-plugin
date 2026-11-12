#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

readonly script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
readonly helper="$script_dir/create-bot-binding.sh"
readonly resume="$script_dir/resume-bot.sh"
test_sandbox=$(mktemp -d "${TMPDIR:-/tmp}/salcara-bot-deploy-test.XXXXXXXX")
test_sandbox=$(realpath -- "$test_sandbox")
readonly test_sandbox
readonly sandbox_parent=$(dirname -- "$test_sandbox")
readonly test_secret='SYNTHETIC_TEST_ONLY_ADMIN_KEY_1234567890'
readonly csrf_value='SyntheticCSRFValue_012345678901234567890123456789'
readonly package='{"type":"salcara-hub-bot-binding","version":1,"hubUrl":"https://bot.example.com/salcara-hub","groupId":"0123456789abcdef0123456789abcdef","invite":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","signature":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}'
mock_calls="$test_sandbox/calls"
stub_data="$test_sandbox/mock-volume"
mock_git_root=$(cd -- "$script_dir/../.." && pwd -P)
mock_image_restored="$test_sandbox/image-restored"
mock_mode=normal
passed=0
pass() { passed=$((passed + 1)); printf 'PASS: %s\n' "$1"; }
assert_private_output() {
    [[ "$1" != *"$test_secret"* && "$1" != *"$csrf_value"* && "$1" != *'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'* && "$1" != *'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb'* && "$1" != *UNEXPECTED_* ]] || {
        printf 'FAIL: output leaked a synthetic credential or reached an unexpected operation\n' >&2; exit 1;
    }
}
assert_route_only_report() {
    local report=$1 expected=$2
    [[ -f "$report" && "$(stat -c '%a' "$report")" == 600 ]]
    grep -Fq '"scope":"unauthenticated-public-route-only"' "$report"
    grep -Fq "\"routeProbeStatus\":\"$expected\"" "$report"
    grep -Fq '"readyForTasks":false' "$report"
    local field
    for field in authenticatedSSE authenticatedLongPoll proxyBuffering taskRoundTrip; do
        grep -Fq "\"$field\":\"not_tested\"" "$report"
    done
    assert_private_output "$(<"$report")"
}
mkdir -m 700 -- "$stub_data"
export test_sandbox test_secret csrf_value package mock_calls mock_mode stub_data mock_git_root mock_image_restored

cleanup() {
    local status=$?
    if [[ -d "$test_sandbox" && ! -L "$test_sandbox" &&
          "$(realpath -- "$test_sandbox")" == "$test_sandbox" &&
          "$(dirname -- "$test_sandbox")" == "$sandbox_parent" &&
          "$(basename -- "$test_sandbox")" =~ ^salcara-bot-deploy-test\.[A-Za-z0-9]{8}$ ]]; then
        rm -rf -- "$test_sandbox"
    else
        printf 'Refusing unsafe test cleanup: %s\n' "$test_sandbox" >&2
        return 1
    fi
    return "$status"
}
trap cleanup EXIT

uname() { printf 'Linux\n'; }
ss() { :; }
sleep() { :; }
flock() { [[ "$*" == '-n 9' ]]; }
ln() {
    if [[ "$mock_mode" == save_conflict ]]; then
        local target=${!#}
        [[ "$target" == "$test_sandbox/"* ]] || return 99
        printf 'synthetic concurrent existing file\n' > "$target"
        chmod 600 -- "$target"
    fi
    command ln "$@"
}
git() {
    [[ "$1" == -C && "$2" == "$mock_git_root" ]] || return 99
    shift 2
    case "$*" in
        'rev-parse --show-toplevel') printf '%s\n' "$mock_git_root" ;;
        'status --porcelain --untracked-files=all')
            [[ "$mock_mode" != dirty_source ]] || printf ' M internal/hub/bot.go\n' ;;
        'rev-parse --verify HEAD') printf '0123456789abcdef0123456789abcdef01234567\n' ;;
        cat-file\ -e\ 0123456789abcdef0123456789abcdef01234567:*) : ;;
        'archive --format=tar 0123456789abcdef0123456789abcdef01234567') printf 'synthetic source archive\n' ;;
        *) printf 'UNEXPECTED_GIT_OPERATION\n' >&2; return 99 ;;
    esac
}
docker() {
    case "$1 ${2:-}" in
        'context show') printf 'stub-local\n' ;;
        'context inspect') printf 'unix:///test-only/docker.sock\n' ;;
        'info --format') printf 'linux\n' ;;
        'pull ghcr.io/dkdjndbfj-wq/salcara-hub-standalone:0.5.6')
            printf 'docker:pull\n' >> "$mock_calls"
            [[ "$mock_mode" != registry_pull_failure ]] ;;
        'compose version') : ;;
        'ps -aq') : ;;
        'volume inspect') [[ "$mock_mode" != missing_volume ]] || return 1; printf 'test-volume\n' ;;
        'volume ls') : ;;
        'inspect --format') printf 'salcara-hub-bot\n' ;;
        compose\ *)
            shift
            while (( $# )); do
                case "$1" in
                    --project-name|--project-directory|--env-file|-f) shift 2 ;;
                    *) break ;;
                esac
            done
            local action=${1:-}
            printf 'compose:%s\n' "$action" >> "$mock_calls"
            case "$action" in
                config|pull|up) : ;;
                ps) printf 'test-container-id\n' ;;
                run)
                    [[ "$*" == 'run --rm --no-deps hub -init' ]] || return 99
                    printf '%s\n' "$test_secret" > "$stub_data/admin-initial-login.txt"
                    chmod 600 -- "$stub_data/admin-initial-login.txt" ;;
                cp)
                    local destination=${!#}
                    [[ "$destination" == "$test_sandbox/"* ]] || return 99
                    cp -- "$stub_data/admin-initial-login.txt" "$destination" ;;
                *) printf 'UNEXPECTED_DOCKER_OPERATION\n' >&2; return 99 ;;
            esac ;;
        'build --pull')
            printf 'docker:build\n' >> "$mock_calls"
            cat >/dev/null
            [[ "$mock_mode" != source_build_failure ]] ;;
        'image inspect')
            local format=${4:-}
            case "$format" in
                '{{.Id}}')
                    if [[ "$mock_mode" == source_image_missing && ! -f "$mock_image_restored" ]]; then return 1; fi
                    printf 'sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc\n' ;;
                '{{ index .Config.Labels "org.opencontainers.image.version" }}') printf '0.5.6\n' ;;
                *) printf 'UNEXPECTED_IMAGE_INSPECT\n' >&2; return 99 ;;
            esac ;;
        save\ *)
            printf 'docker:save\n' >> "$mock_calls"
            printf 'synthetic image archive\n' ;;
        load*)
            cat >/dev/null
            : > "$mock_image_restored"
            printf 'docker:load\n' >> "$mock_calls" ;;
        *) printf 'UNEXPECTED_DOCKER_OPERATION\n' >&2; return 99 ;;
    esac
}
curl() {
    [[ "${1:-}" == --disable ]] || { printf 'UNEXPECTED_CURL_CONFIG\n' >&2; return 99; }
    local endpoint='' output_file='' body_file='' status_format='' origin='' csrf='' arg
    while (( $# )); do
        arg=$1
        case "$arg" in
            -o) output_file=$2; shift 2 ;;
            -w) status_format=$2; shift 2 ;;
            --data-binary) body_file=$2; shift 2 ;;
            -H)
                case "$2" in
                    Origin:*) origin=${2#Origin: } ;;
                    X-Salcara-CSRF:*) csrf=${2#X-Salcara-CSRF: } ;;
                esac
                shift 2 ;;
            *) endpoint=$arg; shift ;;
        esac
    done
    printf '%s\n' "$endpoint" >> "$mock_calls"
    case "$endpoint" in
        'https://api.github.com/repos/dkdjndbfj-wq/salcara-hub-plugin/releases/tags/standalone-v0.5.6')
            printf '{"tag_name":"standalone-v0.5.6","draft":false,"prerelease":false}\n' ;;
        'https://bot.example.com/salcara-hub/_admin/v1/auth/login')
            [[ "$origin" == 'https://bot.example.com' && "$body_file" == @* ]] || return 99
            grep -Fq "$test_secret" "${body_file#@}" || return 99
            [[ -n "$output_file" ]] && printf '{"csrf_token":"%s"}\n' "$csrf_value" > "$output_file"
            printf '200' ;;
        'https://bot.example.com/salcara-hub/_admin/v1/bot/invites')
            [[ "$origin" == 'https://bot.example.com' && "$csrf" == "$csrf_value" && "$body_file" == '{"confirm":true}' ]] || return 99
            [[ "$mock_mode" != invite_timeout ]] || return 28
            if [[ "$mock_mode" == invite_bad_response ]]; then printf 'not JSON\n' > "$output_file"; printf '201'; return; fi
            [[ -n "$output_file" ]] && printf '%s\n' "$package" > "$output_file"
            printf '201' ;;
        'http://127.0.0.1:8787/healthz')
            printf '{"ok":true,"product":"salcara-hub-standalone","version":"0.5.6"}\n' ;;
        'https://bot.example.com/salcara-hub/v1/ping')
            [[ "$mock_mode" != bad_public ]] || return 22
            printf '{"service":"salcara-hub"}\n' ;;
        'https://bot.example.com/salcara-hub/v1/bridge/stream?deviceId='*)
            [[ -z "$origin$csrf$body_file" ]] || return 99
            case "$mock_mode" in stream_404) printf '404';; stream_200) printf '200';; stream_timeout) return 28;; *) printf '403';; esac ;;
        'https://bot.example.com/salcara-hub/v1/app/events?deviceId='*)
            [[ -z "$origin$csrf$body_file" ]] || return 99
            case "$mock_mode" in events_404) printf '404';; events_200) printf '200';; events_timeout) return 28;; *) printf '403';; esac ;;
        *) printf 'UNEXPECTED_CURL_ENDPOINT\n' >&2; return 99 ;;
    esac
}
export -f uname ss sleep flock ln git docker curl

install_dir="$test_sandbox/install"
mkdir -m 700 -- "$install_dir" "$install_dir/secrets"
printf '%s\n' 'SALCARA_HUB_MODE=bot' 'SALCARA_HUB_IMAGE_VERSION=0.5.6' 'SALCARA_HUB_PROJECT=salcara-hub-bot' 'SALCARA_HUB_DATA_VOLUME=salcara-hub-bot_hub-data' 'SALCARA_HUB_PUBLIC_URL=https://bot.example.com/salcara-hub' > "$install_dir/.env"
printf '%s\n' "$test_secret" > "$install_dir/secrets/admin-initial-login.txt"
chmod 600 -- "$install_dir/.env" "$install_dir/secrets/admin-initial-login.txt"
printf 'name: salcara-hub-bot\n' > "$install_dir/compose.release.yml"

first=$(bash "$helper" --install-dir "$install_dir")
[[ "$first" == "$install_dir/bot-binding.json" ]]
[[ "$(stat -c '%a' "$install_dir/bot-binding.json")" == 600 ]]
assert_private_output "$first"
calls_before=$(wc -l < "$mock_calls")
second=$(bash "$helper" --install-dir "$install_dir")
[[ "$second" == "$first" && "$(wc -l < "$mock_calls")" == "$calls_before" ]]
pass 'binding package remains private; stdout is only a path and reuse never creates another invite'

resumed=$(bash "$resume" --install-dir "$install_dir")
[[ "$resumed" == *"$install_dir/bot-binding.json"* && "$resumed" == *'真实任务仍待验收'* ]]
assert_private_output "$resumed"
assert_route_only_report "$install_dir/bot-deployment-checks.json" expected_http_responses
! grep -Fxq 'compose:run' "$mock_calls"
grep -Fxq 'compose:up' "$mock_calls"
pass 'resume retains the Bot volume; route-only report never claims transport/task acceptance'

success_dir="$test_sandbox/first-install"
set +e
success_output=$(bash "$script_dir/install.sh" --mode bot --public-url https://bot.example.com/salcara-hub --install-dir "$success_dir" 2>&1)
success_status=$?
set -e
assert_private_output "$success_output"
[[ $success_status -eq 0 && "$success_output" == *"$success_dir/bot-binding.json"* && "$success_output" == *'版本 0.5.6'* ]] || { printf 'FAIL: first Bot install (exit %s)\n' "$success_status" >&2; exit 1; }
[[ "$(<"$success_dir/bot-binding.json")" == "$package" ]]
[[ "$(stat -c '%a' "$success_dir/bot-binding.json")" == 600 ]]
grep -Fxq 'SALCARA_HUB_IMAGE_VERSION=0.5.6' "$success_dir/.env"
[[ -f "$success_dir/resume-bot.sh" && -f "$success_dir/create-bot-binding.sh" && -f "$success_dir/check-bot-routes.sh" ]]
assert_route_only_report "$success_dir/bot-deployment-checks.json" expected_http_responses
pass 'first Bot install saves the invitation without logging it and reports only route probes'

mock_mode=registry_pull_failure
missing_image_dir="$test_sandbox/missing-image"
set +e
missing_image_output=$(bash "$script_dir/install.sh" --mode bot --public-url https://bot.example.com/salcara-hub --install-dir "$missing_image_dir" 2>&1)
missing_image_status=$?
set -e
[[ $missing_image_status -ne 0 && "$missing_image_output" == *'尚未创建部署目录或初始化管理密钥'* && ! -e "$missing_image_dir" ]]
pass 'missing Bot image fails before creating deployment state and never falls back to Station'

source_dir="$test_sandbox/source-build"
calls_before_source_failure=$(wc -l < "$mock_calls")
mock_mode=source_build_failure
set +e
source_failure=$(bash "$script_dir/install.sh" --mode bot --source-build --public-url https://bot.example.com/salcara-hub --install-dir "$source_dir" 2>&1)
source_status=$?
set -e
[[ $source_status -ne 0 && "$source_failure" == *'源码镜像构建失败'* && ! -e "$source_dir" ]]
source_failure_calls=$(tail -n +$((calls_before_source_failure + 1)) "$mock_calls")
[[ "$source_failure_calls" != *'compose:run'* ]]
pass 'failed source build stops before creating deployment state or initializing data'

calls_before_dirty_source=$(wc -l < "$mock_calls")
mock_mode=dirty_source
set +e
dirty_failure=$(bash "$script_dir/install.sh" --mode bot --source-build --public-url https://bot.example.com/salcara-hub --install-dir "$test_sandbox/dirty-source" 2>&1)
dirty_status=$?
set -e
[[ $dirty_status -ne 0 && "$dirty_failure" == *'源码工作树有未提交或未跟踪改动'* && ! -e "$test_sandbox/dirty-source" ]]
[[ "$(wc -l < "$mock_calls")" == "$calls_before_dirty_source" ]]
pass 'source fallback rejects a dirty checkout before building'

mock_mode=normal
source_success_dir="$test_sandbox/source-success"
calls_before_source_success=$(wc -l < "$mock_calls")
source_output=$(bash "$script_dir/install.sh" --mode bot --source-build --public-url https://bot.example.com/salcara-hub --install-dir "$source_success_dir")
[[ "$source_output" == *"$source_success_dir/bot-binding.json"* ]]
assert_private_output "$source_output"
grep -Fxq 'SALCARA_HUB_BUILD_MODE=source' "$source_success_dir/.env"
grep -Fxq 'SALCARA_HUB_SOURCE_COMMIT=0123456789abcdef0123456789abcdef01234567' "$source_success_dir/.env"
grep -Fxq 'SALCARA_HUB_LOCAL_IMAGE_ID=sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc' "$source_success_dir/.env"
[[ -f "$source_success_dir/bot-image.tar.gz" && "$(stat -c '%a' "$source_success_dir/bot-image.tar.gz")" == 600 ]]
source_success_calls=$(tail -n +$((calls_before_source_success + 1)) "$mock_calls")
[[ "$source_success_calls" != *'compose:pull'* && "$source_success_calls" != *'docker:pull'* && "$source_success_calls" != *'api.github.com'* ]]
pass 'clean committed source builds, verifies, archives and deploys without registry fallback'

mock_mode=source_image_missing
resumed_source=$(bash "$resume" --install-dir "$source_success_dir")
[[ "$resumed_source" == *"$source_success_dir/bot-binding.json"* ]]
assert_private_output "$resumed_source"
grep -Fxq 'docker:load' "$mock_calls"
pass 'source deployment restores the exact image archive and resumes without reinitializing'

missing_secret_dir="$test_sandbox/recover-secret"
mkdir -m 700 -- "$missing_secret_dir"
cp -- "$install_dir/.env" "$install_dir/compose.release.yml" "$missing_secret_dir/"
mock_mode=bad_public
set +e
failed=$(bash "$resume" --install-dir "$missing_secret_dir" 2>&1)
status=$?
set -e
[[ $status -ne 0 && "$failed" == *'公开 HTTPS /v1/ping 不可用'* ]]
[[ ! -e "$missing_secret_dir/secrets/admin-initial-login.txt" ]]
[[ ! -e "$missing_secret_dir/bot-binding.json" ]]
assert_private_output "$failed"
assert_route_only_report "$missing_secret_dir/bot-deployment-checks.json" failed
pass 'failed public probe keeps credentials unexported and records failure without claiming acceptance'

for mock_mode in stream_404 stream_200 stream_timeout events_404 events_200 events_timeout; do
    calls_before=$(wc -l < "$mock_calls")
    set +e
    rejected=$(bash "$resume" --install-dir "$install_dir" 2>&1)
    status=$?
    set -e
    assert_private_output "$rejected"
    [[ $status -ne 0 && "$rejected" != *'本地健康与无凭据路由探测已完成'* ]]
    assert_route_only_report "$install_dir/bot-deployment-checks.json" failed
    calls_after=$(tail -n +$((calls_before + 1)) "$mock_calls")
    [[ "$calls_after" != *'compose:run'* && "$calls_after" != *'/_admin/'* ]]
    [[ "$(<"$install_dir/bot-binding.json")" == "$package" ]]
    pass "route failure $mock_mode invalidates old report and preserves the private invitation"
done

mock_mode=missing_volume
calls_before=$(wc -l < "$mock_calls")
set +e
missing_volume_output=$(bash "$resume" --install-dir "$install_dir" 2>&1)
status=$?
set -e
[[ $status -ne 0 && "$missing_volume_output" == *'原 Bot 数据卷不存在'* ]]
calls_after=$(tail -n +$((calls_before + 1)) "$mock_calls")
[[ "$calls_after" != *'compose:up'* && "$calls_after" != *'compose:run'* ]]
assert_private_output "$missing_volume_output"
pass 'a missing persistent Bot volume is not silently recreated or initialized'

retry_dir="$test_sandbox/failed-then-resumed"
mock_mode=bad_public
calls_before=$(wc -l < "$mock_calls")
set +e
failed_install=$(bash "$script_dir/install.sh" --mode bot --public-url https://bot.example.com/salcara-hub --install-dir "$retry_dir" 2>&1)
status=$?
set -e
[[ $status -ne 0 && -f "$retry_dir/.env" && ! -e "$retry_dir/bot-binding.json" ]]
assert_private_output "$failed_install"
assert_route_only_report "$retry_dir/bot-deployment-checks.json" failed
mock_mode=normal
retried=$(bash "$retry_dir/resume-bot.sh" --install-dir "$retry_dir" 2>&1)
assert_private_output "$retried"
assert_route_only_report "$retry_dir/bot-deployment-checks.json" expected_http_responses
calls_after=$(tail -n +$((calls_before + 1)) "$mock_calls")
[[ "$(grep -Fc 'compose:run' <<< "$calls_after")" == 1 ]]
[[ "$(<"$retry_dir/bot-binding.json")" == "$package" ]]
pass 'failure after initialization resumes the same deployment without a second init'

changed_dir="$test_sandbox/changed-address"
mkdir -m 700 -- "$changed_dir"
cp -a -- "$install_dir/.env" "$install_dir/secrets" "$install_dir/bot-binding.json" "$changed_dir/"
sed -i 's#https://bot.example.com/#https://new.example.com/#' "$changed_dir/.env"
calls_before=$(wc -l < "$mock_calls")
set +e
changed=$(bash "$helper" --install-dir "$changed_dir" 2>&1)
status=$?
set -e
[[ $status -ne 0 && "$changed" == *'不会覆盖或手改签名 JSON'* && "$(wc -l < "$mock_calls")" == "$calls_before" ]]
[[ "$(<"$changed_dir/bot-binding.json")" == "$package" ]]
assert_private_output "$changed"
pass 'a changed canonical URL never rewrites or sends the old signed invitation'

for mock_mode in invite_timeout invite_bad_response; do
    uncertain_dir="$test_sandbox/$mock_mode"
    mkdir -m 700 -- "$uncertain_dir"
    cp -a -- "$install_dir/.env" "$install_dir/secrets" "$uncertain_dir/"
    set +e
    uncertain=$(bash "$helper" --install-dir "$uncertain_dir" 2>&1)
    status=$?
    set -e
    [[ $status -ne 0 && -f "$uncertain_dir/.bot-binding-pending" && ! -e "$uncertain_dir/bot-binding.json" ]]
    [[ "$(stat -c '%a' "$uncertain_dir/.bot-binding-pending")" == 600 ]]
    assert_private_output "$uncertain"
    calls_before=$(wc -l < "$mock_calls")
    set +e
    blocked=$(bash "$helper" --install-dir "$uncertain_dir" 2>&1)
    status=$?
    set -e
    [[ $status -ne 0 && "$blocked" == *'先前绑定包签发结果未知'* && "$(wc -l < "$mock_calls")" == "$calls_before" ]]
    assert_private_output "$blocked"
    pass "$mock_mode leaves a private unknown-result marker and blocks automatic reissuance"
done
mock_mode=normal
confirmed=$(bash "$helper" --install-dir "$uncertain_dir" --confirm-prior-invite-revoked 2>&1)
[[ ! -e "$uncertain_dir/.bot-binding-pending" && -f "$uncertain_dir/bot-binding.json" ]]
assert_private_output "$confirmed"
pass 'only an explicit operator confirmation permits a new invite after an unknown result'

reused_dir="$test_sandbox/reuse-without-initial-key"
mkdir -m 700 -- "$reused_dir"
cp -a -- "$install_dir/.env" "$install_dir/compose.release.yml" "$install_dir/bot-binding.json" "$reused_dir/"
calls_before=$(wc -l < "$mock_calls")
reused=$(bash "$resume" --install-dir "$reused_dir" 2>&1)
calls_after=$(tail -n +$((calls_before + 1)) "$mock_calls")
[[ "$calls_after" != *'compose:cp'* && "$calls_after" != *'/_admin/'* && ! -e "$reused_dir/secrets" ]]
[[ "$reused" == *"$reused_dir/bot-binding.json"* ]]
assert_private_output "$reused"
pass 'an existing package remains reusable after the user removes the initial admin-key copy'

pending_dir="$test_sandbox/pending-without-initial-key"
mkdir -m 700 -- "$pending_dir"
cp -a -- "$install_dir/.env" "$install_dir/compose.release.yml" "$pending_dir/"
printf 'synthetic unknown outcome\n' > "$pending_dir/.bot-binding-pending"
calls_before=$(wc -l < "$mock_calls")
set +e
blocked=$(bash "$resume" --install-dir "$pending_dir" 2>&1)
status=$?
set -e
calls_after=$(tail -n +$((calls_before + 1)) "$mock_calls")
[[ $status -ne 0 && "$blocked" == *'先前绑定包签发结果未知'* && ! -e "$pending_dir/secrets" ]]
[[ "$calls_after" != *'compose:cp'* && "$calls_after" != *'/_admin/'* ]]
assert_private_output "$blocked"
pass 'unknown issuance stays blocked without exporting credentials even when the initial key is gone'

cp -a -- "$install_dir/secrets" "$pending_dir/"
calls_before=$(wc -l < "$mock_calls")
set +e
blocked=$(bash "$resume" --install-dir "$pending_dir" 2>&1)
status=$?
set -e
calls_after=$(tail -n +$((calls_before + 1)) "$mock_calls")
[[ $status -ne 0 && -f "$pending_dir/.bot-binding-pending" && "$blocked" == *'先前绑定包签发结果未知'* ]]
[[ "$calls_after" != *'compose:cp'* && "$calls_after" != *'/_admin/'* ]]
assert_private_output "$blocked"
pass 'resume never reissues an unknown invitation even when a usable initial key still exists'

conflict_dir="$test_sandbox/package-save-conflict"
mkdir -m 700 -- "$conflict_dir"
cp -a -- "$install_dir/.env" "$install_dir/secrets" "$conflict_dir/"
mock_mode=save_conflict
set +e
conflict=$(bash "$helper" --install-dir "$conflict_dir" 2>&1)
status=$?
set -e
[[ $status -ne 0 && -f "$conflict_dir/.bot-binding-pending" && "$(<"$conflict_dir/bot-binding.json")" == 'synthetic concurrent existing file' ]]
assert_private_output "$conflict"
calls_before=$(wc -l < "$mock_calls")
set +e
conflict_retry=$(bash "$helper" --install-dir "$conflict_dir" 2>&1)
status=$?
set -e
[[ $status -ne 0 && "$(wc -l < "$mock_calls")" == "$calls_before" && -f "$conflict_dir/.bot-binding-pending" ]]
[[ "$(<"$conflict_dir/bot-binding.json")" == 'synthetic concurrent existing file' ]]
assert_private_output "$conflict_retry"
pass 'a save conflict after 201 preserves the existing target and unknown outcome without reissuance'

mock_mode=normal
trace_dir="$test_sandbox/traced-signing"
mkdir -m 700 -- "$trace_dir"
cp -a -- "$install_dir/.env" "$install_dir/secrets" "$trace_dir/"
traced=$(bash -x "$helper" --install-dir "$trace_dir" 2>&1)
assert_private_output "$traced"
[[ -f "$trace_dir/bot-binding.json" ]]
pass 'fresh signing under bash -x does not print admin key, CSRF, invitation or signature'

printf 'Bot deployment safety tests: %s passed; all services/credentials synthetic, no public transport or real task acceptance.\n' "$passed"
