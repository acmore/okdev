package logx

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/klog/v2"
)

var (
	logWriterMu sync.Mutex
	logWriter   io.Writer
)

const (
	maxLogSizeBytes = 10 * 1024 * 1024
	maxLogBackups   = 5
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
	// LogToStderr(false) alone is not enough: klog still duplicates records at
	// or above its stderrthreshold (default ERROR) onto os.Stderr, which is how
	// client-go errors like "Websocket Ping failed" leaked into exec's terminal
	// streams (#176). Raise the threshold so only FATAL — which aborts the
	// process anyway — reaches stderr.
	var klogFlags flag.FlagSet
	klog.InitFlags(&klogFlags)
	_ = klogFlags.Set("stderrthreshold", "FATAL")
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
	f, err := OpenRotatingLog(filepath.Join(logDir, "okdev.log"))
	if err != nil {
		logWriter = io.Discard
		return logWriter
	}
	logWriter = f
	return logWriter
}

func OpenRotatingLog(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if err := rotateLogIfNeeded(path, maxLogSizeBytes, maxLogBackups); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

func Printf(format string, args ...any) {
	_, _ = fmt.Fprintf(okdevLogWriter(), format, args...)
}

func rotateLogIfNeeded(path string, maxSize int64, maxBackups int) error {
	if maxSize <= 0 || maxBackups <= 0 {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Size() < maxSize {
		return nil
	}
	oldest := path + "." + strconv.Itoa(maxBackups)
	if err := os.Remove(oldest); err != nil && !os.IsNotExist(err) {
		return err
	}
	for i := maxBackups - 1; i >= 1; i-- {
		src := path + "." + strconv.Itoa(i)
		dst := path + "." + strconv.Itoa(i+1)
		if err := os.Rename(src, dst); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.Rename(path, path+".1"); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
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
