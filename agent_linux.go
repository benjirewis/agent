package agent

import (
	"context"
	_ "embed"
	"os/exec"
	"path/filepath"

	errw "github.com/pkg/errors"
	"github.com/viamrobotics/agent/utils"
	"go.viam.com/rdk/logging"
	rutils "go.viam.com/rdk/utils"
)

const (
	serviceName = "viam-agent"

	// detachedServiceName is a fallback service that runs viam-server directly when the
	// installed viam-agent binary is "bad". It is started via OnFailure= from viam-agent.service.
	detachedServiceName = "viam-server-detached"

	// detachedRetryScriptName is the ExecStartPre= helper for viam-server-detached.service
	// that schedules delayed attempts to leave detached mode. It must match the
	// path referenced in viam-server-detached.service.
	detachedRetryScriptName = "schedule-agent-retry.sh"
)

//go:embed viam-agent.service
var serviceFileContents []byte

//go:embed viam-server-detached.service
var detachedServiceFileContents []byte

//go:embed schedule-agent-retry.sh
var detachedRetryScriptContents []byte

// InstallNewVersion runs the newly downloaded binary's Install() for installation of systemd files and the like.
func InstallNewVersion(ctx context.Context, logger logging.Logger) (bool, error) {
	expectedPath := filepath.Join(utils.ViamDirs.Bin, SubsystemName)

	// Run the newly updated version to install systemd and other service files.
	logger.Info("running viam-agent --install for new version")

	cleanup := rutils.SlowLogger(
		ctx,
		"Waiting for new version of viam-agent to finish installing", "", "",
		logger,
	)
	defer cleanup()

	//nolint:gosec
	cmd := exec.CommandContext(ctx, expectedPath, "--install")
	output, err := cmd.CombinedOutput()
	logger.Info(string(output))
	if err != nil {
		return false, errw.Wrapf(err, "error running install step %s", output)
	}
	return true, nil
}
