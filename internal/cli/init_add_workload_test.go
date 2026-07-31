package cli

import (
	"strings"
	"testing"
)

func TestValidateInitInvocation(t *testing.T) {
	cases := []struct {
		name    string
		inv     initInvocation
		wantErr string // substring; "" means it must be accepted
	}{
		{
			name: "fresh config, plain init",
			inv:  initInvocation{},
		},
		{
			name: "fresh config with project flags",
			inv:  initInvocation{ProjectFlagsSet: []string{"namespace"}},
		},
		{
			name:    "fresh config with a workload name",
			inv:     initInvocation{WorkloadName: "train"},
			wantErr: "--workload-name",
		},
		{
			name: "existing config, additive",
			inv:  initInvocation{ConfigExists: true, WorkloadName: "train"},
		},
		{
			name:    "existing config without a workload name",
			inv:     initInvocation{ConfigExists: true},
			wantErr: "already exists",
		},
		{
			name: "existing config with --force rewrites wholesale",
			inv:  initInvocation{ConfigExists: true, Force: true},
		},
		{
			name:    "existing config with --force and a workload name",
			inv:     initInvocation{ConfigExists: true, Force: true, WorkloadName: "train"},
			wantErr: "--force",
		},
		{
			name:    "existing config with a project flag",
			inv:     initInvocation{ConfigExists: true, WorkloadName: "train", ProjectFlagsSet: []string{"namespace", "ssh-user"}},
			wantErr: "namespace",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateInitInvocation(tc.inv)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected the invocation to be accepted, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error mentioning %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q should mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestInitAdditiveMode(t *testing.T) {
	if !initAdditiveMode(initInvocation{ConfigExists: true, WorkloadName: "train"}) {
		t.Error("an existing config plus a workload name is the additive path")
	}
	if initAdditiveMode(initInvocation{WorkloadName: "train"}) {
		t.Error("a fresh config is never additive")
	}
	if initAdditiveMode(initInvocation{ConfigExists: true, Force: true}) {
		t.Error("--force rewrites wholesale, it does not append")
	}
}
