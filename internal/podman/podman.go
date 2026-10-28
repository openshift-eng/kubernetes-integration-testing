package podman

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

type Client interface {
	Run(ctx context.Context, opts RunOpts) (string, error)
	Stop(ctx context.Context, name string, timeout int) error
	Remove(ctx context.Context, name string, force bool) error
	Wait(ctx context.Context, name string) (int, error)
	Restart(ctx context.Context, name string) error
	PodCreate(ctx context.Context, opts PodCreateOpts) (string, error)
	PodStop(ctx context.Context, name string, timeout int) error
	PodRemove(ctx context.Context, name string, force bool) error
	PodList(ctx context.Context, filters map[string]string) ([]PodInfo, error)
}

type Mount struct {
	Source   string
	Target   string
	Options string
}

type RunOpts struct {
	Name       string
	Image      string
	Network    string
	Pod        string
	Labels     map[string]string
	Env        map[string]string
	Mounts     []Mount
	Entrypoint []string
	Args       []string
	Detach     bool
	Replace    bool
	LogDriver  string
	LogOpts    map[string]string
	AuthFile    string
	SecurityOpt []string
}

type PodCreateOpts struct {
	Name           string
	Network        string
	Labels         map[string]string
	NetworkAliases []string
}

type PodInfo struct {
	ID     string            `json:"Id"`
	Name   string            `json:"Name"`
	Labels map[string]string `json:"Labels"`
}

type client struct {
	binary string
	args   []string
}

func NewLocalClient() Client {
	return &client{binary: "podman"}
}

func NewRemoteClient(socketPath string) Client {
	return &client{
		binary: lookupBinary(),
		args:   []string{"--url", "unix://" + socketPath},
	}
}

func lookupBinary() string {
	if _, err := exec.LookPath("podman"); err == nil {
		return "podman"
	}
	return "podman-remote"
}

func (c *client) cmd(ctx context.Context, args ...string) *exec.Cmd {
	all := make([]string, 0, len(c.args)+len(args))
	all = append(all, c.args...)
	all = append(all, args...)
	return exec.CommandContext(ctx, c.binary, all...)
}

func (c *client) run(ctx context.Context, args ...string) error {
	out, err := c.cmd(ctx, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("podman %s: %s: %w", args[0], strings.TrimSpace(string(out)), err)
	}
	return nil
}

func (c *client) cmdNoCtx(args ...string) *exec.Cmd {
	all := make([]string, 0, len(c.args)+len(args))
	all = append(all, c.args...)
	all = append(all, args...)
	return exec.Command(c.binary, all...)
}

func (c *client) Run(ctx context.Context, opts RunOpts) (string, error) {
	args := []string{"run"}
	if opts.Detach {
		args = append(args, "-d")
	}
	if opts.Pod != "" {
		args = append(args, "--pod", opts.Pod)
	}
	if opts.Name != "" {
		args = append(args, "--name", opts.Name)
	}
	if opts.Network != "" {
		args = append(args, "--network", opts.Network)
	}
	if opts.Replace {
		args = append(args, "--replace")
	}
	if opts.LogDriver != "" {
		args = append(args, "--log-driver", opts.LogDriver)
	}
	for k, v := range opts.LogOpts {
		args = append(args, "--log-opt", k+"="+v)
	}
	for k, v := range opts.Labels {
		args = append(args, "--label", k+"="+v)
	}
	for k, v := range opts.Env {
		args = append(args, "-e", k+"="+v)
	}
	for _, m := range opts.Mounts {
		v := m.Source + ":" + m.Target
		if m.Options != "" {
			v += ":" + m.Options
		}
		args = append(args, "-v", v)
	}
	if opts.AuthFile != "" {
		args = append(args, "--authfile", opts.AuthFile)
	}
	for _, opt := range opts.SecurityOpt {
		args = append(args, "--security-opt", opt)
	}
	if len(opts.Entrypoint) > 0 {
		args = append(args, "--entrypoint", opts.Entrypoint[0])
	}
	args = append(args, opts.Image)
	if len(opts.Entrypoint) > 1 {
		args = append(args, opts.Entrypoint[1:]...)
	}
	args = append(args, opts.Args...)

	out, err := c.cmd(ctx, args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("podman run: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (c *client) Stop(ctx context.Context, name string, timeout int) error {
	args := []string{"stop"}
	if timeout >= 0 {
		args = append(args, "-t", fmt.Sprintf("%d", timeout))
	}
	args = append(args, name)
	return c.run(ctx, args...)
}

func (c *client) Remove(ctx context.Context, name string, force bool) error {
	args := []string{"rm"}
	if force {
		args = append(args, "-f")
	}
	args = append(args, name)
	return c.run(ctx, args...)
}

func (c *client) Wait(ctx context.Context, name string) (int, error) {
	out, err := c.cmdNoCtx("wait", name).CombinedOutput()
	if err != nil {
		return 1, fmt.Errorf("podman wait: %s: %w", strings.TrimSpace(string(out)), err)
	}
	var code int
	_, _ = fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &code)
	return code, nil
}

func (c *client) Restart(ctx context.Context, name string) error {
	return c.run(ctx, "restart", name)
}

func (c *client) PodCreate(ctx context.Context, opts PodCreateOpts) (string, error) {
	args := []string{"pod", "create"}
	if opts.Name != "" {
		args = append(args, "--name", opts.Name)
	}
	if opts.Network != "" {
		args = append(args, "--network", opts.Network)
	}
	for k, v := range opts.Labels {
		args = append(args, "--label", k+"="+v)
	}
	for _, alias := range opts.NetworkAliases {
		args = append(args, "--network-alias", alias)
	}

	out, err := c.cmd(ctx, args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("podman pod create: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (c *client) PodStop(ctx context.Context, name string, timeout int) error {
	args := []string{"pod", "stop"}
	if timeout >= 0 {
		args = append(args, "-t", fmt.Sprintf("%d", timeout))
	}
	args = append(args, name)
	return c.run(ctx, args...)
}

func (c *client) PodRemove(ctx context.Context, name string, force bool) error {
	args := []string{"pod", "rm"}
	if force {
		args = append(args, "-f")
	}
	args = append(args, name)
	return c.run(ctx, args...)
}

func (c *client) PodList(ctx context.Context, filters map[string]string) ([]PodInfo, error) {
	args := []string{"pod", "ps", "--format", "json"}
	for k, v := range filters {
		args = append(args, "--filter", k+"="+v)
	}

	out, err := c.cmd(ctx, args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("podman pod ps: %s: %w", strings.TrimSpace(string(out)), err)
	}
	if len(out) == 0 {
		return nil, nil
	}

	var pods []PodInfo
	if err := json.Unmarshal(out, &pods); err != nil {
		return nil, fmt.Errorf("parsing pod list: %w", err)
	}
	return pods, nil
}
