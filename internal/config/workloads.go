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

// validateWorkloadProfile applies the per-type rules that used to guard the
// singular spec.workload. Every declared profile is checked, not only the one
// that happens to be selected, so a broken profile fails fast instead of at
// switch time.
func validateWorkloadProfile(p WorkloadProfile, index int, interPod bool) error {
	field := fmt.Sprintf("spec.workloads[%d]", index)
	switch strings.TrimSpace(p.Type) {
	case "", "pod", "job", "pytorchjob", "generic":
	default:
		return fmt.Errorf("%s.type must be one of pod, job, pytorchjob, generic, got %q", field, p.Type)
	}
	switch strings.TrimSpace(p.Type) {
	case "job", "pytorchjob", "generic":
		if strings.TrimSpace(p.ManifestPath) == "" {
			return fmt.Errorf("%s.manifestPath is required when type=%q", field, p.Type)
		}
	}
	isPod := isPodWorkloadType(p.Type)
	inject := p.Inject
	if len(inject) == 0 && isPod {
		inject = []WorkloadInjectSpec{{Path: ""}}
	}
	for i, in := range inject {
		if strings.TrimSpace(in.Path) == "" && !isPod {
			return fmt.Errorf("%s.inject[%d].path is required", field, i)
		}
		sidecar := in.Sidecar
		if interPod {
			enabled := true
			sidecar = &enabled
		}
		if in.Attachable != nil && *in.Attachable && sidecar != nil && !*sidecar {
			return fmt.Errorf("%s.inject[%d]: attachable=true requires sidecar=true", field, i)
		}
	}
	if strings.TrimSpace(p.Type) == "job" {
		for i, in := range p.Inject {
			if strings.TrimSpace(in.Path) != "spec.template" {
				return fmt.Errorf("%s.inject[%d].path must be spec.template when type=job", field, i)
			}
		}
	}
	if (p.Type == "generic" || p.Type == "pytorchjob") && len(p.Inject) == 0 {
		return fmt.Errorf("%s.inject is required when type=%q", field, p.Type)
	}
	return nil
}

// validateWorkloadProfiles checks names and cross-profile invariants.
func (d *DevEnvironment) validateWorkloadProfiles() error {
	seen := make(map[string]struct{}, len(d.Spec.Workloads))
	for i, p := range d.Spec.Workloads {
		name := strings.TrimSpace(p.Name)
		if name == "" {
			return fmt.Errorf("spec.workloads[%d].name is required", i)
		}
		if _, dup := seen[name]; dup {
			return fmt.Errorf("spec.workloads[%d].name %q is declared more than once", i, name)
		}
		seen[name] = struct{}{}
		if err := validateWorkloadProfile(p, i, d.Spec.SSH.InterPodEnabled()); err != nil {
			return err
		}
	}
	if def := strings.TrimSpace(d.Spec.DefaultWorkload); def != "" {
		if _, ok := seen[def]; !ok {
			return fmt.Errorf("spec.defaultWorkload %q names no declared workload; available: %s",
				def, strings.Join(d.WorkloadProfileNames(), ", "))
		}
	}
	// A pod profile without a manifestPath is synthesized from the shared
	// spec.podTemplate, so two of them would be byte-identical — the very
	// ambiguity profiles exist to remove. At most one may rely on it.
	shared := make([]string, 0, 2)
	for _, p := range d.Spec.Workloads {
		if isPodWorkloadType(p.Type) && strings.TrimSpace(p.ManifestPath) == "" {
			shared = append(shared, strings.TrimSpace(p.Name))
		}
	}
	if len(shared) > 1 {
		return fmt.Errorf("workloads %s are all pod profiles without a manifestPath, so they would share spec.podTemplate and be indistinguishable; give all but one its own manifestPath",
			strings.Join(shared, ", "))
	}
	return nil
}
