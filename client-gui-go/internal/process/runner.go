package process

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

type RunnerMode int

const (
	ModeExternal RunnerMode = iota // 外部进程模式
	ModeEmbedded                   // 内嵌模式
)

type Config struct {
	Server      string
	Listen      string
	Token       string
	IP          string
	DNS         string
	ECH         string
	RoutingMode string
	Mode        RunnerMode // 运行模式
}

type Runner struct {
	mu     sync.Mutex
	cmd    *exec.Cmd
	cancel context.CancelFunc
	mode   RunnerMode

	// 内嵌模式
	embedded *EmbeddedRunner
}

func NewRunner() *Runner {
	return &Runner{
		embedded: NewEmbeddedRunner(nil),
	}
}

func (r *Runner) IsRunning() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	switch r.mode {
	case ModeEmbedded:
		return r.embedded.IsRunning()
	default:
		return r.cmd != nil
	}
}

func FindEchWorkersExe() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(exe)
	p := filepath.Join(dir, workersBinaryName())
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("workers binary not found next to client executable: %s", p)
	}
	return p, nil
}

func BuildArgs(c Config) []string {
	args := []string{}
	if c.Server != "" {
		args = append(args, "-f", c.Server)
	}
	if c.Listen != "" {
		args = append(args, "-l", c.Listen)
	}
	if c.Token != "" {
		args = append(args, "-token", c.Token)
	}
	if c.IP != "" {
		args = append(args, "-ip", c.IP)
	}
	if c.DNS != "" && c.DNS != "dns.alidns.com/dns-query" {
		args = append(args, "-dns", c.DNS)
	}
	if c.ECH != "" && c.ECH != "cloudflare-ech.com" {
		args = append(args, "-ech", c.ECH)
	}
	if c.RoutingMode != "" {
		args = append(args, "-routing", c.RoutingMode)
	}
	return args
}

func (r *Runner) Start(c Config, onLog func(string)) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// 设置模式
	r.mode = c.Mode

	switch r.mode {
	case ModeEmbedded:
		// 内嵌模式
		if r.embedded.IsRunning() {
			return errors.New("embedded server is already running")
		}
		return r.embedded.Start(c, onLog)
	default:
		// 外部进程模式（原有逻辑）
		if r.cmd != nil {
			return errors.New("process already running")
		}
		exe, err := FindEchWorkersExe()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithCancel(context.Background())
		r.cancel = cancel
		cmd := exec.CommandContext(ctx, exe, BuildArgs(c)...)
		cmd.Dir = filepath.Dir(exe)
		applyPlatformCmdTweaks(cmd)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			cancel()
			return err
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			cancel()
			return err
		}
		if err := cmd.Start(); err != nil {
			cancel()
			return err
		}
		r.cmd = cmd

		go streamLines(stdout, onLog)
		go streamLines(stderr, onLog)

		go func() {
			_ = cmd.Wait()
			r.mu.Lock()
			r.cmd = nil
			if r.cancel != nil {
				r.cancel()
				r.cancel = nil
			}
			r.mu.Unlock()
			onLog("[系统] 进程已停止。\n")
		}()

		return nil
	}
}

func streamLines(rc io.ReadCloser, onLog func(string)) {
	scanner := bufio.NewScanner(rc)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)
	for scanner.Scan() {
		onLog(scanner.Text() + "\n")
	}
}

func (r *Runner) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()

	switch r.mode {
	case ModeEmbedded:
		// 内嵌模式
		r.embedded.Stop()
	default:
		// 外部进程模式
		if r.cancel != nil {
			r.cancel()
		}
		if r.cmd != nil && r.cmd.Process != nil {
			_ = r.cmd.Process.Kill()
		}
	}
}
