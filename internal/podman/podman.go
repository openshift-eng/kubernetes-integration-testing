package podman

import (
	"context"
	"encoding/json"
	"errors"
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
	HealthCheckRun(ctx context.Context, name string) error
	ContainerInspect(ctx context.Context, name string) (*ContainerInfo, error)
	ImageInspect(ctx context.Context, image string) (*ImageInfo, error)
	Pull(ctx context.Context, image string) error
	PodCreate(ctx context.Context, opts PodCreateOpts) (string, error)
	PodStop(ctx context.Context, name string, timeout int) error
	PodRemove(ctx context.Context, name string, force bool) error
	SaveOCIArchive(ctx context.Context, image, path string) error
	PodList(ctx context.Context, filters map[string]string) ([]PodInfo, error)
}

type ContainerInfo struct {
	ID          string
	Status      string
	ImageDigest string
	Labels      map[string]string
}

type ImageInfo struct {
	Digest      string
	RepoDigests []string
}

type Mount struct {
	Source  string
	Target  string
	Options string
}

type RunOpts struct {
	Name        string
	Image       string
	Network     string
	Pod         string
	Labels      map[string]string
	Env         map[string]string
	Mounts      []Mount
	Entrypoint  []string
	Args        []string
	Detach      bool
	Replace     bool
	LogDriver   string
	LogOpts     map[string]string
	AuthFile    string
	SecurityOpt []string
	HealthCmd   string
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

func (c *client) output(ctx context.Context, args ...string) ([]byte, error) {
	cmd := c.cmd(ctx, args...)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("podman %s: %s: %w", args[0], stderrFromErr(err), err)
	}
	return out, nil
}

func stderrFromErr(err error) string {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return strings.TrimSpace(string(exitErr.Stderr))
	}
	return ""
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
	if opts.HealthCmd != "" {
		args = append(args, "--health-cmd", opts.HealthCmd)
	}
	if len(opts.Entrypoint) > 0 {
		args = append(args, "--entrypoint", opts.Entrypoint[0])
	}
	args = append(args, opts.Image)
	if len(opts.Entrypoint) > 1 {
		args = append(args, opts.Entrypoint[1:]...)
	}
	args = append(args, opts.Args...)

	out, err := c.output(ctx, args...)
	if err != nil {
		return "", err
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
	out, err := c.cmd(ctx, "wait", name).CombinedOutput()
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

func (c *client) HealthCheckRun(ctx context.Context, name string) error {
	return c.run(ctx, "healthcheck", "run", name)
}

func (c *client) ContainerInspect(ctx context.Context, name string) (*ContainerInfo, error) {
	out, err := c.output(ctx, "container", "inspect", name)
	if err != nil {
		if isNotFoundErr(stderrFromErr(err)) {
			return nil, nil
		}
		return nil, err
	}

	var results []struct {
		ID          string `json:"Id"`
		ImageDigest string `json:"ImageDigest"`
		State       struct {
			Status string `json:"Status"`
		} `json:"State"`
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
	}
	if err := json.Unmarshal(out, &results); err != nil {
		return nil, fmt.Errorf("parsing container inspect: %w", err)
	}
	if len(results) == 0 {
		return nil, nil
	}

	r := results[0]
	return &ContainerInfo{
		ID:          r.ID,
		Status:      r.State.Status,
		ImageDigest: r.ImageDigest,
		Labels:      r.Config.Labels,
	}, nil
}

func (c *client) ImageInspect(ctx context.Context, image string) (*ImageInfo, error) {
	out, err := c.output(ctx, "image", "inspect", image)
	if err != nil {
		if isNotFoundErr(stderrFromErr(err)) {
			return nil, nil
		}
		return nil, err
	}

	var results []struct {
		Digest      string   `json:"Digest"`
		RepoDigests []string `json:"RepoDigests"`
	}
	if err := json.Unmarshal(out, &results); err != nil {
		return nil, fmt.Errorf("parsing image inspect: %w", err)
	}
	if len(results) == 0 {
		return nil, nil
	}

	r := results[0]
	return &ImageInfo{
		Digest:      r.Digest,
		RepoDigests: r.RepoDigests,
	}, nil
}

func (c *client) Pull(ctx context.Context, image string) error {
	return c.run(ctx, "pull", image)
}

func isNotFoundErr(output string) bool {
	lower := strings.ToLower(output)
	return strings.Contains(lower, "no such") ||
		strings.Contains(lower, "not found") ||
		strings.Contains(lower, "not known")
}

func (c *client) SaveOCIArchive(ctx context.Context, image, path string) error {
	return c.run(ctx, "save", "--quiet", "--format", "oci-archive", "-o", path, image)
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

	out, err := c.output(ctx, args...)
	if err != nil {
		return "", err
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

	out, err := c.output(ctx, args...)
	if err != nil {
		return nil, err
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
