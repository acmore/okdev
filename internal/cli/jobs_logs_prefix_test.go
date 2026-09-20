package cli

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/acmore/okdev/internal/kube"
)

func TestJobsLogsSinglePodPreservesUnprefixedContent(t *testing.T) {
	for _, follow := range []bool{false, true} {
		for _, content := range []string{"hello\nworld\n", "last line without newline", "", "\x00binary\xff\n"} {
			t.Run(fmt.Sprintf("follow=%v/content=%q", follow, content), func(t *testing.T) {
				pod := "okdev-long-generated-session-worker-0"
				key := "read"
				if follow {
					key = "follow"
				}
				client := &fakeJobsClient{
					listOutputs: map[string][]string{pod: {detachMetadataJSON("job-one", pod, "dev", 100, "exited", intPtr(0))}},
					streamPlans: map[string]fakeJobsStreamPlan{pod + "|" + key: {stdout: content, stderr: fmt.Sprintf("okdev-log-size:%d\n", len(content))}},
				}
				var out bytes.Buffer
				err := runJobsLogs(context.Background(), client, "default", []kube.PodSummary{{Name: pod, Phase: "Running"}}, "dev", "job-one", 1, jobsLogsOptions{Follow: follow, TailLines: -1}, &out)
				if err != nil {
					t.Fatal(err)
				}
				if got := out.String(); got != content {
					t.Fatalf("log bytes = %q, want %q", got, content)
				}
			})
		}
	}
}

func TestJobsLogsOffersNoPrefixFlag(t *testing.T) {
	cmd := newJobsLogsCmd(&Options{})
	flag := cmd.Flags().Lookup("no-prefix")
	if flag == nil {
		t.Fatal("missing --no-prefix flag")
	}
	if flag.DefValue != "false" {
		t.Fatalf("multi-pod attribution must stay enabled by default, got %q", flag.DefValue)
	}
}

func TestJobsLogsMultiPodPrefixControl(t *testing.T) {
	for _, follow := range []bool{false, true} {
		for _, noPrefix := range []bool{false, true} {
			t.Run(fmt.Sprintf("follow=%v/noPrefix=%v", follow, noPrefix), func(t *testing.T) {
				client := &fakeJobsClient{listOutputs: map[string][]string{}, streamPlans: map[string]fakeJobsStreamPlan{}}
				var pods []kube.PodSummary
				for i := 0; i < 2; i++ {
					pod := fmt.Sprintf("okdev-long-session-worker-%d", i)
					key := "read"
					if follow {
						key = "follow"
					}
					content := fmt.Sprintf("line-%d\n", i)
					client.listOutputs[pod] = []string{detachMetadataJSON("job-two", pod, "dev", 100+i, "exited", intPtr(0))}
					client.streamPlans[pod+"|"+key] = fakeJobsStreamPlan{stdout: content, stderr: fmt.Sprintf("okdev-log-size:%d\n", len(content))}
					pods = append(pods, kube.PodSummary{Name: pod, Phase: "Running"})
				}
				var out bytes.Buffer
				err := runJobsLogs(context.Background(), client, "default", pods, "dev", "job-two", 2, jobsLogsOptions{Follow: follow, TailLines: -1, NoPrefix: noPrefix}, &out)
				if err != nil {
					t.Fatal(err)
				}
				got := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
				sort.Strings(got)
				want := []string{"[worker-0] line-0", "[worker-1] line-1"}
				if noPrefix {
					want = []string{"line-0", "line-1"}
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("lines = %q, want %q", got, want)
				}
			})
		}
	}
}
