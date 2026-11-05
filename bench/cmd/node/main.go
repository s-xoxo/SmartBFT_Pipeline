// Command node runs ONE SmartBFT consensus node in its own OS process, talking to its peers over
// TCP with an emulated one-way delay (RTT/2) applied on the sender side. The leader process (or any
// process given -k) also runs the closed-loop client. At the end it writes a lib.NodeResult JSON.
//
// Sync: when the library asks the node to sync (it fell behind), App.Sync fetches the missing
// decisions from its peers through the same TCP connections (frameSyncReq/frameSyncResp, served
// from each node's in-memory Ledger of the last 4096 decisions), delivers them in order, and
// returns the latest one. A node that is further behind than the ledger keeps is handed the
// latest decision only and jumps forward.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/s-xoxo/SmartBFT_Pipeline/bench/lib"
)

// GapThreshold is the pause between deliveries that counts as a gap (crash detection).
const GapThreshold = time.Second

const postWindow = 15 * time.Second

type recorder struct {
	mu        sync.Mutex
	start     time.Time
	perSecond []uint64
	lastAt    time.Time
	lastSeq   uint64
	gaps      []lib.Gap
	// view changes
	viewStarts  []time.Time
	firstAfter  time.Time // first delivery after the first view start
	postVC      uint64
	postVCDone  bool
	viewStartNs []int64
}

func (r *recorder) onViewStart(_ uint64, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.viewStarts = append(r.viewStarts, at)
	r.viewStartNs = append(r.viewStartNs, at.UnixNano())
}

func (r *recorder) onDecision(at time.Time, seq uint64, requests int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sec := int(at.Sub(r.start) / time.Second)
	for len(r.perSecond) <= sec {
		r.perSecond = append(r.perSecond, 0)
	}
	if sec >= 0 {
		r.perSecond[sec] += uint64(requests)
	}
	if !r.lastAt.IsZero() && at.Sub(r.lastAt) >= GapThreshold {
		r.gaps = append(r.gaps, lib.Gap{BeforeUnixNano: r.lastAt.UnixNano(), AfterUnixNano: at.UnixNano(),
			BeforeSeq: r.lastSeq, AfterSeq: seq})
	}
	for i := range r.gaps { // every gap counts its own 15 s window
		g := &r.gaps[i]
		if g.Post15sComplete {
			continue
		}
		if at.Before(time.Unix(0, g.AfterUnixNano).Add(postWindow)) {
			g.Post15sRequests += uint64(requests)
		} else {
			g.Post15sComplete = true
		}
	}
	if len(r.viewStarts) > 0 && at.After(r.viewStarts[0]) {
		if r.firstAfter.IsZero() {
			r.firstAfter = at
		}
		if !r.postVCDone {
			if at.Before(r.firstAfter.Add(postWindow)) {
				r.postVC += uint64(requests)
			} else {
				r.postVCDone = true
			}
		}
	}
	r.lastAt, r.lastSeq = at, seq
}

// fill writes the recorder's data into the result; now is the end of the window.
func (r *recorder) fill(res *lib.NodeResult, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	res.PerSecond = append([]uint64(nil), r.perSecond...)
	res.Gaps = append([]lib.Gap(nil), r.gaps...)
	for i := range res.Gaps {
		if !now.Before(time.Unix(0, res.Gaps[i].AfterUnixNano).Add(postWindow)) {
			res.Gaps[i].Post15sComplete = true
		}
	}
	res.ViewStartUnixNanos = append([]int64(nil), r.viewStartNs...)
	if !r.firstAfter.IsZero() {
		res.FirstDeliveryAfterViewChangeUnixNano = r.firstAfter.UnixNano()
		res.PostViewChange15sRequests = r.postVC
		res.PostViewChange15sComplete = r.postVCDone || !now.Before(r.firstAfter.Add(postWindow))
	}
}

