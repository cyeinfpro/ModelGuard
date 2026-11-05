package guard

import (
	"time"

	"github.com/cyeinfpro/ModelGuard/internal/fingerprint"
)

type Record struct {
	AccountID      int64                 `json:"account_id"`
	Model          string                `json:"model"`
	Expected       string                `json:"expected"`
	Status         Status                `json:"status"`
	Reason         string                `json:"reason"`
	Failures       int                   `json:"failures"`
	Unavailable    int                   `json:"unavailable"`
	Inconclusive   int                   `json:"inconclusive"`
	RecoveryPasses int                   `json:"recovery_passes"`
	RecoveryAt     time.Time             `json:"recovery_at,omitempty"`
	Hold           bool                  `json:"hold"`
	Pending        bool                  `json:"pending"`
	CheckedAt      time.Time             `json:"checked_at"`
	NextAt         time.Time             `json:"next_at"`
	PausedAt       time.Time             `json:"paused_at"`
	PauseUpdatedAt string                `json:"pause_updated_at,omitempty"`
	Analysis       *fingerprint.Analysis `json:"analysis,omitempty"`
}
type Event struct {
	At        time.Time `json:"at"`
	AccountID int64     `json:"account_id"`
	Action    string    `json:"action"`
	Reason    string    `json:"reason"`
}
type ManualRun struct {
	CurrentIDs   []int64   `json:"current_ids"`
	Passed       int       `json:"passed"`
	Mismatches   int       `json:"mismatches"`
	Unavailable  int       `json:"unavailable"`
	Inconclusive int       `json:"inconclusive"`
	Skipped      int       `json:"skipped"`
	ID           string    `json:"id"`
	Status       string    `json:"status"`
	Total        int       `json:"total"`
	Completed    int       `json:"completed"`
	Errors       int       `json:"errors"`
	CurrentID    int64     `json:"current_id"`
	RequestedAt  time.Time `json:"requested_at"`
	StartedAt    time.Time `json:"started_at"`
	FinishedAt   time.Time `json:"finished_at"`
	Message      string    `json:"message"`
}
type State struct {
	Manual   *ManualRun        `json:"manual,omitempty"`
	Version  int               `json:"version"`
	AdminURL string            `json:"admin_url"`
	Records  map[int64]*Record `json:"records"`
	// Events is a bounded compatibility projection. Persistence writes the audit
	// journal separately, so sample traffic cannot evict control evidence.
	Events         []Event `json:"events,omitempty"`
	AuditEvents    []Event `json:"audit_events,omitempty"`
	ActivityEvents []Event `json:"activity_events,omitempty"`
	Day            string  `json:"day"`
	Requests       int     `json:"requests"`
}

// Three complete samples form one round. Inconclusive scoring is distinct from
// transport failure; neither can create a new pause or confirm a recovery.
func Transition(r *Record, verdict, reason string, now time.Time, c Config) bool {
	r.CheckedAt = now
	r.Reason = reason
	r.NextAt = now.Add(time.Duration(c.NormalSeconds) * time.Second)
	if verdict == "match" || verdict == "family_match" {
		r.Failures = 0
		r.Unavailable = 0
		r.Inconclusive = 0
		if r.Hold {
			spacing := time.Duration(c.RecoverySpacingSeconds) * time.Second
			if r.RecoveryPasses == 0 || spacing == 0 || !now.Before(r.RecoveryAt.Add(spacing)) {
				r.RecoveryPasses++
				r.RecoveryAt = now
			}
			r.Status = StatusRecovering
			r.NextAt = now.Add(time.Duration(c.RetrySeconds) * time.Second)
			if due := r.RecoveryAt.Add(spacing); due.After(r.NextAt) {
				r.NextAt = due
			}
			if r.RecoveryPasses >= 2 {
				r.Status = StatusRecoveryReady
				r.NextAt = now.Add(time.Duration(c.QuarantineSeconds) * time.Second)
			}
		} else {
			r.Status = StatusPassed
			r.RecoveryPasses = 0
			r.RecoveryAt = time.Time{}
		}
		return false
	}
	r.RecoveryPasses = 0
	r.RecoveryAt = time.Time{}
	r.NextAt = now.Add(time.Duration(c.RetrySeconds) * time.Second)
	switch verdict {
	case "mismatch":
		r.Failures++
		r.Unavailable = 0
		r.Inconclusive = 0
	case "indeterminate", "inconclusive":
		r.Inconclusive++
		r.Status = StatusInconclusive
		return false
	default:
		r.Unavailable++
		r.Status = StatusUnavailable
		return false
	}
	if r.Hold {
		r.Status = StatusQuarantined
		r.NextAt = now.Add(time.Duration(c.QuarantineSeconds) * time.Second)
		return false
	}
	r.Status = StatusPauseRecommended
	return c.AutoPause
}
