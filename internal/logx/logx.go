package logx

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/klog/v2"
)

var (
	logWriterMu sync.Mutex
	logWriter   io.Writer
)

func Configure(verbose bool) {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	// Route client-go/kubectl transport noise (for example port-forward stream
	// resets/timeouts) into the shared okdev log instead of the interactive TTY.
	klog.LogToStderr(false)
	writer := okdevLogWriter()
	klog.SetOutput(writer)
	klog.SetOutputBySeverity("INFO", writer)
	klog.SetOutputBySeverity("WARNING", writer)
	klog.SetOutputBySeverity("ERROR", writer)
	klog.SetOutputBySeverity("FATAL", writer)
	configureKubernetesRuntimeErrorLogging(writer)
}

func okdevLogWriter() io.Writer {
	logWriterMu.Lock()
	defer logWriterMu.Unlock()
	if logWriter != nil {
		return logWriter
	}
	home, err := os.UserHomeDir()
	if err != nil {
		logWriter = io.Discard
		return logWriter
	}
	logDir := filepath.Join(home, ".okdev", "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		logWriter = io.Discard
		return logWriter
	}
	f, err := os.OpenFile(filepath.Join(logDir, "okdev.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		logWriter = io.Discard
		return logWriter
	}
	logWriter = f
	return logWriter
}

func configureKubernetesRuntimeErrorLogging(w io.Writer) {
	handler := func(_ context.Context, err error, msg string, keysAndValues ...interface{}) {
		if msg == "" {
			msg = "Unhandled Error"
		}
		_, _ = fmt.Fprintf(w, "time=%s source=k8s-runtime msg=%q err=%q", timeNow().Format("2006-01-02T15:04:05.000Z07:00"), msg, errString(err))
		if len(keysAndValues) > 0 {
			_, _ = fmt.Fprintf(w, " kv=%v", keysAndValues)
		}
		_, _ = io.WriteString(w, "\n")
	}
	if len(utilruntime.ErrorHandlers) == 0 {
		utilruntime.ErrorHandlers = []utilruntime.ErrorHandler{handler}
		return
	}
	utilruntime.ErrorHandlers[0] = handler
}

var timeNow = time.Now

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
