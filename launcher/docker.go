package launcher

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"text/template"
	"time"

	clabernetesconstants "github.com/srl-labs/clabernetes/constants"
	claberneteserrors "github.com/srl-labs/clabernetes/errors"
	claberneteslogging "github.com/srl-labs/clabernetes/logging"
)

const (
	dockerDaemonConfig   = "/etc/docker/daemon.json"
	vfsStorageDriver     = "vfs"
	overlayStorageDriver = "overlay2"
)

func daemonConfigExists() bool {
	_, err := os.Stat(dockerDaemonConfig)

	return err == nil
}

func handleInsecureRegistries() error {
	insecureRegistries := os.Getenv(clabernetesconstants.LauncherInsecureRegistries)

	if insecureRegistries == "" {
		return nil
	}

	splitRegistries := strings.Split(insecureRegistries, ",")

	quotedRegistries := make([]string, len(splitRegistries))

	for idx, elem := range splitRegistries {
		quotedRegistries[idx] = fmt.Sprintf("%q", elem)
	}

	templateVars := struct {
		StorageDriver      string
		InsecureRegistries string
	}{
		StorageDriver:      vfsStorageDriver,
		InsecureRegistries: strings.Join(quotedRegistries, ","),
	}

	// if the pod is privileged we can run w/ overlayfs instead of vfs which should
	// be much more efficient size-wise if not also perofrmance-wise; this *does* assume
	// the hosts kernel supports overlayfs but that *should* be true almost everywhere at
	// this point in time... i hope :P
	if !strings.EqualFold(
		os.Getenv(clabernetesconstants.LauncherPrivilegedEnv),
		clabernetesconstants.True,
	) {
		templateVars.StorageDriver = overlayStorageDriver
	}

	t, err := template.ParseFS(Assets, "assets/docker-daemon.json.template")
	if err != nil {
		return err
	}

	var rendered bytes.Buffer

	err = t.Execute(&rendered, templateVars)
	if err != nil {
		return err
	}

	err = os.WriteFile(
		dockerDaemonConfig,
		rendered.Bytes(),
		clabernetesconstants.PermissionsEveryoneReadWriteOwnerExecute,
	)
	if err != nil {
		return err
	}

	return nil
}

func enableLegacyIPTables(ctx context.Context, logger io.Writer) error {
	updateCmd := exec.CommandContext(
		ctx,
		"update-alternatives",
		"--set",
		"iptables",
		"/usr/sbin/iptables-legacy",
	)

	updateCmd.Stdout = logger
	updateCmd.Stderr = logger

	err := updateCmd.Run()
	if err != nil {
		return err
	}

	return nil
}

func startDocker(ctx context.Context, logger io.Writer) error {
	var attempts int

	for {
		psCmd := exec.CommandContext(ctx, "docker", "ps")

		psCmd.Stdout = logger
		psCmd.Stderr = logger

		err := psCmd.Run()
		if err == nil {
			// exit 0, docker seems happy
			return nil
		}

		if attempts > maxDockerLaunchAttempts {
			return fmt.Errorf("%w: failed starting docker", claberneteserrors.ErrLaunch)
		}

		startCmd := exec.CommandContext(ctx, "service", "docker", "start")

		startCmd.Stdout = logger
		startCmd.Stderr = logger

		err = startCmd.Run()
		if err != nil {
			return err
		}

		time.Sleep(time.Second)

		attempts++
	}
}

func getContainerIDs(ctx context.Context, all bool) ([]string, error) {
	args := []string{"ps"}

	if all {
		args = append(args, "-a")
	}

	args = append(args, "--quiet")

	psCmd := exec.CommandContext(ctx, "docker", args...)

	output, err := psCmd.Output()
	if err != nil {
		return nil, err
	}

	containerIDLines := strings.Split(string(output), "\n")

	var containerIDs []string

	for _, line := range containerIDLines {
		trimmedLine := strings.TrimSpace(line)

		if trimmedLine != "" {
			containerIDs = append(containerIDs, trimmedLine)
		}
	}

	return containerIDs, nil
}

func printContainerLogs(
	ctx context.Context,
	logger claberneteslogging.Instance,
	containerIDs []string,
) {
	for _, containerID := range containerIDs {
		args := []string{
			"logs",
			containerID,
		}

		cmd := exec.CommandContext(ctx, "docker", args...) //nolint:gosec

		cmd.Stdout = logger
		cmd.Stderr = logger

		err := cmd.Run()
		if err != nil {
			logger.Warnf(
				"printing node logs for container id %q failed, err: %s", containerID, err,
			)
		}
	}
}

func tailContainerLogs(
	ctx context.Context,
	logger claberneteslogging.Instance,
	nodeLogger io.Writer,
	containerIDs []string,
) error {
	nodeLogFile, err := os.Create("node.log")
	if err != nil {
		return err
	}

	nodeOutWriter := io.MultiWriter(nodeLogger, nodeLogFile)

	for _, containerID := range containerIDs {
		go func(containerID string, nodeOutWriter io.Writer) {
			args := []string{
				"logs",
				"-f",
				containerID,
			}

			cmd := exec.CommandContext(ctx, "docker", args...) //nolint:gosec

			cmd.Stdout = nodeOutWriter
			cmd.Stderr = nodeOutWriter

			err = cmd.Run()
			if err != nil {
				logger.Warnf(
					"tailing node logs for container id %q failed, err: %s", containerID, err,
				)
			}
		}(containerID, nodeOutWriter)
	}

	return nil
}

func getContainerIDForNodeName(ctx context.Context, nodeName string) (string, error) {
	psCmd := exec.CommandContext( //nolint:gosec
		ctx,
		"docker",
		"ps",
		"--quiet",
		"--filter",
		fmt.Sprintf("name=%s", nodeName),
	)

	output, err := psCmd.Output()
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(string(output)), nil
}

func getContainerAddr(ctx context.Context, containerID string) (string, error) {
	inspectCmd := exec.CommandContext(
		ctx,
		"docker",
		"inspect",
		"--format",
		"{{range.NetworkSettings.Networks}}{{.IPAddress}}{{end}}",
		containerID,
	)

	output, err := inspectCmd.Output()
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(string(output)), nil
}
