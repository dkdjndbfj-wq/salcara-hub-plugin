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
        'pull ghcr.io/dkdjndbfj-wq/salcara-hub-standalone:0.5.3')
            printf 'docker:pull\n' >> "$mock_calls"
            [[ "$mock_mode" != registry_pull_failure ]] ;;
        'compose version') : ;;
        'ps -aq') : ;;
        'volume inspect') printf 'test-volume\n' ;;
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
                '{{ index .Config.Labels "org.opencontainers.image.version" }}') printf '0.5.3\n' ;;
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
        'https://api.github.com/repos/dkdjndbfj-wq/salcara-hub-plugin/releases/tags/standalone-v0.5.3')
            printf '{"tag_name":"standalone-v0.5.3","draft":false,"prerelease":false}\n' ;;
        'https://bot.example.com/salcara-hub/_admin/v1/auth/login')
            [[ "$origin" == 'https://bot.example.com' && "$body_file" == @* ]] || return 99
            grep -Fq "$test_secret" "${body_file#@}" || return 99
            [[ -n "$output_file" ]] && printf '{"csrf_token":"%s"}\n' "$csrf_value" > "$output_file"
            printf '200' ;;
        'https://bot.example.com/salcara-hub/_admin/v1/bot/invites')
            [[ "$origin" == 'https://bot.example.com' && "$csrf" == "$csrf_value" && "$body_file" == '{"confirm":true}' ]] || return 99
            [[ -n "$output_file" ]] && printf '%s\n' "$package" > "$output_file"
            printf '201' ;;
        'http://127.0.0.1:8787/healthz')
            printf '{"ok":true,"product":"salcara-hub-standalone","version":"0.5.3"}\n' ;;
        'https://bot.example.com/salcara-hub/v1/ping')
            [[ "$mock_mode" != bad_public ]] || return 22
            printf '{"service":"salcara-hub"}\n' ;;
        'https://bot.example.com/salcara-hub/v1/bridge/stream?deviceId='*) printf '403' ;;
        'https://bot.example.com/salcara-hub/v1/app/events?deviceId='*) printf '403' ;;
        *) printf 'UNEXPECTED_CURL_ENDPOINT\n' >&2; return 99 ;;
    esac
}
export -f uname ss sleep flock git docker curl

install_dir="$test_sandbox/install"
mkdir -m 700 -- "$install_dir" "$install_dir/secrets"
printf '%s\n' 'SALCARA_HUB_MODE=bot' 'SALCARA_HUB_IMAGE_VERSION=0.5.3' 'SALCARA_HUB_PROJECT=salcara-hub-bot' 'SALCARA_HUB_DATA_VOLUME=salcara-hub-bot_hub-data' 'SALCARA_HUB_PUBLIC_URL=https://bot.example.com/salcara-hub' > "$install_dir/.env"
printf '%s\n' "$test_secret" > "$install_dir/secrets/admin-initial-login.txt"
chmod 600 -- "$install_dir/.env" "$install_dir/secrets/admin-initial-login.txt"
printf 'name: salcara-hub-bot\n' > "$install_dir/compose.release.yml"

first=$(bash "$helper" --install-dir "$install_dir")
[[ "$first" == "$package" ]]
[[ "$(stat -c '%a' "$install_dir/bot-binding.json")" == 600 ]]
[[ "$first" != *"$test_secret"* ]]
calls_before=$(wc -l < "$mock_calls")
second=$(bash "$helper" --install-dir "$install_dir")
[[ "$second" == "$first" && "$(wc -l < "$mock_calls")" == "$calls_before" ]]
printf 'PASS: binding package is private, validates and reuses without another invite\n'

resumed=$(bash "$resume" --install-dir "$install_dir")
[[ "$resumed" == *"$package"* ]]
! grep -Fxq 'compose:run' "$mock_calls"
grep -Fxq 'compose:up' "$mock_calls"
printf 'PASS: resume reuses the Bot volume and never initializes it again\n'

