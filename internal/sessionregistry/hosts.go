package sessionregistry

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aiomni/dune/pkg/api"
)

// HostRecord is minimum execution/discovery evidence outside the Runtime
// directory. A process number is not live identity or control authority. Only
// a verified IPC handshake can attach; absence checks can prove loss.
type HostRecord struct {
	Target          api.SubmissionTarget
	Instance        string
	BootID          string
	PID             int
	GroupID         int
	GroupGeneration uint64
	Phase           string
	Runtime         api.Runtime
	Registration    json.RawMessage
	Resources       CleanupResources
}

// RegisterHost can activate an identity once. In particular, starting another
// host with a copied bootstrap cannot overwrite the original process evidence.
func (r *Registry) RegisterHost(ctx context.Context, host HostRecord) error {
	if host.Target.Validate() != nil || host.Target.RuntimeID == "" || api.ValidateSubmissionID(host.Instance) != nil || api.ValidateSubmissionID(host.BootID) != nil || host.PID <= 1 || len(host.Registration) > 64*1024 || !json.Valid(host.Registration) || !matchingRuntime(host.Target, host.Runtime) || host.Resources.Validate(host.Target, host.Instance) != nil {
		return fmt.Errorf("complete bounded host registration required")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := checkRuntimeOpen(ctx, tx, host.Target); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO session_hosts(target,instance,boot_id,pid,runtime,registration,resources) VALUES(?,?,?,?,?,?,?)`, encodeTarget(host.Target), host.Instance, host.BootID, host.PID, api.Payload(host.Runtime), host.Registration, api.Payload(host.Resources))
	if err != nil {
		return err
	}
	return tx.Commit()
}

func matchingRuntime(target api.SubmissionTarget, runtime api.Runtime) bool {
	return target.RuntimeID == runtime.ID && target.RuntimeIncarnation == runtime.Incarnation && target.RuntimeGeneration == runtime.Generation && len(api.Payload(runtime)) <= 16*1024
}

// RecordGroup precedes the guardian's start gate. Replacing a record requires
// the original host instance and current generation, after the caller proved
// the preceding group absent. Cleanup sealing and group registration share this
// transaction boundary, so an unadmitted spawn cannot cross a cleanup seal.
func (r *Registry) RecordGroup(ctx context.Context, target api.SubmissionTarget, instance string, previous uint64, groupID int) (uint64, error) {
	if groupID <= 1 || previous >= 1<<62 {
		return previous, fmt.Errorf("valid bounded process generation required")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return previous, err
	}
	defer tx.Rollback()
	if err := checkRuntimeOpen(ctx, tx, target); err != nil {
		return previous, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE session_hosts SET group_id=?,group_generation=group_generation+1 WHERE target=? AND instance=? AND group_generation=? AND phase='active'`, groupID, encodeTarget(target), instance, previous)
	if err != nil {
		return previous, err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return previous, conflict()
	}
	if err := tx.Commit(); err != nil {
		return previous, err
	}
	return previous + 1, nil
}

