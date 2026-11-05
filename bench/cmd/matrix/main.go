// Command matrix orchestrates the process-separated benchmark: for every W x RTT cell it launches
// N cmd/node processes (GOMAXPROCS pinned), waits for warm-up, lets them measure, collects their
// JSON results and appends a CSV row; optionally it runs the leader-crash cells (SIGKILL of node 1
// after warm-up, with realistic view-change timers and the client on node 2 submitting to all nodes).
package main

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/s-xoxo/SmartBFT_Pipeline/bench/lib"
)

type opts struct {
	n, k, gomaxprocs      int
	batch, warmup         uint64
	interval              time.Duration
	duration              time.Duration
	nodeBin, logDir, host string
	basePort              int
	verbose               bool
	heartbeat             time.Duration
}

type cell struct {
	name       string
	w, rtt     uint64
	crash      bool
	duration   time.Duration
	crashAt    time.Duration // after all nodes are READY
	clientNode int           // crash cells: the node that runs the client
	k          int           // crash cells: client workers (0 = o.k)
}

type cellOutcome struct {
	results map[uint64]*lib.NodeResult // by node id; killed node absent
	t0      time.Time                  // crash time (crash cells)
	err     error
}

func main() {
	ws := flag.String("w", "1,2,4,8", "comma-separated PipelineDepthMax values")
	rtts := flag.String("rtt", "0,50,100,200", "comma-separated RTTs in ms")
	mode := flag.String("mode", "all", "matrix | crash | all")
	crashW := flag.String("crash-w", "4,1", "W values of the leader-crash cells")
	crashRTT := flag.Uint64("crash-rtt", 50, "RTT of the leader-crash cells (ms)")
	crashAt := flag.Duration("crash-after", 5*time.Second, "SIGKILL the leader this long after all nodes are READY")
	crashDuration := flag.Duration("crash-duration", 40*time.Second, "node measurement window in the crash cells")
	crashK := flag.Int("crash-k", 256, "client workers in the crash cells (K=1024 makes the W=1 request latency exceed RequestComplainTimeout)")
	crashClient := flag.Int("crash-client-node", 4, "node that hosts the client in the crash cells (not 1, and preferably not the next leader, 2)")
	out := flag.String("out", "../results/2026-11-05-second-bench", "directory for matrix.csv / crash.csv")
	o := opts{}
	flag.IntVar(&o.n, "n", 4, "number of nodes")
	flag.IntVar(&o.k, "k", 1024, "closed-loop client workers")
	flag.Uint64Var(&o.batch, "batch", 50, "RequestBatchMaxCount")
	flag.DurationVar(&o.interval, "batch-interval", 50*time.Millisecond, "RequestBatchMaxInterval")
	flag.Uint64Var(&o.warmup, "warmup", 30, "decisions before measuring")
	flag.DurationVar(&o.duration, "duration", 15*time.Second, "measurement window per throughput cell")
	flag.IntVar(&o.gomaxprocs, "gomaxprocs", 2, "GOMAXPROCS of each node process (capped by the core count / N)")
	flag.StringVar(&o.nodeBin, "node-bin", "", "prebuilt cmd/node binary (default: go build into -logdir)")
	flag.StringVar(&o.logDir, "logdir", os.TempDir(), "directory for per-node logs and JSON results")
	flag.StringVar(&o.host, "host", "127.0.0.1", "address the nodes listen on")
	flag.IntVar(&o.basePort, "base-port", 23000, "first port; cell i uses base+10*i+id")
	flag.BoolVar(&o.verbose, "verbose", false, "pass -verbose to the nodes")
	flag.DurationVar(&o.heartbeat, "heartbeat-timeout", 0, "pass -heartbeat-timeout to the nodes of the throughput cells (0 = lib.Config default 60s)")
	flag.Parse()

	cores := runtime.NumCPU()
	if o.gomaxprocs*o.n > cores {
		o.gomaxprocs = max(1, cores/o.n)
	}
	fmt.Fprintf(os.Stderr, "matrix: %s %s/%s, %d cores, GOMAXPROCS=%d per node, N=%d K=%d batch=%d/%s warmup=%d duration=%s\n",
		runtime.Version(), runtime.GOOS, runtime.GOARCH, cores, o.gomaxprocs, o.n, o.k, o.batch, o.interval, o.warmup, o.duration)
	if err := os.MkdirAll(o.logDir, 0o755); err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		log.Fatal(err)
	}
	if o.nodeBin == "" {
		o.nodeBin = filepath.Join(o.logDir, "node")
		b := exec.Command("go", "build", "-o", o.nodeBin, "./cmd/node")
		b.Stderr = os.Stderr
		if err := b.Run(); err != nil {
			log.Fatalf("build cmd/node: %v", err)
		}
	}

	var cells []cell
	if *mode == "matrix" || *mode == "all" {
		for _, rtt := range parseList(*rtts) {
			for _, w := range parseList(*ws) {
				cells = append(cells, cell{name: fmt.Sprintf("w%d-rtt%d", w, rtt), w: w, rtt: rtt, duration: o.duration})
			}
		}
	}
	if *mode == "crash" || *mode == "all" {
		for _, w := range parseList(*crashW) {
			cells = append(cells, cell{name: fmt.Sprintf("crash-w%d-rtt%d", w, *crashRTT), w: w, rtt: *crashRTT, crash: true,
				duration: *crashDuration, crashAt: *crashAt, clientNode: *crashClient, k: *crashK})
		}
	}

	var matrixW, crashW2 *csv.Writer
	var matrixF, crashF *os.File
	defer func() {
		if matrixF != nil {
			matrixF.Close()
		}
		if crashF != nil {
			crashF.Close()
		}
	}()
	tps := map[[2]uint64]string{}
	p50 := map[[2]uint64]string{}
	p99 := map[[2]uint64]string{}
	for i, c := range cells {
		fmt.Fprintf(os.Stderr, "cell %s ... ", c.name)
		oc := runCell(o, c, i)
		if oc.err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: %v\n", oc.err)
		}
		if !c.crash {
			if matrixW == nil {
				matrixF, matrixW = openCSV(filepath.Join(*out, "matrix.csv"),
					"w,rtt_ms,k,batch,n,gomaxprocs,duration_s,decisions,requests,tps,decisions_per_s,lat_p50_ms,lat_p99_ms,lat_mean_ms,lat_max_ms,deliver_mean_us,max_in_flight,view_changes,syncs,warnings,dropped,follower_lag,clock_jump_ms,note")
			}
			row, t, l50, l99 := matrixRow(o, c, oc)
			_ = matrixW.Write(row)
			matrixW.Flush()
			tps[[2]uint64{c.w, c.rtt}], p50[[2]uint64{c.w, c.rtt}], p99[[2]uint64{c.w, c.rtt}] = t, l50, l99
			fmt.Fprintf(os.Stderr, "%s\n", strings.Join(row[9:], " | "))
			continue
		}
		if crashW2 == nil {
			crashF, crashW2 = openCSV(filepath.Join(*out, "crash.csv"),
				"w,rtt_ms,k,node,leader_at_end,t0_unix_nano,tail_ms,first_view_start_ms,recovery_ms,gaps,view_changes,in_flight_commits,pre_crash_tps,post_recovery_tps_15s,post_15s_complete,syncs,warnings,resubmits,dropped,clock_jump_ms,note")
		}
		for _, row := range crashRows(o, c, oc) {
			_ = crashW2.Write(row)
			fmt.Fprintf(os.Stderr, "\n   node %s: tail=%sms vc-start=%sms recovery=%sms vc=%s inflight=%s pre=%s post15=%s note=%s", row[3], row[6], row[7], row[8], row[10], row[11], row[12], row[13], row[19])
		}
		crashW2.Flush()
		fmt.Fprintln(os.Stderr)
	}

	if len(tps) > 0 {
		wList, rttList := parseList(*ws), parseList(*rtts)
		fmt.Printf("\n### TPS (K=%d)\n\n", o.k)
		printTable(wList, rttList, tps)
		fmt.Printf("\n### p50 latency (ms)\n\n")
		printTable(wList, rttList, p50)
		fmt.Printf("\n### p99 latency (ms)\n\n")
		printTable(wList, rttList, p99)
	}
}

