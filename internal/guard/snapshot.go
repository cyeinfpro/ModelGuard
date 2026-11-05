package guard

import (
	"encoding/json"
	"sort"
	"time"
)

func (e *Engine) initializeSnapshot() {
	if e.Detector != nil {
		e.modelsJSON, _ = json.Marshal(e.Detector.Models())
	}
	if len(e.modelsJSON) == 0 {
		e.modelsJSON = []byte("[]")
	}
	e.revision, e.stateRevision = 1, 1
}

// Snapshot builds immutable cold data only after state changes and serializes
// outside stateMu. Progress callbacks merely increment a revision; concurrent
// readers share the cached JSON rather than repeatedly copying every record.
func (e *Engine) Snapshot() string {
	e.snapshotMu.Lock()
	defer e.snapshotMu.Unlock()
	e.stateMu.Lock()
	day := time.Now().UTC().Format("2006-01-02")
	if e.snapshot != "" && e.snapshotRevision == e.revision && e.snapshotDay == day {
		e.stateMu.Unlock()
		return e.snapshot
	}
	revision, stateRevision := e.revision, e.stateRevision
	coldChanged := e.stateSnapshot == nil || e.stateSnapshotRevision != stateRevision
	var state State
	if coldChanged {
		state = e.Store.snapshotState()
	}
	active := make([]*ActiveSample, 0, len(e.activeSamples))
	for _, progress := range e.activeSamples {
		copy := *progress
		active = append(active, &copy)
	}
	sort.Slice(active, func(i, j int) bool { return active[i].AccountID < active[j].AccountID })
	var first *ActiveSample
	if len(active) > 0 {
		first = active[0]
	}
	var batch *BatchState
	if e.batch != nil {
		copy := *e.batch
		copy.ActiveIDs = append([]int64(nil), e.batch.ActiveIDs...)
		copy.QueuedIDs = append([]int64(nil), e.batch.QueuedIDs...)
		batch = &copy
	}
	lastError := e.lastError
	if e.storageError != "" {
		lastError = e.storageError
	}
	c := e.Config
	version := ""
	if e.Detector != nil {
		version = e.Detector.Version()
	}
	payload := map[string]any{
		"enabled": c.Enabled, "auto_pause": c.AutoPause, "auto_recover": c.AutoRecover,
		"recovery_spacing_seconds": c.RecoverySpacingSeconds, "family_match": c.FamilyMatch,
		"last_error": lastError, "storage_error": e.storageError,
		"active_sample": first, "active_samples": active, "batch": batch,
		"startup_ready_at": e.readyAt, "budget": e.budgetLocked(), "concurrency": c.Concurrency,
		"detector_version": version, "models": json.RawMessage(e.modelsJSON),
		"status_catalog": StatusCatalog(), "revision": revision, "state_revision": stateRevision,
		"storage_metrics": e.Store.metrics,
	}
	e.stateMu.Unlock()
	if coldChanged {
		raw, err := json.Marshal(state)
		if err != nil {
			return e.snapshot
		}
		e.stateSnapshot, e.stateSnapshotRevision = raw, stateRevision
	}
	payload["state"] = json.RawMessage(e.stateSnapshot)
	raw, err := json.Marshal(payload)
	if err != nil {
		return e.snapshot
	}
	e.snapshot, e.snapshotRevision, e.snapshotDay = string(raw), revision, day
	return e.snapshot
}
