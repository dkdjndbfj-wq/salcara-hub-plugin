#!/usr/bin/env bash
# Installer regression tests. Docker, curl, ss and sleep are process-local mocks;
# every file/fixture lives inside one dedicated, validated mktemp directory.
set -Eeuo pipefail
umask 077

readonly test_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
readonly installer="$test_dir/install.sh"
test_sandbox=$(mktemp -d "${TMPDIR:-/tmp}/salcara-hub-install-test.XXXXXXXX")
test_sandbox=$(realpath -- "$test_sandbox")
readonly test_sandbox
readonly sandbox_parent=$(dirname -- "$test_sandbox")

cleanup() {
    local status=$?
    # Only remove the exact directory created by this test. Never use an
    # unchecked variable, parent directory, user home or workspace as the target.
    if [[ -d "$test_sandbox" && ! -L "$test_sandbox" &&
          "$(realpath -- "$test_sandbox")" == "$test_sandbox" &&
          "$(dirname -- "$test_sandbox")" == "$sandbox_parent" &&
          "$(basename -- "$test_sandbox")" =~ ^salcara-hub-install-test\.[A-Za-z0-9]{8}$ ]]; then
        rm -rf -- "$test_sandbox"
    else
        printf 'Refusing unsafe test cleanup: %s\n' "$test_sandbox" >&2
        return 1
    fi
    return "$status"
}
trap cleanup EXIT

readonly public_url='https://relay.example.com/salcara-hub'
readonly test_secret='TEST_ONLY_SYNTHETIC_MANAGEMENT_KEY_NOT_A_REAL_SECRET'
stub_calls="$test_sandbox/mock-calls"
stub_data="$test_sandbox/mock-volume"
stub_mode=normal
mkdir -m 700 -- "$stub_data"
export test_sandbox stub_calls stub_data stub_mode test_secret
# Do not let the developer's real context or interpolation settings affect tests.
unset DOCKER_HOST DOCKER_CONTEXT COMPOSE_FILE COMPOSE_PROJECT_NAME
unset SALCARA_HUB_PUBLIC_URL SALCARA_HUB_IMAGE_VERSION SALCARA_HUB_DISABLE_UPDATES

uname() { printf 'Linux\n'; }
ss() { :; }
sleep() { :; }
docker() {
    case "$1 ${2:-}" in
        'context show') printf 'stub-local\n' ;;
        'context inspect')
            if [[ "$stub_mode" == remote ]]; then printf 'ssh://test-only\n'; else printf 'unix:///test-only/docker.sock\n'; fi ;;
        'info --format') printf 'linux\n' ;;
        'ps -aq')
            if [[ "$stub_mode" == existing_container ]]; then printf 'test-only-container\n'; fi ;;
        'volume ls')
            if [[ "$stub_mode" == existing_volume && $# == 3 ]]; then printf 'salcara-hub-standalone_hub-data\n'; fi ;;
        'compose version') : ;;
        compose\ *)
            shift
            while (( $# )); do
                case "$1" in
                    --project-name|--project-directory|--env-file|-f) shift 2 ;;
                    *) break ;;
                esac
            done
            local action=${1:-}
            printf 'compose:%s\n' "$action" >> "$stub_calls"
            case "$action" in
                config) : ;;
                pull) [[ "$stub_mode" != pull_failure ]] ;;
                run)
                    [[ "$*" == 'run --rm --no-deps hub -init' ]] || { printf 'UNEXPECTED_MOCK_OPERATION\n' >&2; return 99; }
                    printf '%s\n' "$test_secret" > "$stub_data/admin-initial-login.txt"
                    chmod 600 -- "$stub_data/admin-initial-login.txt" ;;
                up) : ;;
                cp)
                    local destination=${!#}
                    [[ "$destination" == "$test_sandbox/"* ]] || { printf 'UNEXPECTED_MOCK_OPERATION\n' >&2; return 99; }
                    cp -- "$stub_data/admin-initial-login.txt" "$destination" ;;
                *) printf 'UNEXPECTED_MOCK_OPERATION\n' >&2; return 99 ;;
            esac ;;
        *) printf 'UNEXPECTED_MOCK_OPERATION\n' >&2; return 99 ;;
    esac
}
curl() {
    local endpoint=${!#}
    case "$endpoint" in
        'https://api.github.com/repos/dkdjndbfj-wq/salcara-hub-plugin/releases/tags/standalone-v0.5.0')
            [[ "$stub_mode" != missing_release ]] || return 22
            printf '{"tag_name":"standalone-v0.5.0","draft":false,"prerelease":false}\n' ;;
        'http://127.0.0.1:8787/healthz')
            if [[ "$stub_mode" == wrong_health ]]; then
                printf '{"ok":true,"product":"salcara-hub-standalone","version":"0.4.0"}\n'
            else
                printf '{"ok":true,"product":"salcara-hub-standalone","version":"0.5.0"}\n'
            fi ;;
        *) printf 'UNEXPECTED_MOCK_OPERATION\n' >&2; return 99 ;;
    esac
}
export -f uname ss sleep docker curl

