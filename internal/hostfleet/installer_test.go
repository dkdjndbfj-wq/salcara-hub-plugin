package hostfleet

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// This executes the actual installer in an isolated disposable Linux container.
// systemd/curl are fixtures: it verifies file ownership and rollback, not a VPS.
func TestInstallerRecoveryInLinuxContainer(t *testing.T) {
	if os.Getenv("SALCARA_TEST_INSTALLER_DOCKER") != "1" {
		t.Skip("opt-in isolated Docker installer validation")
	}
	dir := t.TempDir()
	files := map[string]string{"install.sh": installScript, "service": serviceUnit, "run.sh": installerHarness}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	command := exec.Command("docker", "run", "--rm", "--network", "none", "--mount", "type=bind,source="+dir+",target=/qa,readonly", "--entrypoint", "sh", "golang:1.27.1-bookworm", "/qa/run.sh")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("installer recovery: %v\n%s", err, output)
	} else {
		t.Log(string(output))
	}
}

const installerHarness = `set -eu
mkdir -p /tmp/bin /run/lock
export PATH=/tmp/bin:$PATH
cat > /tmp/bin/ss <<'CMD'
#!/bin/sh
exit 0
CMD
cat > /tmp/bin/sleep <<'CMD'
#!/bin/sh
exit 0
CMD
cat > /tmp/bin/systemctl <<'CMD'
#!/bin/sh
set -eu
if [ "$1" = start ]; then
 if grep -q NEW /opt/salcara-edge-managed/salcara-edge; then
  printf '%s' '{"future_schema":2}' > /var/lib/salcara-edge-managed/edge-pool.json
 elif [ ! -f /var/lib/salcara-edge-managed/edge-pool.json ]; then
  printf '%s' '{"revision":1,"mode":"manual"}' > /var/lib/salcara-edge-managed/edge-pool.json
 fi
 chown salcara-edge-managed:salcara-edge-managed /var/lib/salcara-edge-managed/edge-pool.json
 chmod 0600 /var/lib/salcara-edge-managed/edge-pool.json
fi
exit 0
CMD
cat > /tmp/bin/curl <<'CMD'
#!/bin/sh
set -eu
case "$(cat /tmp/health)" in
 fail) exit 1;;
 fail-new) if grep -q NEW /opt/salcara-edge-managed/salcara-edge; then exit 1; fi;;
esac
printf '%s' '{"status":"ready"}'
CMD
chmod 0755 /tmp/bin/*
bundle() {
 dir="$1"; version="$2"
 mkdir -p "$dir"
 cp /qa/install.sh "$dir/install.sh"
 cp /qa/service "$dir/service"
 printf '%s\n' "$version" > "$dir/salcara-edge"
 (cd "$dir"; sha256sum salcara-edge > binary.sha256)
 printf '%s' salcara-hostfleet-v1:synthetic-owner > "$dir/owner"
 printf '%s' synthetic-private-token > "$dir/control.token"
 printf '%s' synthetic-env > "$dir/environment"
 printf '%s' synthetic-curl-config > "$dir/curl-control"
 uname -m > "$dir/machine"
}
bundle /tmp/first OLD
printf '%s' fail > /tmp/health
if sh /tmp/first/install.sh /tmp/first; then echo 'unexpected failed health success'; exit 1; fi
test -f /opt/salcara-edge-managed/.owner
test ! -e /etc/systemd/system/salcara-edge-managed.service
printf '%s' okay > /tmp/health
sh /tmp/first/install.sh /tmp/first
test -f /etc/systemd/system/salcara-edge-managed.service
test "$(stat -c %a /opt/salcara-edge-managed/control.token)" = 400
test "$(stat -c %U /opt/salcara-edge-managed/control.token)" = salcara-edge-managed
cp /var/lib/salcara-edge-managed/edge-pool.json /tmp/expected-pool
bundle /tmp/upgrade NEW
printf '%s' fail-new > /tmp/health
if sh /tmp/upgrade/install.sh /tmp/upgrade; then echo 'unexpected failed upgrade success'; exit 1; fi
grep -q OLD /opt/salcara-edge-managed/salcara-edge
cmp /tmp/expected-pool /var/lib/salcara-edge-managed/edge-pool.json
test "$(stat -c %a /var/lib/salcara-edge-managed/edge-pool.json)" = 600
test "$(stat -c %U /var/lib/salcara-edge-managed/edge-pool.json)" = salcara-edge-managed
bundle /tmp/alien NEW
printf '%s' salcara-hostfleet-v1:other-owner > /tmp/alien/owner
if sh /tmp/alien/install.sh /tmp/alien; then echo 'unexpected ownership takeover'; exit 1; fi
grep -q OLD /opt/salcara-edge-managed/salcara-edge
printf '%s\n' 'PASS: first-install retry, private ownership, old-schema rollback, cross-Hub takeover rejected'
`
