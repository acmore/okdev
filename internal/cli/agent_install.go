package cli

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	agentcatalog "github.com/acmore/okdev/internal/agent"
	"github.com/acmore/okdev/internal/config"
)

func ensureConfiguredAgentsInstalled(ctx context.Context, client agentExecClient, namespace, pod, container string, agents []config.AgentSpec, warnf func(string, ...any), progress func(string)) []string {
	results := make([]string, 0, len(agents))
	npmPrepared := false
	var npmPrepareErr error
	var npmPrepareResult string
	report := func(msg string) {
		if progress != nil {
			progress(msg)
		}
	}
	for _, agent := range agents {
		spec, ok := agentcatalog.Lookup(agent.Name)
		if !ok {
			name := strings.TrimSpace(agent.Name)
			if name == "" {
				name = "<empty>"
			}
			warnf("unknown configured agent %q", name)
			results = append(results, name+": unsupported")
			continue
		}
		report(fmt.Sprintf("checking %s", spec.Name))
		installed, err := agentBinaryInstalled(ctx, client, namespace, pod, container, spec.Binary)
		if err != nil {
			warnf("failed to check %s install status: %v", spec.Name, err)
			results = append(results, spec.Name+": check failed")
			continue
		}
		if installed {
			results = append(results, spec.Name+": present")
			continue
		}
		if agentInstallNeedsNPM(spec) {
			reportPrepare := false
			if !npmPrepared {
				report(fmt.Sprintf("preparing npm for %s", spec.Name))
				npmPrepareResult, npmPrepareErr = ensureAgentNPMInstalled(ctx, client, namespace, pod, container)
				npmPrepared = true
				reportPrepare = true
			}
			if npmPrepareErr != nil {
				warnf("skipping %s install: %v", spec.Name, npmPrepareErr)
				results = append(results, spec.Name+": skipped (npm missing)")
				continue
			}
			if reportPrepare && strings.TrimSpace(npmPrepareResult) != "" {
				results = append(results, spec.Name+": "+npmPrepareResult)
			}
		}
		report(fmt.Sprintf("installing %s", spec.Name))
		if _, err := client.ExecShInContainer(ctx, namespace, pod, container, spec.InstallCommand); err != nil {
			warnf("failed to install %s: %v", spec.Name, err)
			results = append(results, spec.Name+": install failed")
			continue
		}
		if err := linkAgentBinary(ctx, client, namespace, pod, container, spec); err != nil {
			warnf("failed to expose %s binary in PATH: %v", spec.Name, err)
		}
		report(fmt.Sprintf("verifying %s", spec.Name))
		installed, err = agentBinaryInstalled(ctx, client, namespace, pod, container, spec.Binary)
		if err != nil || !installed {
			if err != nil {
				warnf("failed to verify %s after install: %v", spec.Name, err)
			} else {
				warnf("%s install completed but binary %q is still unavailable", spec.Name, spec.Binary)
			}
			results = append(results, spec.Name+": verification failed")
			continue
		}
		results = append(results, spec.Name+": installed")
	}
	return results
}

func agentInstallNeedsNPM(spec agentcatalog.Spec) bool {
	return strings.HasPrefix(strings.TrimSpace(spec.InstallCommand), "npm ")
}

func linkAgentBinary(ctx context.Context, client agentExecClient, namespace, pod, container string, spec agentcatalog.Spec) error {
	if !agentInstallNeedsNPM(spec) || strings.TrimSpace(spec.Binary) == "" {
		return nil
	}
	script := fmt.Sprintf(`set -eu
binary=%s
node_path="$(readlink -f "$(command -v node)")"
node_bin_dir="$(dirname "$node_path")"
if [ -x "$node_bin_dir/$binary" ]; then
  ln -sfn "$node_bin_dir/$binary" /usr/local/bin/"$binary"
fi
`, shellQuote(spec.Binary))
	_, err := client.ExecShInContainer(ctx, namespace, pod, container, script)
	return err
}

