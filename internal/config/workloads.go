package config

import "strings"

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
