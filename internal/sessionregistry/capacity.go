package sessionregistry

import (
	"context"

	"github.com/aiomni/dune/pkg/api"
)

// Capacity uses a single SELECT snapshot across all classes. It must not use
// BeginTx: this database's immediate transactions are for write admission.
func (r *Registry) Capacity(ctx context.Context) (api.SubmissionCapacity, error) {
	out := api.SubmissionCapacity{
		Ordinary:       api.CapacityUsage{Limit: r.maxKeys},
		Runtimes:       api.CapacityUsage{Limit: MaxLiveRuntimes},
		RuntimeRecords: api.CapacityUsage{Limit: MaxRuntimeRecords},
		Controls:       make(map[string]api.ControlCapacityUsage),
	}
	for _, kind := range []string{ControlElicitation, ControlPermission, ControlCancel, ControlStop, ControlForget} {
		limit := r.maxControls
		if kind == ControlStop || kind == ControlForget {
			limit = MaxRuntimeRecords
		}
		out.Controls[kind] = api.ControlCapacityUsage{Limit: limit}
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT 'ordinary', COUNT(*), 0,
			COALESCE(SUM(state='claimed'),0), COALESCE(SUM(state='accepted'),0), COALESCE(SUM(state='not_accepted'),0), 0
		FROM submission_keys WHERE control_resource=''
		UNION ALL
		SELECT c.kind, COUNT(*), COALESCE(SUM(c.consumed_key=''),0),
			COALESCE(SUM(s.state='claimed'),0), COALESCE(SUM(s.state='accepted'),0), COALESCE(SUM(s.state='not_accepted'),0),
			COALESCE(SUM(s.state='accepted' AND s.stage IN ('written','input_unrecoverable','not_sent','stopped','completed')),0)
		FROM control_reservations c LEFT JOIN submission_keys s ON s.key=c.consumed_key GROUP BY c.kind
		UNION ALL
		SELECT 'runtimes', COALESCE(SUM(live),0), 0, 0, 0, 0, 0 FROM runtime_reservations
		UNION ALL
		SELECT 'runtime_records', COUNT(*), 0, 0, 0, 0, 0 FROM runtime_reservations`)
	if err != nil {
		return api.SubmissionCapacity{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var usage api.ControlCapacityUsage
		if err := rows.Scan(&kind, &usage.Used, &usage.Reserved, &usage.Claimed, &usage.Accepted, &usage.Rejected, &usage.Completed); err != nil {
			return api.SubmissionCapacity{}, err
		}
		switch kind {
		case "ordinary":
			out.Ordinary.Used, out.Claimed, out.Accepted, out.Rejected = usage.Used, usage.Claimed, usage.Accepted, usage.Rejected
		case "runtimes":
			out.Runtimes.Used = usage.Used
		case "runtime_records":
			out.RuntimeRecords.Used = usage.Used
		default:
			usage.Limit = out.Controls[kind].Limit
			out.Controls[kind] = usage
		}
	}
	return out, rows.Err()
}