func openCSV(path, header string) (*os.File, *csv.Writer) {
	f, err := os.Create(path)
	if err != nil {
		log.Fatal(err)
	}
	w := csv.NewWriter(f)
	_ = w.Write(strings.Split(header, ","))
	w.Flush()
	return f, w
}

func printTable(wList, rttList []uint64, cells map[[2]uint64]string) {
	fmt.Printf("| W \\ RTT |")
	for _, rtt := range rttList {
		fmt.Printf(" %d ms |", rtt)
	}
	fmt.Printf("\n|---|")
	for range rttList {
		fmt.Printf("---|")
	}
	for _, w := range wList {
		fmt.Printf("\n| %d |", w)
		for _, rtt := range rttList {
			fmt.Printf(" %s |", cells[[2]uint64{w, rtt}])
		}
	}
	fmt.Println()
}

type proc struct {
	id   uint64
	cmd  *exec.Cmd
	out  *os.File
	done chan struct{} // closed when the process has exited
}

// runCell launches the N node processes of one cell and collects their results.
func runCell(o opts, c cell, idx int) cellOutcome {
	oc := cellOutcome{results: map[uint64]*lib.NodeResult{}}
	base := o.basePort + 10*(idx%600)
	var peers []string
	for id := 1; id <= o.n; id++ {
		peers = append(peers, fmt.Sprintf("%d=%s:%d", id, o.host, base+id))
	}
	peerList := strings.Join(peers, ",")

	ready := make(chan uint64, o.n)
	procs := make([]*proc, 0, o.n)
	var wg sync.WaitGroup
	for id := 1; id <= o.n; id++ {
		resultPath := filepath.Join(o.logDir, fmt.Sprintf("%s-node%d.json", c.name, id))
		_ = os.Remove(resultPath)
		args := []string{"-id", strconv.Itoa(id), "-n", strconv.Itoa(o.n), "-peers", peerList,
			"-w", u(c.w), "-rtt", u(c.rtt), "-batch", u(o.batch), "-batch-interval", o.interval.String(),
			"-warmup", u(o.warmup), "-duration", c.duration.String(), "-result", resultPath}
		if o.verbose {
			args = append(args, "-verbose")
		}
		if o.heartbeat > 0 && !c.crash {
			args = append(args, "-heartbeat-timeout", o.heartbeat.String())
		}
		if c.crash {
			// Realistic view-change timers; the client lives on a follower (so it survives the crash
			// of node 1) and submits round-robin to all nodes, failing over after 3 s without delivery.
			args = append(args, "-vc-timers")
			if id == c.clientNode {
				var targets []string
				for t := 1; t <= o.n; t++ {
					targets = append(targets, strconv.Itoa(t))
				}
				k := c.k
				if k == 0 {
					k = o.k
				}
				args = append(args, "-k", strconv.Itoa(k), "-submit-to", strings.Join(targets, ","), "-resubmit", "3s")
			}
		} else if id == 1 {
			args = append(args, "-k", strconv.Itoa(o.k))
		}
		cmd := exec.Command(o.nodeBin, args...)
		cmd.Env = append(os.Environ(), "GOMAXPROCS="+strconv.Itoa(o.gomaxprocs))
		logf, err := os.Create(filepath.Join(o.logDir, fmt.Sprintf("%s-node%d.log", c.name, id)))
		if err != nil {
			oc.err = err
			return oc
		}
		cmd.Stderr = logf
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			oc.err = err
			return oc
		}
		if err := cmd.Start(); err != nil {
			oc.err = fmt.Errorf("start node %d: %w", id, err)
			logf.Close()
			break
		}
		p := &proc{id: uint64(id), cmd: cmd, out: logf, done: make(chan struct{})}
		procs = append(procs, p)
		wg.Add(1)
		go func(p *proc, r io.Reader) {
			defer wg.Done()
			sc := bufio.NewScanner(r)
			sc.Buffer(make([]byte, 1<<20), 1<<20)
			for sc.Scan() {
				line := sc.Text()
				fmt.Fprintln(p.out, line)
				if strings.HasPrefix(line, "READY") {
					ready <- p.id
				}
			}
		}(p, stdout)
		go func(p *proc) { _ = p.cmd.Wait(); close(p.done) }(p)
	}
	killAll := func() {
		for _, p := range procs {
			_ = p.cmd.Process.Kill()
		}
	}
	defer func() {
		killAll()
		wg.Wait()
		for _, p := range procs {
			<-p.done
			p.out.Close()
		}
	}()
	if oc.err != nil {
		return oc
	}

	// Wait for every node to finish warm-up (or exit early).
	readyCount := 0
	timeout := time.After(90*time.Second + c.duration)
	exited := make(chan uint64, o.n)
	for _, p := range procs {
		go func(p *proc) { <-p.done; exited <- p.id }(p)
	}
	for readyCount < len(procs) {
		select {
		case <-ready:
			readyCount++
		case id := <-exited:
			oc.err = fmt.Errorf("node %d exited before READY", id)
			readyCount = len(procs) // fall through and collect whatever was written
		case <-timeout:
			oc.err = fmt.Errorf("nodes not READY in time")
			return oc
		}
	}

	if c.crash && oc.err == nil {
		time.Sleep(c.crashAt)
		oc.t0 = time.Now()
		if err := procs[0].cmd.Process.Signal(syscall.SIGKILL); err != nil {
			oc.err = fmt.Errorf("kill leader: %w", err)
		}
	}

	// Wait for the nodes to finish their window and write results.
	deadline := time.After(c.duration + 60*time.Second)
	for _, p := range procs {
		select {
		case <-p.done:
		case <-deadline:
			oc.err = fmt.Errorf("node %d did not exit in time", p.id)
			_ = p.cmd.Process.Kill()
		}
	}
	for _, p := range procs {
		b, err := os.ReadFile(filepath.Join(o.logDir, fmt.Sprintf("%s-node%d.json", c.name, p.id)))
		if err != nil {
			if !(c.crash && p.id == 1) {
				oc.err = fmt.Errorf("node %d wrote no result", p.id)
			}
			continue
		}
		r := &lib.NodeResult{}
		if err := json.Unmarshal(b, r); err != nil {
			oc.err = fmt.Errorf("node %d result: %w", p.id, err)
			continue
		}
		oc.results[p.id] = r
	}
	return oc
}

