package fabricd

import (
	"github.com/aiomni/dune/internal/lifecycle"
	"github.com/aiomni/dune/pkg/api"
)

func lifecycleUsage(log *lifecycle.Log) *api.LifecycleLogUsage {
	if log == nil {
		return nil
	}
	usage := log.Usage()
	return &api.LifecycleLogUsage{Bytes: api.CapacityUsage{Used: int(usage.Bytes), Limit: usage.ByteLimit}, Queue: api.CapacityUsage{Used: usage.Queued, Limit: usage.QueueLimit}, Dropped: usage.Dropped, WriteErrors: usage.WriteErrors}
}

func (d *Engine) recordLifecycle(kind string, r *runtime, ref, code string, term uint64) {
	entry := lifecycle.Entry{Kind: kind, OperationRef: ref, Code: code, ConnectorTerm: term}
	if r != nil {
		entry.RuntimeID, entry.RuntimeIncarnation = r.id, r.inc
		if r.hostInfo != nil {
			entry.HostInstance = r.hostInfo.Instance
		}
	}
	d.events.Record(entry)
}

// RecordGatewayUnavailable lets connector transports report dial failures
// without persisting remote error bodies, URLs, headers or credential material.
func (d *Engine) RecordGatewayUnavailable() {
	d.recordLifecycle("gateway_unavailable", nil, "", "GATEWAY_UNAVAILABLE", d.sessionTerm)
}
