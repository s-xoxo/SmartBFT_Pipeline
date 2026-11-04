### Summary

After a restart, `PersistedState.Restore` can bring back a pre-prepare the application has already delivered. The restored view then holds a stale proposal, re-broadcasts its prepare, and rejects every prepare of the next sequence with `Got wrong digest at processPrepares`. The node misses the next sequence (and the one after) by consensus and only catches up through a later `Synchronizer.Sync`. Reproduced on `main` (f55a7d12) and on 32a628d6, with `SyncOnStart` both on and off; three runs, identical results.

### Where

- `internal/bft/state.go`, `Restore` → `recoverProposed`: when the last WAL record is a `proposed_record`, the proposal is restored as in flight **without** comparing its sequence with the application's last delivered sequence. The commit case (`recoverPrepared`) does compare (`has been safely committed` / `WAL out of sync`), and so does the view-record path added in #748 (`restoreInFlightProposal`); `recoverProposed` is the one path that does not.
- `internal/bft/util.go`, `ProposalMaker.NewProposer`: when the controller's sequence is higher than the restored one it only raises `ProposalSequence` and `DecisionsInView`; `Phase` stays `PROPOSED` and `inFlightProposal` keeps the stale proposal.

### How the WAL gets into that state

A `proposed_record` is written when the node processes the pre-prepare (truncating the WAL), the commit record only once it has a prepare quorum. If the node then loses the prepares and commits of that sequence and later learns of the decisions through `Synchronizer.Sync` (for example after a leader heartbeat reports a higher sequence: `Heartbeat sequence is bigger than expected`), its checkpoint moves past the sequence while the WAL still ends with the pre-prepare. `sync()` writes a `new_view` record only when `fetchState` returns a newer view, so in the common case nothing is appended. A restart at that point restores the stale proposal.

### Steps (4 nodes, node 1 leads view 0, sequential protocol; the attached test does this)

