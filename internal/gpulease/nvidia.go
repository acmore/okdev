package gpulease

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// ErrNoNvidiaSMI means the container has no nvidia-smi to ask.
var ErrNoNvidiaSMI = errors.New("nvidia-smi not found in this container")

// NvidiaSMI probes GPUs through the nvidia-smi on PATH.
type NvidiaSMI struct{}

func (NvidiaSMI) query(ctx context.Context, args ...string) ([]string, error) {
	path, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return nil, ErrNoNvidiaSMI
	}
	out, err := exec.CommandContext(ctx, path, append(args, "--format=csv,noheader")...).Output()
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi %s: %w", strings.Join(args, " "), err)
	}
	var lines []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines, nil
}

func (p NvidiaSMI) GPUs(ctx context.Context) ([]GPU, error) {
	lines, err := p.query(ctx, "--query-gpu=index,uuid")
	if err != nil {
		return nil, err
	}
	gpus := make([]GPU, 0, len(lines))
	for _, line := range lines {
		index, uuid, ok := strings.Cut(line, ",")
		n, convErr := strconv.Atoi(strings.TrimSpace(index))
		uuid = strings.TrimSpace(uuid)
		if !ok || convErr != nil || uuid == "" {
			return nil, fmt.Errorf("unexpected nvidia-smi GPU line %q", line)
		}
		gpus = append(gpus, GPU{Index: n, UUID: uuid})
	}
	return gpus, nil
}

func (p NvidiaSMI) BusyUUIDs(ctx context.Context) (map[string]bool, error) {
	lines, err := p.query(ctx, "--query-compute-apps=gpu_uuid")
	if err != nil {
		return nil, err
	}
	busy := make(map[string]bool, len(lines))
	for _, line := range lines {
		busy[line] = true
	}
	return busy, nil
}
