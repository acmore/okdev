package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"
	"time"
)

func TestCopyStatsExcludeReusedBytesFromRate(t *testing.T) {
	p := newSinglePodProgress(io.Discard, "copy")
	p.started = time.Unix(100, 0)
	p.addExistingBytes(900)
	p.addBytes(100)
	p.addBytes(50) // Retransmitted bytes still crossed the application stream.
	var out bytes.Buffer
	p.writeStats(&out, false, false, p.started.Add(2*time.Second))
	var got cpTransferStats
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Event != "cp_stats" || got.StreamBytes != 150 || got.ReusedBytes != 900 || got.AverageBytesPerSecond != 75 || got.ElapsedSeconds != 2 || got.Success || got.Targets != 1 || got.Direction != "download" {
		t.Fatalf("%+v", got)
	}
	if p.enabled {
		t.Fatal("noninteractive statistics enabled spinner")
	}
}

func TestCopyStatsAggregateAndZeroTime(t *testing.T) {
	p := newMultiPodProgress(io.Discard, 2)
	p.addBytes(32)
	p.addBytes(32)
	got := p.transferStats(true, true, p.started)
	if got.StreamBytes != 64 || got.Targets != 2 || got.AverageBytesPerSecond != 0 || !got.Success || got.Direction != "upload" {
		t.Fatalf("%+v", got)
	}
	got = p.transferStats(true, true, p.started.Add(-time.Second))
	if got.ElapsedSeconds != 0 {
		t.Fatalf("%+v", got)
	}
}