1. Node 4 drops `Prepare` and `Commit` messages (pre-prepares and heartbeats pass).
2. Request 1: nodes 1–3 deliver sequence 1; node 4 logs `Processed proposal with seq 1`, so its WAL is `[PP(1)]` and it waits in `PROPOSED`.
3. Request 2: nodes 1–3 deliver sequence 2. Node 4 sees the leader heartbeat (`Heartbeat sequence is bigger than expected, leader's sequence is 3 and ours is 1`), syncs (`Synchronizer returned with sequence 2 while the controller is at sequence 0`) and restarts its view at sequence 3. Its WAL is still `[PP(1)]`.
4. Restart node 4 (with or without `SyncOnStart`; with it, `Sync` returns sequence 2 == the controller's sequence, so nothing changes before `startView(3)` → `NewProposer` → `Restore`).
5. Requests 3, 4, 5.

### Observed on node 4 after the restart

```
Restored proposal with sequence 1
Starting view with number 0, sequence 3, and decisions 2
(nodes 1-3) got message from 4 with sequence 1 but our sequence is 3        <- stale prepare(1) re-broadcast
Got wrong digest at processPrepares for prepare with seq 3, expecting <digest of proposal 1> but got <digest of proposal 3>, we are in seq 3   (x3, one per peer)
Heartbeat sequence is bigger than expected, leader's sequence is 5 and ours is 3, syncing and ignoring
Synchronizer returned with sequence 4 while the controller is at sequence 2
Processed proposal with seq 5                                               <- normal participation resumes
```

The pre-prepare of sequence 3 is received but never processed (the view is in `processPrepares` for the stale proposal); the commits of sequence 3 are registered and ignored. Node 4 delivers sequences 3 and 4 only through the sync (`Sync deliver` x2).

### Scope

No safety violation: the node never delivers a wrong value, the other nodes make progress, and the node recovers through sync. It is a liveness delay for the restarted node: it misses the next sequences until the leader's heartbeat gets far enough ahead to trigger a sync (or `NumOfTicksBehindBeforeSyncing` ticks pass), and it logs misleading `wrong digest` warnings meanwhile. With one affected node (f+1 not reached) there is no view change. If f+1 nodes restarted in this state at the same time, they would all fail to prepare the next sequence, which could lead to an unnecessary view change.

### Note on an existing test

`TestRestartKeepsViewAndInFlightProposal` (test/wal_truncation_test.go, added in #739 as a regression test for WAL file growth) restarts the isolated node with `Restart()`, which syncs first: the sync delivers sequence 1, so the proposal the test then expects to be "restored in flight", and whose prepare it expects to be re-broadcast, has already been delivered. The test pins this behaviour incidentally; the sync being on does not look intentional given the test's own description ("crashed before collecting a quorum"). A fix would need that restart to use `RestartSync(false)`; the test's other assertions (same view, same WAL record, flat file count over ten restarts) are unaffected.

### One possible fix

Drop the record in `recoverProposed` when its sequence is at or below the application's last delivered sequence (the same check `recoverPrepared` and `restoreInFlightProposal` make), and/or have `NewProposer` reset the phase and the in-flight fields when it raises the sequence past the restored one.

### Reproduction test

Self-contained integration test for the `test/` harness (4 nodes, ~15 s). It loses prepares/commits on one node, waits for the heartbeat-driven sync, restarts the node and records the log lines above; it only fails on harness timeouts and prints a `SUMMARY` line per subtest (`syncOnRestart=false` / `true`). Drop it into `test/` and run:

```
go test ./test/ -run TestStaleProposedRecordRestore -count=1 -v
```

<details>
<summary>test/stale_restore_repro_test.go</summary>

```go
// Copyright IBM Corp. All Rights Reserved.
//
// SPDX-License-Identifier: Apache-2.0
//

package test

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hyperledger/SmartBFT/smartbftprotos"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"google.golang.org/protobuf/proto"
)

// logRecorder captures every log line a node emits, so the test can wait on
// protocol events and report counts of interesting lines afterwards.
type logRecorder struct {
	lock  sync.Mutex
	lines []string
}

func (r *logRecorder) hook(entry zapcore.Entry) error {
	r.lock.Lock()
	r.lines = append(r.lines, entry.Message)
	r.lock.Unlock()
	return nil
}

// mark returns the current number of recorded lines, used to split the log into phases.
func (r *logRecorder) mark() int {
	r.lock.Lock()
	defer r.lock.Unlock()
	return len(r.lines)
}

// count returns how many lines recorded from index `from` contain every substring in subs.
func (r *logRecorder) count(from int, subs ...string) int {
	r.lock.Lock()
	defer r.lock.Unlock()
	n := 0
	for _, l := range r.lines[from:] {
		if containsAll(l, subs...) {
			n++
		}
	}
	return n
}

// matching returns the lines recorded from index `from` that contain every substring in subs.
func (r *logRecorder) matching(from int, subs ...string) []string {
	r.lock.Lock()
	defer r.lock.Unlock()
	var res []string
	for _, l := range r.lines[from:] {
		if containsAll(l, subs...) {
			res = append(res, l)
		}
	}
	return res
}

// waitFor blocks until at least one line recorded from index `from` contains every substring in subs,
// or until the timeout expires. It returns whether the line was seen.
func (r *logRecorder) waitFor(timeout time.Duration, from int, subs ...string) bool {
	deadline := time.Now().Add(timeout)
	for {
		if r.count(from, subs...) > 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

// describeWAL renders the raw WAL entries passed to the consensus at start-up.
func describeWAL(entries [][]byte) []string {
	var res []string
	for i, raw := range entries {
		sm := &smartbftprotos.SavedMessage{}
		if err := proto.Unmarshal(raw, sm); err != nil {
			res = append(res, fmt.Sprintf("[%d] unmarshal error: %v", i, err))
			continue
		}
		switch {
		case sm.GetProposedRecord() != nil:
			pp := sm.GetProposedRecord().GetPrePrepare()
			res = append(res, fmt.Sprintf("[%d] proposed_record view=%d seq=%d", i, pp.GetView(), pp.GetSeq()))
		case sm.GetCommit() != nil:
			c := sm.GetCommit().GetCommit()
			res = append(res, fmt.Sprintf("[%d] commit view=%d seq=%d", i, c.GetView(), c.GetSeq()))
		case sm.GetNewView() != nil:
			nv := sm.GetNewView()
			res = append(res, fmt.Sprintf("[%d] new_view view=%d latestSeq=%d", i, nv.GetViewId(), nv.GetLatestSequence()))
		case sm.GetViewChange() != nil:
			res = append(res, fmt.Sprintf("[%d] view_change nextView=%d", i, sm.GetViewChange().GetNextView()))
		default:
			res = append(res, fmt.Sprintf("[%d] unknown record", i))
		}
	}
	return res
}

func seqOf(t *testing.T, rec *AppRecord) uint64 {
	md := &smartbftprotos.ViewMetadata{}
	require.NoError(t, proto.Unmarshal(rec.Metadata, md))
	return md.GetLatestSequence()
}

// awaitDelivery reads one decision from the node and requires its sequence to be seq.
func awaitDelivery(t *testing.T, n *App, seq uint64, timeout time.Duration) {
	select {
	case rec := <-n.Delivered:
		require.Equal(t, seq, seqOf(t, rec), "node %d delivered an unexpected sequence", n.ID)
	case <-time.After(timeout):
		t.Fatalf("harness timeout: node %d did not deliver sequence %d within %v", n.ID, seq, timeout)
	}
}

// tryDelivery reads one decision from the node, returning its sequence and whether anything arrived.
func tryDelivery(t *testing.T, n *App, timeout time.Duration) (uint64, bool) {
	select {
	case rec := <-n.Delivered:
		return seqOf(t, rec), true
	case <-time.After(timeout):
		return 0, false
	}
}

// TestStaleProposedRecordRestore reproduces a suspected latent bug in the WAL restore path:
// a node whose WAL ends with a proposed_record for sequence n, and whose checkpoint has meanwhile
// advanced past n through Synchronizer.Sync (which writes no new_view record), restarts into a view
// whose phase is PROPOSED and whose in-flight proposal is the stale proposal n, while NewProposer
// bumps only the proposal sequence. The test records what the node then does; it fails only on
// harness timeouts, never on the suspected bug itself.
func TestStaleProposedRecordRestore(t *testing.T) {
	t.Parallel()
	for _, syncOnRestart := range []bool{false, true} {
		t.Run(fmt.Sprintf("syncOnRestart=%v", syncOnRestart), func(t *testing.T) {
			staleProposedRecordRestore(t, syncOnRestart)
		})
	}
}

func staleProposedRecordRestore(t *testing.T, syncOnRestart bool) {
	network := NewNetwork()
	defer network.Shutdown()

	testDir := t.TempDir()

	const numberOfNodes = 4
	nodes := make([]*App, 0, numberOfNodes)
	for i := 1; i <= numberOfNodes; i++ {
		n := newNode(uint64(i), network, t.Name(), testDir, false, 0)
		nodes = append(nodes, n)
	}

	// Record the logs of every node (the stale prepare re-broadcast and any view change show up
	// on the other nodes), and drive the test off node 4's log. Setup rebuilds each consensus so
	// that its components use the hooked logger.
	recorders := make([]*logRecorder, numberOfNodes)
	for i, n := range nodes {
		rec := &logRecorder{}
		recorders[i] = rec
		n.logger = n.logger.Desugar().WithOptions(zap.Hooks(rec.hook)).Sugar()
		n.Setup()
	}

	// Node 1 leads view 0. Make it send heartbeats every second (timeout/count) so that a
	// lagging follower notices the leader's sequence quickly. Note that App.Setup rebuilds the
	// consensus from the closure's copy of fastConfig, so this must be set after Setup and
	// applies to node 1 only, which is never restarted in this test.
	nodes[0].Consensus.Config.LeaderHeartbeatTimeout = 10 * time.Second
	nodes[0].Consensus.Config.LeaderHeartbeatCount = 10

	victim := nodes[3]
	vlog := recorders[3]

	// Step 1: node 4 loses every prepare and commit (but keeps pre-prepares, heartbeats and
	// state-transfer traffic) until the restart.
	var dropVotes atomic.Bool
	var droppedPrepares, droppedCommits atomic.Int64
	dropVotes.Store(true)
	victim.LoseMessages(func(m *smartbftprotos.Message) bool {
		if !dropVotes.Load() {
			return false
		}
		if m.GetPrepare() != nil {
			droppedPrepares.Add(1)
			return true
		}
		if m.GetCommit() != nil {
			droppedCommits.Add(1)
			return true
		}
		return false
	})

	startNodes(nodes, network)

	// Step 2: request 1. Nodes 1-3 decide sequence 1; node 4 processes PP(1), writes it to its
	// WAL and gets stuck in PROPOSED waiting for prepares that never arrive.
	nodes[0].Submit(Request{ID: "1", ClientID: "alice"})
	for i := 0; i < 3; i++ {
		awaitDelivery(t, nodes[i], 1, 30*time.Second)
	}
	require.True(t, vlog.waitFor(30*time.Second, 0, "Processed proposal with seq 1"),
		"harness timeout: node 4 never processed PP(1)")
	t.Logf("step 2: node 4 processed PP(1); nodes 1-3 delivered seq 1")

	// Step 3: request 2. Nodes 1-3 decide sequence 2. The leader's next heartbeat carries seq 3,
	// node 4 is at seq 1, so its heartbeat monitor calls Sync: checkpoint jumps to 2 and the
	// controller restarts view 0 at sequence 3 without writing anything to the WAL.
	nodes[0].Submit(Request{ID: "2", ClientID: "alice"})
	for i := 0; i < 3; i++ {
		awaitDelivery(t, nodes[i], 2, 30*time.Second)
	}
	require.True(t, vlog.waitFor(30*time.Second, 0, "Heartbeat sequence is bigger than expected"),
		"harness timeout: node 4's heartbeat monitor never noticed it is behind; lines so far: %v",
		vlog.matching(0, "eartbeat"))
	require.True(t, vlog.waitFor(30*time.Second, 0, "Synchronizer returned with sequence 2 while the controller is at sequence 0"),
		"harness timeout: node 4 never synced to sequence 2; sync lines: %v", vlog.matching(0, "Synchronizer"))
	require.True(t, vlog.waitFor(30*time.Second, 0, "Starting view with number 0, sequence 3"),
		"harness timeout: node 4 never restarted view 0 at sequence 3; lines: %v", vlog.matching(0, "Starting view"))
	// The sync delivered sequences 1 and 2 to node 4's application.
	awaitDelivery(t, victim, 1, 30*time.Second)
	awaitDelivery(t, victim, 2, 30*time.Second)

	processedBeforeRestart := vlog.matching(0, "Processed proposal with seq")
	t.Logf("step 3: node 4 synced; 'Processed proposal' lines before restart: %v", processedBeforeRestart)
	t.Logf("step 3: node 4 dropped %d prepares and %d commits so far", droppedPrepares.Load(), droppedCommits.Load())
	for _, l := range vlog.matching(0, "Synchronizer returned") {
		t.Logf("step 3: node 4 log: %s", l)
	}
	for _, l := range vlog.matching(0, "Starting view with number") {
		t.Logf("step 3: node 4 log: %s", l)
	}

	// Step 4: restart node 4 before it processes any new pre-prepare, and look at its WAL.
	// This mirrors App.RestartSync but exposes the WAL content read at start-up.
	victim.Consensus.Stop()
	victim.Node.Lock()
	victim.Setup()
	walEntries := victim.Consensus.WALInitialContent
	walDescription := describeWAL(walEntries)
	checkpointSeq := victim.latestMD.GetLatestSequence()
	t.Logf("step 4: node 4 WAL at restart (%d entries): %v", len(walEntries), walDescription)
	t.Logf("step 4: node 4 application checkpoint at restart: view %d seq %d", victim.latestMD.GetViewId(), checkpointSeq)
	preconditionMet := len(walEntries) == 1 && strings.HasPrefix(walDescription[0], "[0] proposed_record view=0 seq=1") && checkpointSeq == 2
	t.Logf("step 4: pre-restart precondition (WAL == [PP(1)], checkpoint == 2, no new_view record) met: %v", preconditionMet)

	restartMark := vlog.mark()
	dropVotes.Store(false) // participate normally from now on
	victim.Consensus.Config.SyncOnStart = syncOnRestart
	require.NoError(t, victim.Consensus.Start())
	victim.Node.Unlock()

	require.True(t, vlog.waitFor(30*time.Second, restartMark, "Restored proposal with sequence"),
		"harness timeout: node 4 did not log a restore after restart; lines: %v", vlog.matching(restartMark, "estore"))
	require.True(t, vlog.waitFor(30*time.Second, restartMark, "Starting view with number"),
		"harness timeout: node 4 did not start a view after restart")
	for _, l := range vlog.matching(restartMark, "Restored proposal with sequence") {
		t.Logf("step 4: node 4 log: %s", l)
	}
	for _, l := range vlog.matching(restartMark, "Starting view with number") {
		t.Logf("step 4: node 4 log: %s", l)
	}
	for _, l := range vlog.matching(restartMark, "Synchronizer returned") {
		t.Logf("step 4: node 4 log (sync on start): %s", l)
	}

	// Step 5: requests 3, 4 and 5. Observe whether node 4 takes part in sequence 3 or misses it.
	nodes[0].Submit(Request{ID: "3", ClientID: "alice"})
	for i := 0; i < 3; i++ {
		awaitDelivery(t, nodes[i], 3, 30*time.Second)
	}
	// Give node 4 a moment to react to PP(3) and the prepares for it (observation only).
	sawWrongDigest := vlog.waitFor(5*time.Second, restartMark, "Got wrong digest at processPrepares")
	t.Logf("step 5: after seq 3 decided by nodes 1-3, node 4 logged 'Got wrong digest at processPrepares': %v", sawWrongDigest)
	seq3Direct, got := tryDelivery(t, victim, 3*time.Second)
	if got {
		t.Logf("step 5: node 4 delivered seq %d within 3s of nodes 1-3 deciding seq 3", seq3Direct)
	} else {
		t.Logf("step 5: node 4 delivered nothing within 3s of nodes 1-3 deciding seq 3")
	}

	nodes[0].Submit(Request{ID: "4", ClientID: "alice"})
	for i := 0; i < 3; i++ {
		awaitDelivery(t, nodes[i], 4, 30*time.Second)
	}

	// Node 4 must eventually deliver 3 and 4 (by consensus or by a later sync) or the harness is broken.
	delivered := make([]uint64, 0, 3)
	if got {
		delivered = append(delivered, seq3Direct)
	}
	for len(delivered) < 2 {
		s, ok := tryDelivery(t, victim, 60*time.Second)
		require.True(t, ok, "harness timeout: node 4 delivered only %v of seqs 3,4 within 60s; sync lines: %v; view lines: %v",
			delivered, vlog.matching(restartMark, "Synchronizer returned"), vlog.matching(restartMark, "Starting view"))
		delivered = append(delivered, s)
	}
	t.Logf("step 5: node 4 delivered (in order): %v", delivered)

	nodes[0].Submit(Request{ID: "5", ClientID: "alice"})
	for i := 0; i < numberOfNodes; i++ {
		awaitDelivery(t, nodes[i], 5, 60*time.Second)
	}
	t.Logf("step 5: all 4 nodes delivered seq 5")

	// Summary of node 4's behaviour after the restart.
	type probe struct {
		name string
		subs []string
	}
	probes := []probe{
		{"restored stale proposal (seq 1)", []string{"Restored proposal with sequence 1"}},
		{"pre-prepare for seq 3 received", []string{"got message <PrePrepare with view: 0, seq: 3"}},
		{"'dropping message' (pre-prepare dropped)", []string{"dropping message"}},
		{"'Got wrong digest at processPrepares'", []string{"Got wrong digest at processPrepares"}},
		{"'Got wrong digest at processCommits'", []string{"Got wrong digest at processCommits"}},
		{"'Heartbeat sequence is bigger than expected' (immediate sync)", []string{"Heartbeat sequence is bigger than expected"}},
		{"'Our sequence is behind the heartbeat sequence' (tick-counted sync)", []string{"Our sequence is behind the heartbeat sequence"}},
		{"'Synchronizer returned with sequence'", []string{"Synchronizer returned with sequence"}},
		{"'Sync deliver' (decisions delivered through sync)", []string{"Sync deliver"}},
		{"'Starting view with number'", []string{"Starting view with number"}},
		{"'Heartbeat timeout expired'", []string{"Heartbeat timeout expired"}},
		{"'started view change'", []string{"started view change"}},
		{"'is joining view change'", []string{"is joining view change"}},
		{"'processing a new view message'", []string{"processing a new view message"}},
		{"'ViewChanged, the new view is'", []string{"ViewChanged"}},
		{"'Processed proposal with seq'", []string{"Processed proposal with seq"}},
		{"'processed commits for proposal with seq'", []string{"processed commits for proposal with seq"}},
	}
	t.Logf("---- node 4 log counts after restart (syncOnRestart=%v) ----", syncOnRestart)
	for _, p := range probes {
		t.Logf("  %-70s %d", p.name, vlog.count(restartMark, p.subs...))
	}
	for _, key := range []string{"Got wrong digest", "Synchronizer returned", "Starting view with number", "Sync deliver", "Processed proposal with seq", "processed commits", "dropping message", "Heartbeat sequence is bigger", "Our sequence is behind"} {
		for _, l := range vlog.matching(restartMark, key) {
			t.Logf("  node 4: %s", l)
		}
	}

	// Evidence on the other nodes: the stale prepare for seq 1 that node 4 re-broadcasts on restart
	// is logged by its receivers, and a view change would show up on every node.
	staleRebroadcastSeen := 0
	for i := 0; i < 3; i++ {
		lines := recorders[i].matching(restartMark, "got message from 4 with sequence 1 but our sequence is")
		staleRebroadcastSeen += len(lines)
		for _, l := range lines {
			t.Logf("  node %d: %s", i+1, l)
		}
	}
	viewChangeProbes := []string{"started view change", "is joining view change", "processing a view change message", "ViewChanged", "Starting view with number 1", "Heartbeat timeout expired"}
	viewChangeLines := 0
	for i, rec := range recorders {
		for _, p := range viewChangeProbes {
			lines := rec.matching(0, p)
			viewChangeLines += len(lines)
			for _, l := range lines {
				t.Logf("  node %d view-change evidence: %s", i+1, l)
			}
		}
	}

	missedSeq3 := !got || seq3Direct != 3
	syncsAfterRestart := vlog.count(restartMark, "Synchronizer returned with sequence")
	viewChangeOnVictim := vlog.count(restartMark, "started view change")+vlog.count(restartMark, "is joining view change")+vlog.count(restartMark, "Starting view with number 1") > 0
	t.Logf("SUMMARY syncOnRestart=%v: precondition=%v restoredStale=%v staleSeq1PrepareReceivedByOthers=%d wrongDigest=%d missedSeq3Within3s=%v syncsAfterRestart=%d syncDelivers=%d viewChangeOnNode4=%v viewChangeLinesAllNodes=%d delivered=%v",
		syncOnRestart, preconditionMet,
		vlog.count(restartMark, "Restored proposal with sequence 1") > 0,
		staleRebroadcastSeen,
		vlog.count(restartMark, "Got wrong digest at processPrepares"),
		missedSeq3, syncsAfterRestart, vlog.count(restartMark, "Sync deliver"),
		viewChangeOnVictim, viewChangeLines, delivered)
}
```

</details>
