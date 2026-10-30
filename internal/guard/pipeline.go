package guard

import "time"

type BatchState struct {
	Kind       string    `json:"kind"`
	Status     string    `json:"status"`
	Total      int       `json:"total"`
	Completed  int       `json:"completed"`
	QueuedIDs  []int64   `json:"queued_ids"`
	ActiveIDs  []int64   `json:"active_ids"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
}

func withoutID(ids []int64, id int64) []int64 {
	for i, x := range ids {
		if x == id {
			return append(ids[:i], ids[i+1:]...)
		}
	}
	return ids
}
func nextBudgetReset(now time.Time) time.Time {
	n := now.UTC()
	return time.Date(n.Year(), n.Month(), n.Day()+1, 0, 0, 0, 0, time.UTC)
}
func (e *Engine) budgetLocked() map[string]any {
	used := 0
	if e.Store.State.Day == time.Now().UTC().Format("2006-01-02") {
		used = e.Store.State.Requests
	}
	remaining := e.Config.DailyRequests - used - e.reservedRequests
	if remaining < 0 {
		remaining = 0
	}
	return map[string]any{"used": used, "limit": e.Config.DailyRequests, "reserved": e.reservedRequests, "remaining": remaining, "reset_at": nextBudgetReset(time.Now())}
}