func matrixRow(o opts, c cell, oc cellOutcome) (row []string, tps, p50, p99 string) {
	leader := oc.results[1]
	var note []string
	if oc.err != nil {
		note = append(note, oc.err.Error())
	}
	var vc, syncs, warnings, dropped uint64
	minFollower := uint64(1<<63 - 1)
	first := ""
	for id := 1; id <= o.n; id++ {
		r := oc.results[uint64(id)]
		if r == nil {
			minFollower = 0
			continue
		}
		vc += r.ViewChanges
		syncs += r.Syncs
		warnings += r.Warnings
		dropped += r.Dropped
		if id > 1 {
			minFollower = min(minFollower, r.TotalDecisions)
		}
		if r.Note != "" {
			note = append(note, fmt.Sprintf("node%d: %s", id, strings.TrimSpace(r.Note)))
		}
		if first == "" && r.FirstWarning != "" {
			first = fmt.Sprintf("node%d: %s", id, r.FirstWarning)
		}
	}
	if vc > 0 {
		note = append(note, fmt.Sprintf("view changes: %d", vc))
	}
	if syncs > 0 {
		note = append(note, fmt.Sprintf("syncs: %d", syncs))
	}
	if first != "" {
		note = append(note, fmt.Sprintf("first warning: %q", first))
	}
	if leader == nil {
		row = []string{u(c.w), u(c.rtt), strconv.Itoa(o.k), u(o.batch), strconv.Itoa(o.n), strconv.Itoa(o.gomaxprocs), fs(c.duration.Seconds()),
			"", "", "", "", "", "", "", "", "", "", u(vc), u(syncs), u(warnings), u(dropped), "", "", strings.Join(note, "; ")}
		return row, "error", "error", "error"
	}
	lag := int64(leader.TotalDecisions) - int64(minFollower)
	row = []string{u(c.w), u(c.rtt), strconv.Itoa(o.k), u(o.batch), strconv.Itoa(o.n), strconv.Itoa(o.gomaxprocs),
		fs(float64(leader.MeasureEndUnixNano-leader.MeasureStartUnixNano)/1e9 - leader.ClockJumpMs/1e3),
		u(leader.Decisions), u(leader.Requests), fs(leader.TPS), fs(leader.DecisionsPerSec),
		fs(leader.LatP50Ms), fs(leader.LatP99Ms), fs(leader.LatMeanMs), fs(leader.LatMaxMs), fs(leader.DeliverMeanUs), fs(leader.MaxInFlight),
		u(vc), u(syncs), u(warnings), u(dropped), strconv.FormatInt(lag, 10), fs(leader.ClockJumpMs), strings.Join(note, "; ")}
	mark := ""
	if vc > 0 || leader.Decisions == 0 || leader.ClockJumpMs > 2000 {
		mark = " (!)"
	}
	return row, fmt.Sprintf("%.0f%s", leader.TPS, mark), fmt.Sprintf("%.1f", leader.LatP50Ms), fmt.Sprintf("%.1f", leader.LatP99Ms)
}

