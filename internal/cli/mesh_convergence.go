package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/session"
)

func observeMeshReceiver(ctx context.Context, pod, hubBase, hubKey, recvBase, recvKey, hubID, recvID, folder string) meshReceiverHealth {
	h := meshReceiverHealth{Pod: pod}
	hubConn, err := syncthingPeerConnected(ctx, hubBase, hubKey, recvID)
	if err != nil {
		h.Err = err.Error()
		return h
	}
	recvConn, err := syncthingPeerConnected(ctx, recvBase, recvKey, hubID)
	if err != nil {
		h.Err = err.Error()
		return h
	}
	h.Connected = hubConn && recvConn
	if !h.Connected {
		h.Reason = "hub/receiver link disconnected"
		return h
	}
	h.NeedBytes, h.Reason, err = syncthingRevisionConvergence(ctx, hubBase, hubKey, recvBase, recvKey, hubID, recvID, folder)
	if err != nil {
		h.Err = err.Error()
		return h
	}
	h.InSync = h.Reason == ""
	return h
}

// Observations belong to a Pod UID. A replacement or membership change during
// probing must be checked again instead of inheriting the old Pod's success.
func reconcileMeshObservations(before, after []kube.PodSummary, observed []meshReceiverHealth) []meshReceiverHealth {
	old := make(map[string]string, len(before))
	current := make(map[string]string, len(after))
	for _, p := range before {
		old[p.Name] = p.UID
	}
	for _, p := range after {
		current[p.Name] = p.UID
	}
	for i := range observed {
		uid, ok := current[observed[i].Pod]
		if !ok || uid != old[observed[i].Pod] {
			observed[i] = meshReceiverHealth{Pod: observed[i].Pod, Reason: "receiver disappeared or was replaced during verification"}
		}
	}
	for _, p := range after {
		if _, ok := old[p.Name]; !ok {
			observed = append(observed, meshReceiverHealth{Pod: p.Name, Reason: "new receiver awaits verification"})
		}
	}
	return observed
}

func meshConvergenceReason(summary *meshHealthSummary, minimum int) string {
	if summary == nil {
		if minimum > 0 {
			return fmt.Sprintf("expected at least %d receivers; none currently discoverable", minimum)
		}
		return ""
	}
	if summary.Error != "" {
		return summary.Error
	}
	broken := brokenMeshReceiverPods(summary)
	if len(summary.Receivers) < minimum {
		return fmt.Sprintf("expected at least %d receivers; discovered %d", minimum, len(summary.Receivers))
	}
	if len(broken) > 0 {
		connected, converged := 0, 0
		for _, r := range summary.Receivers {
			if r.Connected {
				connected++
			}
			if r.Connected && r.InSync && r.Err == "" {
				converged++
			}
		}
		return fmt.Sprintf("receivers expected=%d connected=%d converged=%d; pending: %s", len(summary.Receivers), connected, converged, strings.Join(broken, ", "))
	}
	return ""
}

func meshConvergenceLabels(name string) map[string]string {
	labels := meshResetLabels(name)
	if info, err := session.LoadInfo(name); err == nil && info.RunID != "" {
		labels["okdev.io/run-id"] = info.RunID
	}
	return labels
}

func newMeshConvergenceCheck(ctx context.Context, cc *commandContext, hub, folder string, minimums ...int) (func(context.Context) error, error) {
	labels := meshConvergenceLabels(cc.sessionName)
	initial, err := listMeshReceivers(ctx, cc.kube, cc.namespace, labels, hub)
	if err != nil {
		return nil, err
	}
	minimum := len(initial)
	for _, floor := range minimums {
		minimum = max(minimum, floor)
	}
	var initialNames []string
	for _, p := range initial {
		initialNames = append(initialNames, p.Name)
	}
	return func(ctx context.Context) error {
		summary, err := checkMeshHealth(ctx, cc.opts, cc.kube, cc.namespace, cc.sessionName, labels, hub, folder)
		if err != nil {
			return err
		}
		if summary != nil && summary.Expected > minimum {
			minimum = summary.Expected
		}
		if reason := meshConvergenceReason(summary, minimum); reason != "" {
			return fmt.Errorf("%s: %s (initial receivers: %s)", folder, reason, strings.Join(initialNames, ", "))
		}
		return nil
	}, nil
}

type syncReceiverCoverage struct {
	Scope     string               `json:"scope"`
	Expected  int                  `json:"expected"`
	Connected int                  `json:"connected"`
	Converged int                  `json:"converged"`
	Pending   []string             `json:"pending,omitempty"`
	Pods      []meshReceiverHealth `json:"pods"`
}

func buildSyncReceiverCoverage(target meshReceiverHealth, mesh *meshHealthSummary, primary bool) *syncReceiverCoverage {
	c := &syncReceiverCoverage{Scope: "target-only", Pods: []meshReceiverHealth{target}}
	if primary {
		c.Scope = "target-and-private-workspace-mesh"
		if mesh != nil {
			c.Pods = append(c.Pods, mesh.Receivers...)
		}
	}
	c.Expected = len(c.Pods)
	for _, r := range c.Pods {
		if r.Connected {
			c.Connected++
		}
		if r.Connected && r.InSync && r.Err == "" {
			c.Converged++
		} else {
			c.Pending = append(c.Pending, r.Pod)
		}
	}
	return c
}
