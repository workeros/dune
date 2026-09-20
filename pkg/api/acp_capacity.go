package api

import "time"

// SubmissionCapacity reports installation-wide durable reservations. A read
// neither allocates evidence nor changes its retention. Completed controls are
// included in Accepted and Used, not an additional pool that can be borrowed.
type SubmissionCapacity struct {
	Ordinary       CapacityUsage                   `json:"ordinary"`
	Claimed        int                             `json:"claimed"`
	Accepted       int                             `json:"accepted"`
	Rejected       int                             `json:"rejected"`
	Controls       map[string]ControlCapacityUsage `json:"controls"`
	Runtimes       CapacityUsage                   `json:"runtimes"`
	RuntimeRecords CapacityUsage                   `json:"runtime_records"`
}

type ControlCapacityUsage struct {
	Used      int `json:"used"`
	Limit     int `json:"limit"`
	Reserved  int `json:"reserved"`
	Claimed   int `json:"claimed"`
	Accepted  int `json:"accepted"`
	Rejected  int `json:"rejected"`
	Completed int `json:"completed"`
}

// ACPResourceUsage belongs to the original host, including while fabricd is
// absent. Result slots remain occupied until the host's completion TTL expires.
type ACPResourceUsage struct {
	Queue       CapacityUsage     `json:"queue"`
	Permissions CapacityUsage     `json:"permissions"`
	Operations  ACPOperationUsage `json:"operations"`
}

type ACPOperationUsage struct {
	Records          CapacityUsage `json:"records"`
	Unfinished       int           `json:"unfinished"`
	Completed        int           `json:"completed"`
	Bytes            CapacityUsage `json:"bytes"`
	Updates          CapacityUsage `json:"updates"`
	OldestCompletion *time.Time    `json:"oldest_completion,omitempty"`
	NextExpiry       *time.Time    `json:"next_expiry,omitempty"`
}
