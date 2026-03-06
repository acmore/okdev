package connect

import (
	"context"
	"io"
)

type ExecClient interface {
	ExecInteractive(ctx context.Context, namespace, pod string, tty bool, command []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error
}

func Run(ctx context.Context, client ExecClient, namespace, pod string, command []string, tty bool, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	return client.ExecInteractive(ctx, namespace, pod, tty, command, stdin, stdout, stderr)
}
