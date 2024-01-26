// Package viamserver contains the viam-server agent subsystem.
package viamserver

import (
	"context"
	"errors"
	"net/http"
	"os/exec"
	"path"
	"regexp"
	"sync"
	"syscall"
	"time"

	errw "github.com/pkg/errors"
	"github.com/viamrobotics/agent"
	"github.com/viamrobotics/agent/subsystems"
	"github.com/viamrobotics/agent/subsystems/registry"
	"go.uber.org/zap"
	pb "go.viam.com/api/app/agent/v1"
)

func init() {
	registry.Register(SubsysName, NewSubsystem, DefaultConfig)
}

const (
	startTimeout = time.Minute * 5
	stopTimeout  = time.Minute * 2
	SubsysName   = "viam-server"
)

var (
	ConfigFilePath = "/etc/viam.json"
	DefaultConfig = &pb.DeviceSubsystemConfig{}
)

type viamServer struct {
	mu        sync.Mutex
	cmd       *exec.Cmd
	running   bool
	shouldRun bool
	lastExit  int
	checkURL  string

	// for blocking start/stop/check ops while another is in progress
	startStopMu sync.Mutex

	logger *zap.SugaredLogger
}

func (s *viamServer) Start(ctx context.Context) error {
	s.startStopMu.Lock()
	defer s.startStopMu.Unlock()

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running {
		return nil
	}
	if s.shouldRun {
		s.logger.Warnf("Restarting %s after unexpected exit", SubsysName)
	} else {
		s.logger.Infof("Starting %s", SubsysName)
		s.shouldRun = true
	}

	stdio := agent.NewMatchingLogger(s.logger, false)
	stderr := agent.NewMatchingLogger(s.logger, true)

	s.cmd = exec.Command(path.Join(agent.ViamDirs["bin"], SubsysName), "-config", ConfigFilePath)
	s.cmd.Dir = agent.ViamDirs["viam"]
	s.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	s.cmd.Stdout = stdio
	s.cmd.Stderr = stderr

	// watch for this line in the logs to indicate successful startup
	c, err := stdio.AddMatcher("checkURL", regexp.MustCompile(`serving\W*{"url":\W*"(https?://[\w\.:-]+)".*}`), false)
	if err != nil {
		return err
	}
	defer stdio.DeleteMatcher("checkURL")

	err = s.cmd.Start()
	if err != nil {
		return errw.Wrapf(err, "error starting %s", SubsysName)
	}
	s.running = true

	go func() {
		err := s.cmd.Wait()
		s.mu.Lock()
		defer s.mu.Unlock()
		s.running = false
		s.logger.Infof("%s exited", SubsysName)
		if err != nil {
			s.logger.Errorw("error while getting process status", "error", err)
		}
		if s.cmd.ProcessState != nil {
			s.lastExit = s.cmd.ProcessState.ExitCode()
			if s.lastExit != 0 {
				s.logger.Errorw("non-zero exit code", "exit code", s.lastExit)
			}
		}
	}()

	select {
	case matches := <-c:
		s.checkURL = matches[1]
		s.logger.Infof("healthcheck URL: %s", s.checkURL)
		s.logger.Infof("%s started", SubsysName)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(startTimeout):
		return errw.New("startup timed out")
	}
}

func (s *viamServer) Stop(ctx context.Context) error {
	s.startStopMu.Lock()
	defer s.startStopMu.Unlock()

	s.mu.Lock()
	running := s.running
	s.shouldRun = false
	s.mu.Unlock()

	if !running {
		return nil
	}

	// interrupt early in startup
	if s.cmd == nil {
		return nil
	}

	s.logger.Infof("Stopping %s", SubsysName)

	err := s.cmd.Process.Signal(syscall.SIGTERM)
	if err != nil {
		s.logger.Error(err)
	}

	if s.waitForExit(ctx, stopTimeout/2) {
		s.logger.Infof("%s successfully stopped", SubsysName)
		return nil
	}

	s.logger.Warnf("%s refused to exit, killing", SubsysName)
	err = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
	if err != nil {
		s.logger.Error(err)
	}

	if s.waitForExit(ctx, stopTimeout/2) {
		s.logger.Infof("%s successfully killed", SubsysName)
		return nil
	}

	return errw.Errorf("%s process couldn't be killed", SubsysName)
}

func (s *viamServer) waitForExit(ctx context.Context, timeout time.Duration) bool {
	ctxTimeout, cancelFunc := context.WithTimeout(ctx, timeout)
	defer cancelFunc()

	// loop so that even after the context expires, we still have one more second before a final check.
	var lastTry bool
	for {
		s.mu.Lock()
		running := s.running
		s.mu.Unlock()
		if !running || lastTry {
			return !running
		}
		if ctxTimeout.Err() != nil {
			lastTry = true
		}
		time.Sleep(time.Second)
	}
}

func (s *viamServer) HealthCheck(ctx context.Context) (errRet error) {
	s.startStopMu.Lock()
	defer s.startStopMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running {
		return errw.Errorf("%s not running", SubsysName)
	}
	if s.checkURL == "" {
		return errw.Errorf("can't find listening URL for %s", SubsysName)
	}

	s.logger.Debugf("starting healthcheck for %s using %s", SubsysName, s.checkURL)

	timeoutCtx, cancelFunc := context.WithTimeout(ctx, time.Second*30)
	defer cancelFunc()

	req, err := http.NewRequestWithContext(timeoutCtx, http.MethodGet, s.checkURL, nil)
	if err != nil {
		return errw.Wrapf(err, "checking %s status", SubsysName)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return errw.Wrapf(err, "checking %s status", SubsysName)
	}

	defer func() {
		errRet = errors.Join(errRet, resp.Body.Close())
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errw.Wrapf(err, "checking %s status, got code: %d", SubsysName, resp.StatusCode)
	}
	s.logger.Debugf("healthcheck for %s is good", SubsysName)
	return nil
}

func NewSubsystem(ctx context.Context, logger *zap.SugaredLogger, updateConf *pb.DeviceSubsystemConfig) (subsystems.Subsystem, error) {
	return agent.NewAgentSubsystem(ctx, SubsysName, logger, &viamServer{logger: logger.Named(SubsysName)})
}
