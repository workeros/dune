package api

// ObservationVersion orders complete Runtime snapshots, including native-session
// confirmation and lifecycle facts that do not change SessionMetadata.Revision.
// Revisions are comparable only within the same epoch and exact Runtime identity.
// An epoch change requires a fresh directory subscription and discovery.
type ObservationVersion struct {
	Epoch    string `json:"epoch"`
	Revision uint64 `json:"revision,string"`
}

func (v ObservationVersion) Valid() bool { return v.Epoch != "" && v.Revision != 0 }