func (r *Registry) RecordHostRuntime(ctx context.Context, target api.SubmissionTarget, instance string, runtime api.Runtime) error {
	if !matchingRuntime(target, runtime) {
		return fmt.Errorf("original bounded Runtime description required")
	}
	phase := "active"
	if runtime.State == "exited" {
		phase = "exited"
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := checkRuntimeOpen(ctx, tx, target); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE session_hosts SET runtime=?,phase=? WHERE target=? AND instance=? AND phase='active'`, api.Payload(runtime), phase, encodeTarget(target), instance)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return conflict()
	}
	return tx.Commit()
}

const hostColumns = `h.target,h.instance,h.boot_id,h.pid,h.group_id,h.group_generation,h.phase,h.runtime,h.registration,h.resources`

func scanHost(row interface{ Scan(...any) error }, extra ...any) (HostRecord, error) {
	var host HostRecord
	var target, runtime, registration, resources []byte
	columns := []any{&target, &host.Instance, &host.BootID, &host.PID, &host.GroupID, &host.GroupGeneration, &host.Phase, &runtime, &registration, &resources}
	err := row.Scan(append(columns, extra...)...)
	host.Registration = registration
	if err != nil {
		return host, err
	}
	if json.Unmarshal(target, &host.Target) != nil || json.Unmarshal(runtime, &host.Runtime) != nil || json.Unmarshal(resources, &host.Resources) != nil || host.Resources.Validate(host.Target, host.Instance) != nil || host.Target.Validate() != nil || !matchingRuntime(host.Target, host.Runtime) || api.ValidateSubmissionID(host.Instance) != nil || api.ValidateSubmissionID(host.BootID) != nil || host.PID <= 1 || host.GroupID < 0 || (host.Phase != "active" && host.Phase != "exited") {
		return host, fmt.Errorf("invalid persisted host identity")
	}
	return host, nil
}

func (r *Registry) Host(ctx context.Context, target api.SubmissionTarget) (HostRecord, error) {
	if target.Validate() != nil || target.RuntimeID == "" {
		return HostRecord{}, fmt.Errorf("complete original Runtime target required")
	}
	return scanHost(r.db.QueryRowContext(ctx, `SELECT `+hostColumns+` FROM session_hosts h WHERE h.target=?`, encodeTarget(target)))
}

// Hosts is read-only and bounded by the reserved Runtime identity pool. It can
// rediscover a lost host even when its Runtime directory is absent or damaged.
type HostDiscovery struct {
	Hosts  []HostRecord
	Issues []api.RuntimeDiscoveryIssue
}

func (r *Registry) Hosts(ctx context.Context) (HostDiscovery, error) {
	discovery := HostDiscovery{Hosts: []HostRecord{}, Issues: []api.RuntimeDiscoveryIssue{}}
	// One snapshot includes reservations whose original host has not registered
	// yet. The launch link is fixed with admission, before starting any process.
	rows, err := r.db.QueryContext(ctx, `SELECT r.target,COALESCE(h.instance,''),COALESCE(h.boot_id,''),COALESCE(h.pid,0),
		COALESCE(h.group_id,0),COALESCE(h.group_generation,0),COALESCE(h.phase,''),h.runtime,h.registration,h.resources,
		k.runtime,COALESCE(k.stage,'')
		FROM runtime_reservations r LEFT JOIN session_hosts h ON r.target=h.target
		LEFT JOIN submission_keys k ON k.key=r.launch_key WHERE r.live=1 ORDER BY r.target LIMIT ?`, MaxRuntimeRecords)
	if err != nil {
		return discovery, err
	}
	defer rows.Close()
	for rows.Next() {
		var launchRuntime []byte
		var stage string
		host, err := scanHost(rows, &launchRuntime, &stage)
		if host.Instance == "" && len(launchRuntime) != 0 {
			var pending api.Runtime
			if json.Unmarshal(launchRuntime, &pending) == nil && matchingRuntime(host.Target, pending) {
				if pending.Adapter == "acp" {
					pending.Availability = "unavailable"
					code := "HOST_REGISTRATION_PENDING"
					if stage == "failed" {
						code = "LAUNCH_FAILED"
					}
					discovery.Issues = append(discovery.Issues, api.RuntimeDiscoveryIssue{Runtime: &pending, Code: code})
				}
				continue
			}
		}
		if host.Instance == "" && len(launchRuntime) == 0 && host.Target.Validate() == nil {
			// Direct internal reservations do not claim that an Agent was launched.
			continue
		}
		if err != nil {
			issue := api.RuntimeDiscoveryIssue{Code: "REGISTRATION_INVALID"}
			if host.Target.Validate() == nil && host.Target.RuntimeID != "" {
				issue.Runtime = &api.Runtime{ID: host.Target.RuntimeID, Incarnation: host.Target.RuntimeIncarnation, Generation: host.Target.RuntimeGeneration, Adapter: "acp", Availability: "unavailable"}
			}
			discovery.Issues = append(discovery.Issues, issue)
			continue
		}
		discovery.Hosts = append(discovery.Hosts, host)
	}
	return discovery, rows.Err()
}