success_dir="$test_sandbox/first-install"
set +e
success_output=$(bash "$script_dir/install.sh" --mode bot --public-url https://bot.example.com/salcara-hub --install-dir "$success_dir" 2>&1)
success_status=$?
set -e
[[ $success_status -eq 0 && "$success_output" == *"$package"* && "$success_output" == *'版本 0.5.3'* ]] || { printf 'FAIL: first Bot install (%s)\n%s\n' "$success_status" "$success_output" >&2; exit 1; }
[[ "$success_output" != *"$test_secret"* ]]
[[ "$(<"$success_dir/bot-binding.json")" == "$package" ]]
[[ "$(stat -c '%a' "$success_dir/bot-binding.json")" == 600 ]]
grep -Fxq 'SALCARA_HUB_IMAGE_VERSION=0.5.3' "$success_dir/.env"
[[ -f "$success_dir/resume-bot.sh" && -f "$success_dir/create-bot-binding.sh" ]]
printf 'PASS: first Bot install checks the full public path and emits its signed binding package\n'

mock_mode=registry_pull_failure
missing_image_dir="$test_sandbox/missing-image"
set +e
missing_image_output=$(bash "$script_dir/install.sh" --mode bot --public-url https://bot.example.com/salcara-hub --install-dir "$missing_image_dir" 2>&1)
missing_image_status=$?
set -e
[[ $missing_image_status -ne 0 && "$missing_image_output" == *'尚未创建部署目录或初始化管理密钥'* && ! -e "$missing_image_dir" ]]
printf 'PASS: missing Bot image fails before creating deployment state and never falls back to Station\n'

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
printf 'PASS: failed source build stops before creating deployment state or initializing data\n'

calls_before_dirty_source=$(wc -l < "$mock_calls")
mock_mode=dirty_source
set +e
dirty_failure=$(bash "$script_dir/install.sh" --mode bot --source-build --public-url https://bot.example.com/salcara-hub --install-dir "$test_sandbox/dirty-source" 2>&1)
dirty_status=$?
set -e
[[ $dirty_status -ne 0 && "$dirty_failure" == *'源码工作树有未提交或未跟踪改动'* && ! -e "$test_sandbox/dirty-source" ]]
[[ "$(wc -l < "$mock_calls")" == "$calls_before_dirty_source" ]]
printf 'PASS: source fallback rejects a dirty checkout before building\n'

mock_mode=normal
source_success_dir="$test_sandbox/source-success"
calls_before_source_success=$(wc -l < "$mock_calls")
source_output=$(bash "$script_dir/install.sh" --mode bot --source-build --public-url https://bot.example.com/salcara-hub --install-dir "$source_success_dir")
[[ "$source_output" == *"$package"* ]]
grep -Fxq 'SALCARA_HUB_BUILD_MODE=source' "$source_success_dir/.env"
grep -Fxq 'SALCARA_HUB_SOURCE_COMMIT=0123456789abcdef0123456789abcdef01234567' "$source_success_dir/.env"
grep -Fxq 'SALCARA_HUB_LOCAL_IMAGE_ID=sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc' "$source_success_dir/.env"
[[ -f "$source_success_dir/bot-image.tar.gz" && "$(stat -c '%a' "$source_success_dir/bot-image.tar.gz")" == 600 ]]
source_success_calls=$(tail -n +$((calls_before_source_success + 1)) "$mock_calls")
[[ "$source_success_calls" != *'compose:pull'* && "$source_success_calls" != *'docker:pull'* && "$source_success_calls" != *'api.github.com'* ]]
printf 'PASS: clean committed source builds, verifies, archives and deploys without registry fallback\n'

mock_mode=source_image_missing
resumed_source=$(bash "$resume" --install-dir "$source_success_dir")
[[ "$resumed_source" == *"$package"* ]]
grep -Fxq 'docker:load' "$mock_calls"
printf 'PASS: source deployment restores the exact image archive and resumes without reinitializing\n'

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
! grep -Fxq 'compose:run' "$mock_calls"
printf 'PASS: failed public probe keeps credentials unexported and never initializes again\n'

printf 'Bot deployment safety tests: 9 passed; all files and services were synthetic.\n'