const agentNPMDetectScript = `set -eu
node_major() {
  if ! command -v node >/dev/null 2>&1; then
    echo 0
    return 0
  fi
  node -p 'process.versions.node.split(".")[0]' 2>/dev/null || echo 0
}
npm_ok() {
  command -v npm >/dev/null 2>&1 && npm --version >/dev/null 2>&1
}
if npm_ok && [ "$(node_major)" -ge 16 ]; then
  echo installed:none
  exit 0
fi
if command -v bash >/dev/null 2>&1 && command -v curl >/dev/null 2>&1; then
  echo install:nvm
  exit 0
fi
if [ "$(id -u)" != "0" ]; then
  echo no-root:none
  exit 0
fi
if ! command -v bash >/dev/null 2>&1; then
  echo unavailable:none
  exit 0
fi
installer="none"
if command -v apk >/dev/null 2>&1; then
  installer="apk"
elif command -v apt-get >/dev/null 2>&1; then
  installer="apt-get"
elif command -v apt >/dev/null 2>&1; then
  installer="apt"
elif command -v dnf >/dev/null 2>&1; then
  installer="dnf"
elif command -v microdnf >/dev/null 2>&1; then
  installer="microdnf"
elif command -v yum >/dev/null 2>&1; then
  installer="yum"
fi
if [ "$installer" != "none" ]; then
  echo install:${installer}
else
  echo unavailable:none
fi
`

const agentNPMInstallScript = `set -eu
installer="${OKDEV_NPM_INSTALLER:-none}"
node_major() {
  if ! command -v node >/dev/null 2>&1; then
    echo 0
    return 0
  fi
  node -p 'process.versions.node.split(".")[0]' 2>/dev/null || echo 0
}
npm_ok() {
  command -v npm >/dev/null 2>&1 && npm --version >/dev/null 2>&1
}
nvm_install() {
  if ! command -v bash >/dev/null 2>&1 || ! command -v curl >/dev/null 2>&1; then
    return 1
  fi
  export NVM_DIR="${NVM_DIR:-$HOME/.nvm}"
  mkdir -p "$NVM_DIR"
  curl -fsSL https://raw.githubusercontent.com/nvm-sh/nvm/v0.40.3/install.sh -o /tmp/okdev-nvm-install.sh >/dev/null 2>&1 || return 1
  PROFILE=/dev/null bash /tmp/okdev-nvm-install.sh >/dev/null 2>&1 || return 1
  cat >/tmp/okdev-nvm-bootstrap.sh <<'OKDEV_NVM_BOOTSTRAP'
set -e
export NVM_DIR="${NVM_DIR:-$HOME/.nvm}"
. "$NVM_DIR/nvm.sh"
nvm install 20 >/dev/null 2>&1
nvm alias default 20 >/dev/null 2>&1 || true
nvm use 20 >/dev/null 2>&1
mkdir -p /usr/local/bin
ln -sfn "$NVM_BIN/node" /usr/local/bin/node
ln -sfn "$NVM_BIN/npm" /usr/local/bin/npm
if [ -x "$NVM_BIN/npx" ]; then
  ln -sfn "$NVM_BIN/npx" /usr/local/bin/npx
fi
OKDEV_NVM_BOOTSTRAP
  NVM_DIR="$NVM_DIR" bash /tmp/okdev-nvm-bootstrap.sh >/dev/null 2>&1 || return 1
  return 0
}
install_curl() {
  case "$installer" in
    apk)
      apk add --no-cache bash curl >/dev/null 2>&1 || return 1
      ;;
    apt-get)
      export DEBIAN_FRONTEND=noninteractive
      apt-get -o DPkg::Lock::Timeout=10 update >/dev/null 2>&1 || return 1
      apt-get -o DPkg::Lock::Timeout=10 install -y --no-install-recommends bash curl ca-certificates >/dev/null 2>&1 || return 1
      ;;
    apt)
      export DEBIAN_FRONTEND=noninteractive
      apt update >/dev/null 2>&1 || return 1
      apt install -y --no-install-recommends bash curl ca-certificates >/dev/null 2>&1 || return 1
      ;;
    dnf)
      dnf install -y bash curl ca-certificates >/dev/null 2>&1 || return 1
      ;;
    microdnf)
      microdnf install -y bash curl ca-certificates >/dev/null 2>&1 || return 1
      ;;
    yum)
      yum install -y bash curl ca-certificates >/dev/null 2>&1 || return 1
      ;;
    *)
      return 1
      ;;
  esac
  return 0
}
if npm_ok && [ "$(node_major)" -ge 16 ]; then
  echo "__OKDEV_NPM_STATUS__=installed:${installer}"
  exit 0
fi
if [ "$(id -u)" != "0" ]; then
  echo "__OKDEV_NPM_STATUS__=no-root:none"
  exit 0
fi
if ! command -v curl >/dev/null 2>&1; then
  install_curl || true
fi
if [ "$installer" = "nvm" ] || [ "$installer" = "apk" ] || [ "$installer" = "apt-get" ] || [ "$installer" = "apt" ] || [ "$installer" = "dnf" ] || [ "$installer" = "microdnf" ] || [ "$installer" = "yum" ]; then
  nvm_install || true
fi
if npm_ok && [ "$(node_major)" -ge 16 ]; then
  echo "__OKDEV_NPM_STATUS__=installed:${installer}"
  exit 0
fi
echo "__OKDEV_NPM_STATUS__=unavailable:${installer}"
`

func ensureAgentNPMInstalled(ctx context.Context, client agentExecClient, namespace, pod, container string) (string, error) {
	npmInstalled, err := agentNPMUsable(ctx, client, namespace, pod, container)
	if err != nil {
		return "", fmt.Errorf("check npm availability: %w", err)
	}
	nodeMajor, err := agentNodeMajorVersion(ctx, client, namespace, pod, container)
	if err != nil {
		return "", fmt.Errorf("check node version: %w", err)
	}
	if npmInstalled && nodeMajor >= 16 {
		return "", nil
	}
	status, installer, err := detectAgentNPM(ctx, client, namespace, pod, container)
	if err != nil {
		return "", err
	}
	switch status {
	case "no-root":
		return "", fmt.Errorf("npm is unavailable and the dev container is not running as root")
	case "unavailable":
		return "", fmt.Errorf("npm is unavailable and neither nvm prerequisites nor a supported package manager were found")
	case "install":
	default:
		return "", fmt.Errorf("unexpected npm prepare result %q", status)
	}
	out, err := client.ExecShInContainer(ctx, namespace, pod, container, "export OKDEV_NPM_INSTALLER="+shellQuote(installer)+"; "+agentNPMInstallScript)
	if err != nil {
		return "", err
	}
	status, installer = parseAgentNPMStatus(string(out))
	switch status {
	case "installed":
		if installer == "nvm" {
			return "node/npm installed via nvm", nil
		}
		if installer != "" && installer != "none" {
			return "npm installed via " + installer, nil
		}
		return "npm installed", nil
	case "no-root":
		return "", fmt.Errorf("npm is unavailable and the dev container is not running as root")
	case "unavailable":
		if installer == "nvm" {
			return "", fmt.Errorf("npm is unavailable after best-effort install via nvm")
		}
		if installer != "" && installer != "none" {
			return "", fmt.Errorf("npm is unavailable after best-effort install via %s", installer)
		}
		return "", fmt.Errorf("npm is unavailable after best-effort install")
	default:
		return "", fmt.Errorf("unexpected npm install result %q", strings.TrimSpace(string(out)))
	}
}

func agentNPMUsable(ctx context.Context, client agentExecClient, namespace, pod, container string) (bool, error) {
	_, err := client.ExecShInContainer(ctx, namespace, pod, container, "command -v npm >/dev/null 2>&1 && npm --version >/dev/null 2>&1")
	if err != nil {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "exit code 127") || strings.Contains(msg, "exit status 127") || strings.Contains(msg, "not found") {
			return false, nil
		}
		if strings.Contains(msg, "exit code 1") || strings.Contains(msg, "exit status 1") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func detectAgentNPM(ctx context.Context, client agentExecClient, namespace, pod, container string) (status, installer string, err error) {
	out, err := client.ExecShInContainer(ctx, namespace, pod, container, agentNPMDetectScript)
	if err != nil {
		return "", "", err
	}
	status, installer, _ = strings.Cut(strings.TrimSpace(string(out)), ":")
	return strings.TrimSpace(status), strings.TrimSpace(installer), nil
}

func parseAgentNPMStatus(raw string) (status, installer string) {
	for _, line := range strings.Split(raw, "\n") {
		text := strings.TrimSpace(line)
		if !strings.HasPrefix(text, "__OKDEV_NPM_STATUS__=") {
			continue
		}
		payload := strings.TrimPrefix(text, "__OKDEV_NPM_STATUS__=")
		status, installer, _ = strings.Cut(payload, ":")
		return strings.TrimSpace(status), strings.TrimSpace(installer)
	}
	return "", ""
}

func agentNodeMajorVersion(ctx context.Context, client agentExecClient, namespace, pod, container string) (int, error) {
	out, err := client.ExecShInContainer(ctx, namespace, pod, container, `node -p 'process.versions.node.split(".")[0]'`)
	if err != nil {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "exit code 127") || strings.Contains(msg, "exit status 127") || strings.Contains(msg, "not found") {
			return 0, nil
		}
		return 0, err
	}
	value := strings.TrimSpace(string(out))
	if value == "" {
		return 0, nil
	}
	major, convErr := strconv.Atoi(value)
	if convErr != nil {
		return 0, fmt.Errorf("parse node major version %q: %w", value, convErr)
	}
	return major, nil
}
