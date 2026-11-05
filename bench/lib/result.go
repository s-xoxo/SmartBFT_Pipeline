package lib

// NodeResult is what one process-separated node (cmd/node) writes at the end of a run; cmd/matrix
// reads it. Counts prefixed with "measured" cover the measurement window only.
type NodeResult struct {
	ID     uint64 `json:"id"`
	Leader uint64 `json:"leader_at_end"` // Consensus.GetLeaderID() when the window closed
	N      int    `json:"n"`
	W      uint64 `json:"w"`
	RTTms  uint64 `json:"rtt_ms"`
	K      int    `json:"k"`
	Batch  uint64 `json:"batch"`
	Note   string `json:"note"`

	StartUnixNano        int64 `json:"start_unix_nano"`         // process start (PerSecond is relative to it)
	ReadyUnixNano        int64 `json:"ready_unix_nano"`         // warm-up done
	MeasureStartUnixNano int64 `json:"measure_start_unix_nano"` // == Ready
	MeasureEndUnixNano   int64 `json:"measure_end_unix_nano"`

	Decisions       uint64  `json:"measured_decisions"`
	Requests        uint64  `json:"measured_requests"`
	TotalDecisions  uint64  `json:"total_decisions"`
	TotalRequests   uint64  `json:"total_requests"`
	TPS             float64 `json:"tps"`
	DecisionsPerSec float64 `json:"decisions_per_s"`
	LatMeanMs       float64 `json:"lat_mean_ms"`
	LatP50Ms        float64 `json:"lat_p50_ms"`
	LatP99Ms        float64 `json:"lat_p99_ms"`
	LatMaxMs        float64 `json:"lat_max_ms"`
	DeliverMeanUs   float64 `json:"deliver_mean_us"`
	MaxInFlight     float64 `json:"max_in_flight"`

	ViewChanges     uint64 `json:"view_changes"`      // "Starting view with number X", X > 0
	InFlightCommits uint64 `json:"in_flight_commits"` // "In-flight view ... has committed a decision"
	Syncs           uint64 `json:"syncs"`
	Warnings        uint64 `json:"warnings"`
	FirstWarning    string `json:"first_warning"`

	SubmitFailed uint64 `json:"submit_failed"` // local SubmitRequest errors seen by the client
	Resubmits    uint64 `json:"resubmits"`     // client requests resubmitted after the timeout
	Dropped      uint64 `json:"dropped"`       // outbound frames dropped (link down / queue full)
	Reconnects   uint64 `json:"reconnects"`
	Submits      uint64 `json:"remote_submits"` // frameSubmit received from a client elsewhere
	SyncServed   uint64 `json:"sync_served"`
	BytesIn      uint64 `json:"bytes_in"`
	BytesOut     uint64 `json:"bytes_out"`

	// ViewStartUnixNanos are the times of the "Starting view with number X" (X > 0) lines.
	ViewStartUnixNanos []int64 `json:"view_start_unix_nanos"`
	// FirstDeliveryAfterViewChangeUnixNano is the first delivery after the first view start (0 if none);
	// PostViewChange15sRequests counts the requests delivered in the 15 s after it.
	FirstDeliveryAfterViewChangeUnixNano int64  `json:"first_delivery_after_vc_unix_nano"`
	PostViewChange15sRequests            uint64 `json:"post_vc_15s_requests"`
	PostViewChange15sComplete            bool   `json:"post_vc_15s_complete"`
	// ClockJumpMs is wall-clock elapsed minus monotonic elapsed over the window; > 0 means the machine slept.
	ClockJumpMs float64 `json:"clock_jump_ms"`

	// PerSecond[i] is the number of requests delivered during second i after StartUnixNano.
	PerSecond []uint64 `json:"per_second"`
	// Gaps lists every pause of GapThreshold or more between two consecutive deliveries.
	Gaps []Gap `json:"gaps"`
}

// Gap is a pause between two consecutive deliveries, with the throughput in the 15 s after it.
type Gap struct {
	BeforeUnixNano int64  `json:"before_unix_nano"` // last delivery before the pause
	AfterUnixNano  int64  `json:"after_unix_nano"`  // first delivery after it
	BeforeSeq      uint64 `json:"before_seq"`
	AfterSeq       uint64 `json:"after_seq"`
	// Post15sRequests counts requests delivered in [After, After+15s); Post15sComplete says whether the
	// run lasted long enough for the whole 15 s.
	Post15sRequests uint64 `json:"post_15s_requests"`
	Post15sComplete bool   `json:"post_15s_complete"`
}
