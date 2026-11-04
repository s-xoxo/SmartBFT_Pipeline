// Command trend runs the W x RTT matrix of the pipelining benchmark on one machine and prints a
// markdown table; the raw numbers go to a CSV.
package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"log"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/s-xoxo/SmartBFT_Pipeline/bench/lib"
)

func main() {
	ws := flag.String("w", "1,2,4,8", "comma-separated PipelineDepthMax values")
	rtts := flag.String("rtt", "0,50,100", "comma-separated round-trip times in ms (one-way delay is RTT/2)")
	n := flag.Int("n", 4, "number of nodes")
	k := flag.Int("k", 256, "closed-loop concurrency (outstanding requests)")
	batch := flag.Uint64("batch", 50, "RequestBatchMaxCount")
	interval := flag.Duration("batch-interval", 50*time.Millisecond, "RequestBatchMaxInterval")
	warmup := flag.Uint64("warmup", 30, "decisions before measuring starts")
	duration := flag.Duration("duration", 15*time.Second, "measurement window per cell")
	out := flag.String("out", "trend.csv", "CSV output path")
	flag.Parse()

	wList := parseList(*ws)
	rttList := parseList(*rtts)
	fmt.Fprintf(os.Stderr, "trend: N=%d K=%d batch=%d warmup=%d duration=%s W=%v RTT(ms)=%v; %s %s/%s %d CPUs\n",
		*n, *k, *batch, *warmup, *duration, wList, rttList, runtime.Version(), runtime.GOOS, runtime.GOARCH, runtime.NumCPU())

	f, err := os.Create(*out)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	w := csv.NewWriter(f)
	_ = w.Write(strings.Split("w,rtt_ms,n,k,batch,duration_s,decisions,requests,tps,decisions_per_s,req_per_decision,"+
		"lat_mean_ms,lat_p50_ms,lat_p99_ms,lat_max_ms,deliver_mean_us,max_in_flight,view_changes,syncs,warnings,dropped,follower_lag,note", ","))

	cells := map[[2]uint64]string{}
	for _, rtt := range rttList {
		for _, depth := range wList {
			cfg := lib.RunConfig{N: *n, W: depth, OneWayDelay: time.Duration(rtt) * time.Millisecond / 2,
				BatchMaxCount: *batch, BatchInterval: *interval, Concurrency: *k, WarmupDecisions: *warmup, Duration: *duration}
			fmt.Fprintf(os.Stderr, "cell W=%d RTT=%dms ... ", depth, rtt)
			res, err := lib.Run(cfg)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
				cells[[2]uint64{rtt, depth}] = "error"
				_ = w.Write([]string{u(depth), u(rtt), strconv.Itoa(*n), strconv.Itoa(*k), u(*batch), fs(duration.Seconds()),
					"", "", "", "", "", "", "", "", "", "", "", "", "", "", "", "", err.Error()})
				continue
			}
			rpd := 0.0
			if res.Decisions > 0 {
				rpd = float64(res.Requests) / float64(res.Decisions)
			}
			fmt.Fprintf(os.Stderr, "tps=%.0f dec/s=%.1f req/dec=%.1f p50=%s p99=%s deliver=%s inflight=%.0f%s\n",
				res.TPS, res.DecisionsPerSec, rpd, ms(res.P50), ms(res.P99), res.DeliverMean, res.MaxInFlight, noteSuffix(res.Note))
			cells[[2]uint64{rtt, depth}] = fmt.Sprintf("%.0f / %s", res.TPS, ms(res.P50)) + noteSuffix(res.Note)
			_ = w.Write([]string{u(depth), u(rtt), strconv.Itoa(*n), strconv.Itoa(*k), u(*batch), fs(duration.Seconds()),
				u(res.Decisions), u(res.Requests), fs(res.TPS), fs(res.DecisionsPerSec), fs(rpd),
				fs(msf(res.MeanLat)), fs(msf(res.P50)), fs(msf(res.P99)), fs(msf(res.MaxLat)),
				fs(float64(res.DeliverMean.Nanoseconds()) / 1e3), fs(res.MaxInFlight),
				u(res.ViewChanges), u(res.Syncs), u(res.Warnings), u(res.Dropped), strconv.FormatInt(res.FollowerLag, 10), strings.TrimSpace(res.Note)})
			w.Flush()
		}
	}

	// Markdown table: rows RTT, columns W, cell = TPS / p50.
	fmt.Printf("\n| RTT (ms) |")
	for _, depth := range wList {
		fmt.Printf(" W=%d |", depth)
	}
	fmt.Printf("\n|---|")
	for range wList {
		fmt.Printf("---|")
	}
	for _, rtt := range rttList {
		fmt.Printf("\n| %d |", rtt)
		for _, depth := range wList {
			fmt.Printf(" %s |", cells[[2]uint64{rtt, depth}])
		}
	}
	fmt.Printf("\n\ncell = TPS / p50 latency; CSV written to %s\n", *out)
}

func noteSuffix(note string) string {
	if note = strings.TrimSpace(note); note != "" {
		return " (!" + note + ")"
	}
	return ""
}

func parseList(s string) []uint64 {
	var out []uint64
	for _, p := range strings.Split(s, ",") {
		v, err := strconv.ParseUint(strings.TrimSpace(p), 10, 64)
		if err != nil {
			log.Fatalf("bad list element %q: %v", p, err)
		}
		out = append(out, v)
	}
	return out
}

func u(v uint64) string           { return strconv.FormatUint(v, 10) }
func fs(v float64) string         { return strconv.FormatFloat(v, 'f', 2, 64) }
func msf(d time.Duration) float64 { return float64(d.Nanoseconds()) / 1e6 }
func ms(d time.Duration) string   { return fmt.Sprintf("%.1fms", msf(d)) }
