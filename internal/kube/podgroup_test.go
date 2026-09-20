package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestPodGroupDiagnosticsScopesEventsAndKeepsNewestTen(t *testing.T) {
	var events []corev1.Event
	for i := 0; i < 12; i++ {
		events = append(events, corev1.Event{Reason: fmt.Sprintf("event-%d", i), Count: 1,
			LastTimestamp:  metav1.NewTime(time.Unix(int64(i+1), 0)),
			InvolvedObject: corev1.ObjectReference{Kind: "PodGroup", Name: "group", UID: types.UID("current")}})
	}
	events[0].Series = &corev1.EventSeries{Count: 7, LastObservedTime: metav1.NewMicroTime(time.Unix(50, 0))}
	for _, ref := range []corev1.ObjectReference{
		{Kind: "Pod", Name: "group", UID: "current"},
		{Kind: "PodGroup", Name: "group", UID: "old"},
		{Kind: "PodGroup", Name: "other", UID: "current"},
	} {
		events = append(events, corev1.Event{Reason: "unrelated", InvolvedObject: ref, LastTimestamp: metav1.NewTime(time.Unix(100, 0))})
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/apis/scheduling.volcano.sh/v1beta1/namespaces/demo/podgroups/group":
			_, _ = w.Write([]byte(`{"apiVersion":"scheduling.volcano.sh/v1beta1","kind":"PodGroup","metadata":{"name":"group","uid":"current"},"spec":{"queue":"research","minMember":2},"status":{"phase":"Pending","conditions":[{"type":"Unschedulable","status":"True","reason":"QueueQuotaExceeded","message":"GPU quota exhausted"}]}}`))
		case "/api/v1/namespaces/demo/events":
			selector := r.URL.Query().Get("fieldSelector")
			for _, field := range []string{"involvedObject.kind=PodGroup", "involvedObject.name=group", "involvedObject.uid=current"} {
				if !strings.Contains(selector, field) {
					t.Errorf("missing selector %q: %s", field, selector)
				}
			}
			_ = json.NewEncoder(w).Encode(corev1.EventList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "EventList"}, Items: events})
		default:
			t.Errorf("unexpected API request: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	t.Setenv("KUBECONFIG", writeTLSTestKubeconfig(t, server))
	got, err := (&Client{Context: "dev"}).GetPodGroupDiagnostics(context.Background(), "demo", "group")
	if err != nil {
		t.Fatal(err)
	}
	if got.Queue != "research" || got.MinMember != 2 || got.Phase != "Pending" || len(got.Conditions) != 1 || got.Conditions[0].Message != "GPU quota exhausted" {
		t.Fatalf("missing scheduler fields: %+v", got)
	}
	if len(got.Events) != 10 || got.Events[9].Reason != "event-0" || got.Events[9].Count != 7 || got.Events[9].LastSeen != "1970-01-01T00:00:50Z" {
		t.Fatalf("incorrect recent events: %+v", got.Events)
	}
	for _, event := range got.Events {
		if event.Reason == "unrelated" {
			t.Fatal("included a different object incarnation's event")
		}
	}
}

func TestPodGroupDiagnosticsOptionalFailures(t *testing.T) {
	for _, mode := range []string{"absent", "forbidden", "events-forbidden"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if mode == "events-forbidden" && strings.Contains(r.URL.Path, "/podgroups/") {
					_, _ = w.Write([]byte(`{"apiVersion":"scheduling.volcano.sh/v1beta1","kind":"PodGroup","metadata":{"name":"group","uid":"current"},"spec":{"queue":"research"}}`))
					return
				}
				code, reason := 403, "Forbidden"
				if mode == "absent" {
					code, reason = 404, "NotFound"
				}
				w.WriteHeader(code)
				_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Failure", Code: int32(code), Reason: metav1.StatusReason(reason), Message: strings.ToLower(reason)})
			}))
			defer server.Close()
			t.Setenv("KUBECONFIG", writeTLSTestKubeconfig(t, server))
			got, err := (&Client{Context: "dev"}).GetPodGroupDiagnostics(context.Background(), "demo", "group")
			switch mode {
			case "absent":
				if !apierrors.IsNotFound(err) {
					t.Fatalf("expected NotFound, got %v", err)
				}
			case "forbidden":
				if !apierrors.IsForbidden(err) {
					t.Fatalf("expected Forbidden, got %v", err)
				}
			default:
				if err != nil || got.Queue != "research" || !strings.Contains(got.EventsError, "forbidden") {
					t.Fatalf("lost partial diagnostics: %+v err=%v", got, err)
				}
			}
		})
	}
}
