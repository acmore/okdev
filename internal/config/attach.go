package config

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"
)

// AttachOnlySpec identifies existing pods without declaring a workload to manage.
type AttachOnlySpec struct {
	Pods      []string `yaml:"pods,omitempty"`
	Selector  string   `yaml:"selector,omitempty"`
	Session   string   `yaml:"session,omitempty"`
	Container string   `yaml:"container"`
	Setup     string   `yaml:"setup,omitempty"`
}

func (d *DevEnvironment) validateAttachOnly() error {
	a := d.Spec.AttachOnly
	w := d.Spec.Workload
	if w.Type != "" || w.ManifestPath != "" || len(w.Inject) != 0 || w.Attach.Container != "" || len(d.Spec.Workloads) != 0 || d.Spec.DefaultWorkload != "" {
		return fmt.Errorf("spec.attachOnly cannot be combined with workload/workloads/defaultWorkload")
	}
	n := 0
	if len(a.Pods) > 0 {
		n++
	}
	if strings.TrimSpace(a.Selector) != "" {
		n++
	}
	if strings.TrimSpace(a.Session) != "" {
		n++
	}
	if n != 1 {
		return fmt.Errorf("spec.attachOnly requires exactly one of pods, selector, session")
	}
	if strings.TrimSpace(a.Container) == "" {
		return fmt.Errorf("spec.attachOnly.container is required")
	}
	if errs := validation.IsDNS1123Label(a.Container); len(errs) != 0 {
		return fmt.Errorf("invalid spec.attachOnly.container: %s", strings.Join(errs, "; "))
	}
	for _, name := range a.Pods {
		if errs := validation.IsDNS1123Subdomain(name); len(errs) != 0 {
			return fmt.Errorf("invalid attach-only pod %q", name)
		}
	}
	if a.Session != "" {
		if errs := validation.IsValidLabelValue(a.Session); len(errs) != 0 {
			return fmt.Errorf("invalid attach-only session %q", a.Session)
		}
	}
	if a.Selector != "" {
		if _, err := labels.Parse(a.Selector); err != nil {
			return fmt.Errorf("invalid attach-only selector: %w", err)
		}
	}
	if d.Spec.SSH.InterPodEnabled() || d.Spec.Exec.FanoutMode == ExecFanoutGateway {
		return fmt.Errorf("attach-only mode requires direct exec; interPod SSH/gateway is unavailable")
	}
	return nil
}
