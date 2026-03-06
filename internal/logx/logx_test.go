package logx

import (
	"context"
	"log/slog"
	"testing"
)

func TestConfigureVerbosity(t *testing.T) {
	Configure(false)
	if slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("debug should be disabled when verbose=false")
	}
	if !slog.Default().Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("info should be enabled when verbose=false")
	}

	Configure(true)
	if !slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("debug should be enabled when verbose=true")
	}
}