test_target="$test_sandbox/installation"
passed=0
assert_safe_output() {
    [[ "$1" != *"$test_secret"* && "$1" != *UNEXPECTED_MOCK_OPERATION* ]] || {
        printf 'FAIL: installer printed a secret or invoked an unexpected operation\n' >&2; return 1;
    }
}
expect_failure() {
    local label=$1 expected=$2; shift 2
    local output status
    set +e
    output=$(bash "$installer" "$@" 2>&1)
    status=$?
    set -e
    assert_safe_output "$output"
    [[ $status -ne 0 && "$output" == *"$expected"* && "$output" != *'Hub 容器已就绪'* ]] || {
        printf 'FAIL: %s (exit %s)\n%s\n' "$label" "$status" "$output" >&2; return 1;
    }
    [[ ! -e "$test_target" && ! -e "$stub_calls" ]] || {
        printf 'FAIL: preflight refusal created files or reached a Compose mutation\n' >&2; return 1;
    }
    passed=$((passed + 1))
    printf 'PASS: %s\n' "$label"
}

bash -n "$installer"
help_output=$(bash "$installer" --help)
assert_safe_output "$help_output"
[[ "$help_output" == *'0.5.0'* && "$help_output" == *'--public-url'* ]] || exit 1

expect_failure 'noninteractive missing flags' '非交互运行必须提供' </dev/null
expect_failure 'root path rejected' '安装目录必须' --public-url "$public_url" --install-dir /
expect_failure 'broad path rejected' '请选择一个专门的新子目录' --public-url "$public_url" --install-dir /tmp
expect_failure 'userinfo URL rejected' 'URL 必须为' --public-url 'https://name:synthetic@relay.example.com/salcara-hub' --install-dir "$test_target"
expect_failure 'query URL rejected' 'URL 必须为' --public-url 'https://relay.example.com/salcara-hub?token=synthetic' --install-dir "$test_target"
expect_failure 'path traversal rejected' '路径不能包含' --public-url "$public_url" --install-dir "$test_sandbox/../unexpected"
stub_mode=remote
expect_failure 'remote Docker refused' '仅支持本机 Unix socket' --public-url "$public_url" --install-dir "$test_target"
stub_mode=existing_container
expect_failure 'existing Hub container refused' '已有本项目容器' --public-url "$public_url" --install-dir "$test_target"
stub_mode=existing_volume
expect_failure 'existing Hub volume refused' '已有 Hub 数据卷' --public-url "$public_url" --install-dir "$test_target"
stub_mode=missing_release
expect_failure 'missing Release fails before files/init' '正式 Release 不存在或不可达' --public-url "$public_url" --install-dir "$test_target"

# Test initialization gating after a mocked network failure. All filesystem
# writes below are inside this test's new, dedicated sandbox.
stub_mode=pull_failure
set +e
pull_output=$(bash "$installer" --public-url "$public_url" --install-dir "$test_sandbox/pull-failure" 2>&1)
pull_status=$?
set -e
assert_safe_output "$pull_output"
[[ $pull_status -ne 0 && "$pull_output" == *'没有初始化管理密钥'* && "$pull_output" != *'Hub 容器已就绪'* ]] || exit 1
! grep -Fxq 'compose:run' "$stub_calls"
[[ ! -e "$stub_data/admin-initial-login.txt" ]] || exit 1
passed=$((passed + 1)); printf 'PASS: image pull failure never initializes\n'

stub_mode=wrong_health
set +e
health_output=$(bash "$installer" --public-url "$public_url" --install-dir "$test_sandbox/wrong-health" 2>&1)
health_status=$?
set -e
assert_safe_output "$health_output"
[[ $health_status -ne 0 && "$health_output" == *'未确认 0.5.0 本地健康'* && "$health_output" != *'Hub 容器已就绪'* ]] || exit 1
[[ ! -e "$test_sandbox/wrong-health/secrets/admin-initial-login.txt" ]] || exit 1
passed=$((passed + 1)); printf 'PASS: wrong-version health never claims completion or exports key\n'

# Synthetic success verifies copy/permissions/output, not real Docker/HTTPS.
stub_mode=normal
success_dir="$test_sandbox/success"
success_output=$(bash "$installer" --public-url "$public_url" --install-dir "$success_dir" 2>&1)
assert_safe_output "$success_output"
[[ "$success_output" == *"$public_url/admin/"* && "$success_output" == *"$public_url/"* &&
   "$success_output" == *"cat -- $success_dir/secrets/admin-initial-login.txt"* &&
   "$success_output" == *'公网反代仍需'* && "$success_output" != *'管理员账号'* ]] || exit 1
[[ "$(<"$success_dir/secrets/admin-initial-login.txt")" == "$test_secret" ]] || exit 1
[[ "$(stat -c '%a' "$success_dir/secrets")" == 700 &&
   "$(stat -c '%a' "$success_dir/secrets/admin-initial-login.txt")" == 600 &&
   "$(stat -c '%a' "$success_dir/.env")" == 600 ]] || exit 1
grep -Fxq 'SALCARA_HUB_IMAGE_VERSION=0.5.0' "$success_dir/.env"
grep -Fxq "SALCARA_HUB_PUBLIC_URL=$public_url" "$success_dir/.env"
passed=$((passed + 1)); printf 'PASS: synthetic success protects key and gives URLs without key output\n'

printf 'Installer safety tests: %s passed; no live Docker, SSH, network or production changes.\n' "$passed"
