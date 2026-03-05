package cli

import (
	"fmt"
	"time"

	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/session"
	"github.com/spf13/cobra"
)

func newUpCmd(opts *Options) *cobra.Command {
	var attach bool
	var waitTimeout time.Duration

	cmd := &cobra.Command{
		Use:   "up",
		Short: "Create or resume a dev session",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, ns, err := loadConfigAndNamespace(opts)
			if err != nil {
				return err
			}
			sn, err := resolveSessionName(opts, cfg)
			if err != nil {
				return err
			}
			labels := labelsForSession(cfg, sn)
			pvc := pvcName(cfg, sn)
			pod := podName(sn)

			ctx, cancel := defaultContext()
			defer cancel()
			k := newKubeClient(opts)

			if cfg.Spec.Workspace.PVC.ClaimName == "" {
				pvcManifest, err := kube.BuildPVCManifest(ns, pvc, cfg.Spec.Workspace.PVC.Size, cfg.Spec.Workspace.PVC.StorageClassName, labels)
				if err != nil {
					return err
				}
				if err := k.Apply(ctx, ns, pvcManifest); err != nil {
					return err
				}
			}

			podManifest, err := kube.BuildPodManifest(ns, pod, pvc, labels, cfg.Spec.PodTemplate.Spec)
			if err != nil {
				return err
			}
			if err := k.Apply(ctx, ns, podManifest); err != nil {
				return err
			}
			if err := k.WaitReady(ctx, ns, pod, waitTimeout); err != nil {
				return err
			}

			if err := session.SaveActiveSession(sn); err != nil {
				return err
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Session ready: %s (namespace: %s)\n", sn, ns)
			if attach {
				return runConnect(opts, ns, sn, []string{"/bin/bash"}, true)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&attach, "attach", false, "Print attach hint after startup")
	cmd.Flags().DurationVar(&waitTimeout, "wait-timeout", 3*time.Minute, "Wait timeout for pod readiness")
	return cmd
}
