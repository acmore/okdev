package logx

import (
	"io"
	"log/slog"
	"os"

	"k8s.io/klog/v2"
)

func Configure(verbose bool) {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	// Silence client-go/kubectl transport noise (for example port-forward
	// stream resets/timeouts) so interactive shells do not get spammed with
	// low-level klog messages. okdev surfaces actionable errors separately.
	klog.LogToStderr(false)
	klog.SetOutput(io.Discard)
	klog.SetOutputBySeverity("INFO", io.Discard)
	klog.SetOutputBySeverity("WARNING", io.Discard)
	klog.SetOutputBySeverity("ERROR", io.Discard)
	klog.SetOutputBySeverity("FATAL", io.Discard)
}
