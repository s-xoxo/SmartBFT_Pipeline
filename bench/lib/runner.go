package lib

import (
	"fmt"
	"slices"
	"time"

	"github.com/hyperledger/SmartBFT/pkg/types"
)

// RunConfig describes one benchmark cell.
type RunConfig struct {
	N               int           // number of nodes
	W               uint64        // PipelineDepthMax; 0/1 is the original sequential protocol
	OneWayDelay     time.Duration // per-link delay, RTT/2
	BatchMaxCount   uint64
	BatchInterval   time.Duration
	Concurrency     int           // closed-loop workers K
	WarmupDecisions uint64        // decisions on the leader before measuring starts
	Duration        time.Duration // measurement window
	WarmupTimeout   time.Duration // give up on a cell whose warm-up does not finish in time (default 60s)
}

// Result is what one cell measured on the leader (node 1).
type Result struct {
	Cfg                       RunConfig
	Decisions, Requests       uint64 // delivered on the leader during the window
	TPS, DecisionsPerSec      float64
	MeanLat, P50, P99, MaxLat time.Duration
	DeliverMean               time.Duration // time spent inside Application.Deliver per decision
	MaxInFlight               float64       // peak of the leader's view_in_flight_proposals gauge
	ViewChanges               uint64        // view starts with number > 0, summed over nodes
	Syncs, Warnings, Dropped  uint64        // over all nodes / links
	FollowerLag               int64         // leader decisions minus slowest follower's at the end
	Note                      string        // anything that went wrong
}

// Config builds the node configuration of a cell: DefaultConfig with generous timeouts so no
// view change is triggered by the benchmark itself, no leader rotation, and the pipeline depth W.
func Config(cfg RunConfig) types.Configuration {
	c := types.DefaultConfig
	c.RequestBatchMaxCount = cfg.BatchMaxCount
	c.RequestBatchMaxBytes = 10 * 1024 * 1024
	c.RequestBatchMaxInterval = cfg.BatchInterval
	c.IncomingMessageBufferSize = 1000
	c.RequestPoolSize = 20000
	c.RequestForwardTimeout = 30 * time.Second
	c.RequestComplainTimeout = 60 * time.Second
	c.RequestAutoRemoveTimeout = 120 * time.Second
	c.ViewChangeResendInterval = 5 * time.Second
	c.ViewChangeTimeout = 60 * time.Second
	c.LeaderHeartbeatTimeout = 60 * time.Second
	c.LeaderHeartbeatCount = 10
	c.NumOfTicksBehindBeforeSyncing = 10
	c.CollectTimeout = time.Second
	c.SyncOnStart = false
	c.LeaderRotation = false
	c.DecisionsPerLeader = 0
	c.RequestMaxBytes = 10 * 1024
	c.RequestPoolSubmitTimeout = 5 * time.Second
	c.PipelineDepthMax = cfg.W
	c.AdaptiveBatch = false
	return c
}

// Run executes one cell and returns its measurements. A configuration the library rejects is
// returned as an error; anomalies during the run (view changes, stalls) are reported in Result.Note.
func Run(cfg RunConfig) (Result, error) {
	res := Result{Cfg: cfg}
	if cfg.WarmupTimeout == 0 {
		cfg.WarmupTimeout = 60 * time.Second
	}
	nodeCfg := Config(cfg)
	nodeCfg.SelfID = 1
	if err := nodeCfg.Validate(); err != nil {
		return res, fmt.Errorf("configuration rejected: %w", err)
	}

	ids := make([]uint64, cfg.N)
	for i := range ids {
		ids[i] = uint64(i + 1)
	}
	net := NewNetwork(ids, cfg.OneWayDelay)
	ledger := NewLedger()
	nodes := make([]*Node, cfg.N)
	for i, id := range ids {
		nodes[i] = NewNode(id, nodeCfg, net, ledger)
	}
	leader := nodes[0]
	client := NewClient(leader, cfg.Concurrency)
	net.Start()
	for _, n := range nodes {
		if err := n.Start(); err != nil {
			return res, fmt.Errorf("node %d start: %w", n.ID, err)
		}
	}
	client.Start()

	// Warm-up: wait for the leader to deliver WarmupDecisions decisions.
	deadline := time.Now().Add(cfg.WarmupTimeout)
	for leader.App.Decisions.Load() < cfg.WarmupDecisions {
		if time.Now().After(deadline) {
			res.Note = fmt.Sprintf("stall: only %d decisions after %s warm-up", leader.App.Decisions.Load(), cfg.WarmupTimeout)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if res.Note == "" {
		for _, n := range nodes {
			n.App.Measuring.Store(true)
		}
		start := time.Now()
		time.Sleep(cfg.Duration)
		for _, n := range nodes {
			n.App.Measuring.Store(false)
		}
		elapsed := time.Since(start)
		minFollower := uint64(1<<63 - 1)
		for _, n := range nodes[1:] {
			minFollower = min(minFollower, n.App.Decisions.Load())
		}
		res.FollowerLag = int64(leader.App.Decisions.Load()) - int64(minFollower)
		res.Decisions = leader.App.mDecisions.Load()
		res.Requests = leader.App.mRequests.Load()
		res.TPS = float64(res.Requests) / elapsed.Seconds()
		res.DecisionsPerSec = float64(res.Decisions) / elapsed.Seconds()
		if res.Decisions > 0 {
			res.DeliverMean = time.Duration(leader.App.mDeliverNanos.Load() / int64(res.Decisions))
		}
		res.MeanLat, res.P50, res.P99, res.MaxLat = stats(leader.App.Latencies())
		if res.Decisions == 0 {
			res.Note = "stall: no decisions during the measurement window"
		}
	}

	client.Stop()
	for _, n := range nodes {
		n.Stop()
	}
	net.Stop()

	res.MaxInFlight = leader.InFlight.Max()
	res.Dropped = net.Dropped()
	for _, n := range nodes {
		res.ViewChanges += n.Log.viewStarts.Load()
		res.Syncs += n.App.Syncs.Load()
		res.Warnings += n.Log.warnings.Load()
	}
	if client.Failed.Load() > 0 {
		res.Note += fmt.Sprintf(" submit errors: %d;", client.Failed.Load())
	}
	if res.ViewChanges > 0 {
		res.Note += fmt.Sprintf(" view changes: %d;", res.ViewChanges)
	}
	if res.Warnings > 0 {
		res.Note += fmt.Sprintf(" warnings: %d (first: %q);", res.Warnings, firstWarning(nodes))
	}
	return res, nil
}

func firstWarning(nodes []*Node) string {
	for _, n := range nodes {
		n.Log.mu.Lock()
		f := n.Log.first
		n.Log.mu.Unlock()
		if len(f) > 0 {
			return f[0]
		}
	}
	return ""
}

func stats(l []time.Duration) (mean, p50, p99, maxLat time.Duration) {
	if len(l) == 0 {
		return
	}
	slices.Sort(l)
	var sum time.Duration
	for _, d := range l {
		sum += d
	}
	pct := func(p float64) time.Duration { return l[min(len(l)-1, int(float64(len(l))*p))] }
	return sum / time.Duration(len(l)), pct(0.50), pct(0.99), l[len(l)-1]
}
