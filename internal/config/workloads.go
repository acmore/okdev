package config

import (
	"fmt"
	"strings"
)

// DefaultWorkloadProfileName is the name given to the profile desugared from a
// legacy singular spec.workload.
const DefaultWorkloadProfileName = "default"

// WorkloadProfile is one named workload a config can switch between. It owns
// exactly the fields the singular spec.workload owns, plus a name; everything
// else in the spec is shared across profiles.
type WorkloadProfile struct {
	Name         string               `yaml:"name"`
	Type         string               `yaml:"type"`
	ManifestPath string               `yaml:"manifestPath,omitempty"`
	Inject       []WorkloadInjectSpec `yaml:"inject,omitempty"`
	Attach       WorkloadAttachSpec   `yaml:"attach,omitempty"`
}

// DeclaresWorkloadProfiles reports whether the config literally declared
// spec.workloads, as opposed to having one desugared from spec.workload.
//
// Drift detection depends on this: the profile name only enters the workload
// snapshot for configs that declare profiles, so a legacy config's snapshot
// hash is bit-for-bit what it was before profiles existed and existing
// sessions do not all report drift on their next `okdev up`.
func (d *DevEnvironment) DeclaresWorkloadProfiles() bool {
	return d != nil && d.declaredWorkloadProfiles
}

func (d *DevEnvironment) setWorkloadDefaults() {
	if len(d.Spec.Workloads) > 0 {
		d.declaredWorkloadProfiles = true
	} else {
		d.Spec.Workloads = []WorkloadProfile{{
			Name:         DefaultWorkloadProfileName,
			Type:         d.Spec.Workload.Type,
			ManifestPath: d.Spec.Workload.ManifestPath,
			Inject:       d.Spec.Workload.Inject,
			Attach:       d.Spec.Workload.Attach,
		}}
	}
	for i := range d.Spec.Workloads {
		p := &d.Spec.Workloads[i]
		if strings.TrimSpace(p.Type) == "" {
			p.Type = "pod"
		}
		if p.Type == "job" && len(p.Inject) == 0 {
			p.Inject = []WorkloadInjectSpec{{Path: "spec.template"}}
		}
	}
}

// SelectWorkload collapses one profile into Spec.Workload, which every
// downstream reader treats as the effective workload for this invocation.
//
// An empty name means "whatever this config says by default": Spec.DefaultWorkload
// when set, otherwise the first declared profile.
func (d *DevEnvironment) SelectWorkload(name string) error {
	if d == nil || len(d.Spec.Workloads) == 0 {
		return nil
	}
	want := strings.TrimSpace(name)
	if want == "" {
		want = strings.TrimSpace(d.Spec.DefaultWorkload)
	}
	if want == "" {
		want = strings.TrimSpace(d.Spec.Workloads[0].Name)
	}
	for _, p := range d.Spec.Workloads {
		if strings.TrimSpace(p.Name) != want {
			continue
		}
		d.Spec.Workload = WorkloadSpec{
			Type:         p.Type,
			ManifestPath: p.ManifestPath,
			Inject:       p.Inject,
			Attach:       p.Attach,
		}
		d.selectedWorkload = want
		return nil
	}
	return fmt.Errorf("unknown workload %q; available: %s", want, strings.Join(d.WorkloadProfileNames(), ", "))
}

// SelectedWorkload names the profile currently collapsed into Spec.Workload.
func (d *DevEnvironment) SelectedWorkload() string {
	if d == nil {
		return ""
	}
	if s := strings.TrimSpace(d.selectedWorkload); s != "" {
		return s
	}
	if len(d.Spec.Workloads) > 0 {
		return strings.TrimSpace(d.Spec.Workloads[0].Name)
	}
	return ""
}

func (d *DevEnvironment) WorkloadProfileNames() []string {
	if d == nil {
		return nil
	}
	names := make([]string, 0, len(d.Spec.Workloads))
	for _, p := range d.Spec.Workloads {
		names = append(names, strings.TrimSpace(p.Name))
	}
	return names
}
