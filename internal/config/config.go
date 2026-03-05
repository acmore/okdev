package config

import (
	"errors"
	"fmt"
)

// DevEnvironment is the top-level config structure for .okdev.yaml.
type DevEnvironment struct {
	APIVersion string     `yaml:"apiVersion"`
	Kind       string     `yaml:"kind"`
	Metadata   Metadata   `yaml:"metadata"`
	Spec       DevEnvSpec `yaml:"spec"`
}

type Metadata struct {
	Name string `yaml:"name"`
}

type DevEnvSpec struct {
	Namespace string      `yaml:"namespace"`
	Session   SessionSpec `yaml:"session"`
	Workspace Workspace   `yaml:"workspace"`
}

type SessionSpec struct {
	DefaultNameTemplate string `yaml:"defaultNameTemplate"`
}

type Workspace struct {
	MountPath string `yaml:"mountPath"`
}

func (d *DevEnvironment) Validate() error {
	if d == nil {
		return errors.New("config is nil")
	}
	if d.APIVersion == "" {
		return errors.New("apiVersion is required")
	}
	if d.Kind == "" {
		return errors.New("kind is required")
	}
	if d.Kind != "DevEnvironment" {
		return fmt.Errorf("kind must be DevEnvironment, got %q", d.Kind)
	}
	if d.Metadata.Name == "" {
		return errors.New("metadata.name is required")
	}
	if d.Spec.Workspace.MountPath == "" {
		return errors.New("spec.workspace.mountPath is required")
	}
	return nil
}