func main() {
	id := flag.Uint64("id", 1, "this node's id (1-based)")
	n := flag.Int("n", 4, "number of nodes (must match -peers)")
	peers := flag.String("peers", "1=127.0.0.1:23001,2=127.0.0.1:23002,3=127.0.0.1:23003,4=127.0.0.1:23004", "id=host:port list of ALL nodes")
	listen := flag.String("listen", "", "listen address (default: this node's entry in -peers)")
	w := flag.Uint64("w", 1, "PipelineDepthMax")
	rtt := flag.Uint64("rtt", 0, "emulated RTT in ms (one-way delay RTT/2 per link, sender side)")
	batch := flag.Uint64("batch", 50, "RequestBatchMaxCount")
	interval := flag.Duration("batch-interval", 50*time.Millisecond, "RequestBatchMaxInterval")
	k := flag.Int("k", 0, "closed-loop client workers in this process (0 = no client)")
	submitTo := flag.String("submit-to", "", "comma-separated node ids the workers submit to, round-robin per worker (default: this node)")
	resubmit := flag.Duration("resubmit", 0, "resubmit a request not delivered within this time, to the next target (0 = never)")
	warmup := flag.Uint64("warmup", 30, "decisions delivered here before the measurement window starts")
	warmupTimeout := flag.Duration("warmup-timeout", 60*time.Second, "give up on warm-up after this")
	duration := flag.Duration("duration", 15*time.Second, "measurement window (SIGTERM/SIGUSR2 ends it early)")
	connectTimeout := flag.Duration("connect-timeout", 30*time.Second, "time to wait for all peers")
	result := flag.String("result", "", "path of the JSON result (default: stdout only)")
	heartbeat := flag.Duration("heartbeat-timeout", 0, "override LeaderHeartbeatTimeout (the leader resends in-flight pre-prepares every 1/10 of it); 0 keeps lib.Config's 60s")
	vcTimers := flag.Bool("vc-timers", false, "realistic view-change timers (RequestForwardTimeout 500ms, Complain 2s, Heartbeat 5s/10, ViewChange 20s) instead of the no-view-change ones")
	verbose := flag.Bool("verbose", false, "print view/sync Info lines of the library")
	debug := flag.Bool("debug", false, "print every Debug/Info line of the library (huge)")
	flag.Parse()

	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.SetPrefix(fmt.Sprintf("[node %d] ", *id))
	start := time.Now()

	addrs := map[uint64]string{}
	for _, p := range strings.Split(*peers, ",") {
		kv := strings.SplitN(strings.TrimSpace(p), "=", 2)
		if len(kv) != 2 {
			log.Fatalf("bad -peers entry %q", p)
		}
		pid, err := strconv.ParseUint(kv[0], 10, 64)
		if err != nil {
			log.Fatalf("bad -peers id %q: %v", kv[0], err)
		}
		addrs[pid] = kv[1]
	}
	if len(addrs) != *n {
		log.Fatalf("-peers has %d entries, -n is %d", len(addrs), *n)
	}
	self, ok := addrs[*id]
	if !ok {
		log.Fatalf("-peers has no entry for this node %d", *id)
	}
	if *listen == "" {
		*listen = self
	}
	others := map[uint64]string{}
	for pid, a := range addrs {
		if pid != *id {
			others[pid] = a
		}
	}

	runCfg := lib.RunConfig{N: *n, W: *w, OneWayDelay: time.Duration(*rtt) * time.Millisecond / 2,
		BatchMaxCount: *batch, BatchInterval: *interval, Concurrency: *k, WarmupDecisions: *warmup, Duration: *duration}
	cfg := lib.Config(runCfg)
	if *vcTimers {
		cfg.RequestForwardTimeout = 500 * time.Millisecond
		cfg.RequestComplainTimeout = 2 * time.Second
		cfg.RequestAutoRemoveTimeout = 120 * time.Second
		cfg.LeaderHeartbeatTimeout = 5 * time.Second
		cfg.LeaderHeartbeatCount = 10
		cfg.ViewChangeResendInterval = 5 * time.Second
		cfg.ViewChangeTimeout = 20 * time.Second
	}
	if *heartbeat > 0 {
		cfg.LeaderHeartbeatTimeout = *heartbeat
	}
	cfg.SelfID = *id
	res := lib.NodeResult{ID: *id, N: *n, W: *w, RTTms: *rtt, K: *k, Batch: *batch, StartUnixNano: start.UnixNano()}
	if err := cfg.Validate(); err != nil {
		res.Note = "configuration rejected: " + err.Error()
		finish(*result, &res, nil)
		os.Exit(2)
	}

	tr, err := lib.NewTransport(*id, *listen, others, runCfg.OneWayDelay)
	if err != nil {
		log.Fatalf("listen %s: %v", *listen, err)
	}
	ledger := lib.NewLedger()
	node := lib.NewNodeWithComm(*id, cfg, tr, ledger)
	node.App.Remote = tr
	node.Log.Verbose = *verbose
	node.Log.Debug = *debug
	rec := &recorder{start: start}
	node.App.OnDecision = rec.onDecision
	node.Log.OnViewStart = rec.onViewStart
	if *verbose {
		node.App.OnDecision = func(at time.Time, seq uint64, requests int) {
			rec.onDecision(at, seq, requests)
			log.Printf("deliver seq=%d reqs=%d", seq, requests)
		}
	}

	var client *lib.Client
	if *k > 0 {
		var targets []uint64
		if *submitTo == "" {
			targets = []uint64{*id}
		} else {
			for _, s := range strings.Split(*submitTo, ",") {
				t, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
				if err != nil {
					log.Fatalf("bad -submit-to %q: %v", s, err)
				}
				targets = append(targets, t)
			}
		}
		client = lib.NewClientFunc(node.App, *k, func(worker uint64, attempt int, req []byte) error {
			return tr.Submit(targets[(int(worker-1)+attempt)%len(targets)], req)
		})
		client.ResubmitAfter = *resubmit
	}

	log.Printf("GOMAXPROCS=%d W=%d RTT=%dms K=%d batch=%d/%s vc-timers=%v; waiting for %d peers", runtime.GOMAXPROCS(0), *w, *rtt, *k, *batch, *interval, *vcTimers, len(others))
	if err := tr.WaitConnected(*connectTimeout); err != nil {
		res.Note = err.Error()
		finish(*result, &res, nil)
		os.Exit(3)
	}
	if err := node.Start(); err != nil {
		res.Note = "start: " + err.Error()
		finish(*result, &res, nil)
		os.Exit(4)
	}
	tr.Start(node.Consensus, ledger) // peers' frames buffered in TCP so far are dispatched from here on
	if client != nil {
		client.Start()
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGUSR2)

	// Warm-up.
	deadline := time.Now().Add(*warmupTimeout)
	stopped := false
	for node.App.Decisions.Load() < *warmup {
		if time.Now().After(deadline) {
			res.Note = fmt.Sprintf("stall: only %d decisions after %s warm-up;", node.App.Decisions.Load(), *warmupTimeout)
			break
		}
		select {
		case <-sigs:
			stopped = true
		case <-time.After(10 * time.Millisecond):
		}
		if stopped {
			break
		}
	}
	ready := time.Now()
	res.ReadyUnixNano, res.MeasureStartUnixNano = ready.UnixNano(), ready.UnixNano()
	fmt.Printf("READY %d decisions=%d\n", ready.UnixNano(), node.App.Decisions.Load())

	// Measurement window.
	if !stopped {
		node.App.Measuring.Store(true)
		select {
		case <-time.After(*duration):
		case <-sigs:
			res.Note += " window ended by signal;"
		}
		node.App.Measuring.Store(false)
	}
	end := time.Now()
	res.MeasureEndUnixNano = end.UnixNano()
	elapsed := end.Sub(ready).Seconds() // monotonic: stops while the machine sleeps
	wall := float64(end.UnixNano()-ready.UnixNano()) / 1e9
	res.ClockJumpMs = (wall - elapsed) * 1e3
	if res.ClockJumpMs > 2000 {
		res.Note += fmt.Sprintf(" clock jump: wall %.1fs vs monotonic %.1fs (machine slept?);", wall, elapsed)
	}

	res.Leader = node.Consensus.GetLeaderID()
	res.Decisions, res.Requests, _ = node.App.MeasuredCounts()
	_, _, deliverMean := node.App.MeasuredCounts()
	res.DeliverMeanUs = float64(deliverMean.Nanoseconds()) / 1e3
	res.TotalDecisions, res.TotalRequests = node.App.Decisions.Load(), node.App.Requests.Load()
	if elapsed > 0 {
		res.TPS = float64(res.Requests) / elapsed
		res.DecisionsPerSec = float64(res.Decisions) / elapsed
	}
	mean, p50, p99, maxLat := lib.Stats(node.App.Latencies())
	res.LatMeanMs, res.LatP50Ms, res.LatP99Ms, res.LatMaxMs = msf(mean), msf(p50), msf(p99), msf(maxLat)
	res.MaxInFlight = node.InFlight.Max()
	res.ViewChanges, res.InFlightCommits = node.Log.ViewStarts(), node.Log.InFlightCommits()
	res.Syncs, res.Warnings, res.FirstWarning = node.App.Syncs.Load(), node.Log.Warnings(), node.Log.FirstWarning()
	if client != nil {
		res.SubmitFailed, res.Resubmits = client.Failed.Load(), client.Resubmits.Load()
	}
	res.Dropped, res.Reconnects, res.Submits, res.SyncServed = tr.Dropped.Load(), tr.Reconnects.Load(), tr.Submits.Load(), tr.SyncServed.Load()
	res.BytesIn, res.BytesOut = tr.BytesIn.Load(), tr.BytesOut.Load()
	rec.fill(&res, end)
	if res.Decisions == 0 && res.Note == "" {
		res.Note = "stall: no decisions during the measurement window;"
	}
	finish(*result, &res, func() {
		if client != nil {
			client.Stop()
		}
		node.Stop()
		tr.Close()
	})
}

func msf(d time.Duration) float64 { return float64(d.Nanoseconds()) / 1e6 }

// finish writes the result, prints DONE, then runs the shutdown with a watchdog so that a hanging
// Stop never keeps the orchestrator waiting.
func finish(path string, res *lib.NodeResult, shutdown func()) {
	b, err := json.MarshalIndent(res, "", " ")
	if err != nil {
		log.Fatal(err)
	}
	if path != "" {
		if err := os.WriteFile(path, b, 0o644); err != nil {
			log.Printf("write result: %v", err)
		}
	} else {
		os.Stdout.Write(b)
		fmt.Println()
	}
	fmt.Printf("DONE tps=%.0f p50=%.1fms p99=%.1fms vc=%d syncs=%d warn=%d note=%q\n",
		res.TPS, res.LatP50Ms, res.LatP99Ms, res.ViewChanges, res.Syncs, res.Warnings, res.Note)
	if shutdown == nil {
		return
	}
	done := make(chan struct{})
	go func() { shutdown(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		log.Printf("shutdown timed out, exiting")
	}
}
