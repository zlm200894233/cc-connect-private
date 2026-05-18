package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"

	gopty "github.com/aymanbagabas/go-pty"
	"github.com/creack/pty"
)

type realTerminalPTY struct{}

type realTerminalPTYProcess struct {
	cmd *exec.Cmd
	pty *os.File
}

type windowsTerminalPTYProcess struct {
	cmd *gopty.Cmd
	pty gopty.Pty
}

func (p *realTerminalPTYProcess) Read(b []byte) (int, error) {
	return p.pty.Read(b)
}

func (p *realTerminalPTYProcess) Write(data []byte) (int, error) {
	return p.pty.Write(data)
}

func (p *realTerminalPTYProcess) Resize(width, height int) error {
	return pty.Setsize(p.pty, &pty.Winsize{Rows: uint16(height), Cols: uint16(width)})
}

func (p *realTerminalPTYProcess) Close() error {
	if p.pty != nil {
		return p.pty.Close()
	}
	return nil
}

func (p *realTerminalPTYProcess) Wait() error {
	return p.cmd.Wait()
}

func (p *windowsTerminalPTYProcess) Read(b []byte) (int, error) {
	return p.pty.Read(b)
}

func (p *windowsTerminalPTYProcess) Write(data []byte) (int, error) {
	return p.pty.Write(data)
}

func (p *windowsTerminalPTYProcess) Resize(width, height int) error {
	return p.pty.Resize(width, height)
}

func (p *windowsTerminalPTYProcess) Close() error {
	if p.pty != nil {
		return p.pty.Close()
	}
	return nil
}

func (p *windowsTerminalPTYProcess) Wait() error {
	return p.cmd.Wait()
}

func (realTerminalPTY) Start(ctx context.Context, opts terminalClaudeOptions) (terminalProcess, error) {
	args := []string{}
	if opts.resumeID != "" {
		args = append(args, "--resume", opts.resumeID)
	}
	if runtime.GOOS == "windows" {
		return startWindowsTerminalPTY(ctx, opts, args)
	}
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = opts.workDir

	f, err := pty.Start(cmd)
	if err != nil {
		if errors.Is(err, pty.ErrUnsupported) {
			return nil, fmt.Errorf("terminal PTY is not supported on this platform: %w", err)
		}
		return nil, fmt.Errorf("start PTY: %w", err)
	}
	return &realTerminalPTYProcess{cmd: cmd, pty: f}, nil
}

func startWindowsTerminalPTY(ctx context.Context, opts terminalClaudeOptions, args []string) (terminalProcess, error) {
	pt, err := gopty.New()
	if err != nil {
		return nil, fmt.Errorf("create windows PTY: %w", err)
	}
	cmdPath, err := exec.LookPath("claude")
	if err != nil {
		_ = pt.Close()
		return nil, fmt.Errorf("find claude: %w", err)
	}
	cmd := pt.CommandContext(ctx, cmdPath, args...)
	cmd.Dir = opts.workDir
	if err := cmd.Start(); err != nil {
		_ = pt.Close()
		return nil, fmt.Errorf("start PTY: %w", err)
	}
	return &windowsTerminalPTYProcess{cmd: cmd, pty: pt}, nil
}
