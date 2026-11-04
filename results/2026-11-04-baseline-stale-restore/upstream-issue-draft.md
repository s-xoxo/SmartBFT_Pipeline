# Draft — issue for hyperledger-labs/SmartBFT (not posted yet)

**Title:** Restore of a `proposed_record` ignores the application checkpoint and brings a stale proposal back into the view

**Summary.** `PersistedState.Restore` → `recoverProposed` (internal/bft/state.go) restores the last `proposed_record` of the WAL as the proposal in flight without comparing its sequence with the application's last delivered sequence. `recoverPrepared` does compare ("has been safely committed" / "WAL out of sync"), `recoverProposed` does not. `ProposalMaker.NewProposer` (internal/bft/util.go) then only raises `ProposalSequence` to the controller's sequence and leaves `Phase = PROPOSED` and `inFlightProposal` in place, so the view starts at the right sequence holding the wrong proposal.

**How to reach it (deterministic, 4 nodes, reproduced on main 32a628d6).**
1. A follower processes the pre-prepare of sequence n (WAL = `[PP(n)]`, phase PROPOSED) and then loses the prepares and commits of n.
2. The cluster moves on. The follower sees the leader's heartbeat with a higher sequence ("Heartbeat sequence is bigger than expected"), calls `Synchronizer.Sync`, its checkpoint advances past n and the controller restarts the view at checkpoint+1. `sync()` writes no `new_view` record (fetchState returns the same view), so the WAL still ends with `PP(n)`.
3. The follower restarts (with or without `SyncOnStart`). `Restore` logs "Restored proposal with sequence n" and the view starts as "Starting view with number v, sequence C+1" in phase PROPOSED with proposal n.

**Observed after the restart.**
- the node re-broadcasts the stale prepare of sequence n (peers log "got message from 4 with sequence n but our sequence is C+1");
- the leader's pre-prepare of C+1 is received but never processed (the view is in `processPrepares` for the stale proposal);
- every prepare of C+1 is rejected: "Got wrong digest at processPrepares for prepare with seq C+1, expecting <digest of n> but got <digest of C+1>, we are in seq C+1";
- the node does not decide C+1 (nor C+2) by consensus and recovers only through a later heartbeat-triggered sync. No safety violation; a liveness/efficiency defect for that node. No view change with a single affected node (f+1 would be needed).

**Suggested fix.** In `recoverProposed`, drop the record when its sequence is at or below the application's last delivered sequence (the same check `recoverPrepared` makes), and/or have `NewProposer` reset the phase and the in-flight fields when it raises the sequence past the restored one.

**An existing test pins the behaviour.** `TestRestartKeepsViewAndInFlightProposal` (test/wal_truncation_test.go, from #739) restarts the isolated node with `Restart()` (sync on): the sync delivers sequence 1, so the proposal the test then expects to be "restored in flight" and whose prepare it expects to be re-broadcast is already delivered. With the fix the test needs `RestartSync(false)` for that restart, which is what its description ("crashed before collecting a quorum") means; its other assertions (same view, same WAL record, no WAL growth over ten restarts) are unaffected.

**Reproduction test.** A self-contained integration test (test/ harness, 4 nodes, ~15 s) is attached: it loses prepares/commits on one node, waits for the heartbeat-driven sync, restarts the node and records the log lines above. Three runs, identical results, both with `SyncOnStart` true and false.