func crashRows(o opts, c cell, oc cellOutcome) [][]string {
	var rows [][]string
	ids := make([]int, 0, len(oc.results))
	for id := range oc.results {
		ids = append(ids, int(id))
	}
	sort.Ints(ids)
	k := c.k
	if k == 0 {
		k = o.k
	}
	t0 := oc.t0.UnixNano()
	for _, id := range ids {
		r := oc.results[uint64(id)]
		var note []string
		if oc.err != nil {
			note = append(note, oc.err.Error())
		}
		if r.Note != "" {
			note = append(note, strings.TrimSpace(r.Note))
		}
		// Requests delivered from the start of the window up to T0, per second.
		pre := ""
		startSec := int((r.MeasureStartUnixNano - r.StartUnixNano) / 1e9)
		t0Sec := int((t0 - r.StartUnixNano) / 1e9)
		var before uint64
		for s := startSec; s < t0Sec && s < len(r.PerSecond); s++ {
			before += r.PerSecond[s]
		}
		if secs := float64(t0Sec - startSec); secs > 0 {
			pre = fs(float64(before) / secs)
		}
		// The pipeline tail: first delivery after T0 at all (survivors finish what was already prepared).
		tail := ""
		for _, g := range r.Gaps {
			if g.BeforeUnixNano <= t0 && g.AfterUnixNano > t0 {
				tail = fs(float64(g.AfterUnixNano-t0) / 1e6)
				break
			}
		}
		firstVC, recovery, post, complete := "", "", "", ""
		if len(r.ViewStartUnixNanos) > 0 {
			firstVC = fs(float64(r.ViewStartUnixNanos[0]-t0) / 1e6)
		}
		if r.FirstDeliveryAfterViewChangeUnixNano > 0 {
			recovery = fs(float64(r.FirstDeliveryAfterViewChangeUnixNano-t0) / 1e6)
			post = fs(float64(r.PostViewChange15sRequests) / 15)
			complete = strconv.FormatBool(r.PostViewChange15sComplete)
		} else {
			note = append(note, "no delivery after a view change")
		}
		if r.ViewChanges != 1 {
			note = append(note, fmt.Sprintf("expected 1 view change, saw %d", r.ViewChanges))
		}
		rows = append(rows, []string{u(c.w), u(c.rtt), strconv.Itoa(k), strconv.Itoa(id), u(r.Leader), strconv.FormatInt(t0, 10),
			tail, firstVC, recovery, strconv.Itoa(len(r.Gaps)), u(r.ViewChanges), u(r.InFlightCommits), pre, post, complete,
			u(r.Syncs), u(r.Warnings), u(r.Resubmits), u(r.Dropped), fs(r.ClockJumpMs), strings.Join(note, "; ")})
	}
	if len(rows) == 0 {
		msg := "no results"
		if oc.err != nil {
			msg = oc.err.Error()
		}
		rows = append(rows, []string{u(c.w), u(c.rtt), strconv.Itoa(k), "", "", "", "", "", "", "", "", "", "", "", "", "", "", "", "", "", msg})
	}
	return rows
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

func u(v uint64) string   { return strconv.FormatUint(v, 10) }
func fs(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }
