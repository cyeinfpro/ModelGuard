package guard

// Status is the shared persisted/UI vocabulary. Unknown legacy values are only
// migrated while loading a store, never produced by current transitions.
type Status string

const (
	StatusUnverified       Status = "unverified"
	StatusPending          Status = "pending"
	StatusPassed           Status = "passed"
	StatusRecovering       Status = "recovering"
	StatusRecoveryReady    Status = "recovery_ready"
	StatusPauseRecommended Status = "pause_recommended"
	StatusQuarantined      Status = "quarantined"
	StatusUnavailable      Status = "unavailable"
	StatusInconclusive     Status = "inconclusive"
	StatusBlocked          Status = "blocked"
	StatusSkipped          Status = "skipped"
	StatusUnsupported      Status = "unsupported"
	StatusPauseUncertain   Status = "pause_uncertain"
	StatusArchived         Status = "archived"
)

type StatusInfo struct {
	Label    string `json:"label"`
	Category string `json:"category"`
}

// StatusCatalog is returned as a fresh map so consumers cannot mutate the shared
// definition. Legacy values remain readable for old diagnostic clients.
func StatusCatalog() map[Status]StatusInfo {
	return map[Status]StatusInfo{
		StatusUnverified:       {"等待首次检测", "pending"},
		StatusPending:          {"等待检测", "pending"},
		StatusPassed:           {"通过", "ok"},
		StatusRecovering:       {"恢复确认中", "held"},
		StatusRecoveryReady:    {"等待恢复", "held"},
		StatusPauseRecommended: {"指纹不符", "attention"},
		StatusQuarantined:      {"已暂停调度", "held"},
		StatusUnavailable:      {"采样失败", "attention"},
		StatusInconclusive:     {"评分不确定", "attention"},
		StatusBlocked:          {"需要处理", "attention"},
		StatusSkipped:          {"已跳过", "attention"},
		StatusUnsupported:      {"暂不支持", "attention"},
		StatusPauseUncertain:   {"暂停待核对", "attention"},
		StatusArchived:         {"已归档", "archived"},
		"external_change":      {"外部状态变更", "attention"},
		"paused_observation":   {"暂停观察", "held"},
		"suspect":              {"指纹不符", "attention"},
	}
}

func migrateStatus(r *Record) {
	switch r.Status {
	case "suspect":
		if r.Hold {
			r.Status = StatusQuarantined
		} else {
			r.Status = StatusPauseRecommended
		}
	case "paused_observation":
		if r.Analysis == nil {
			r.Status = StatusUnavailable
		} else if r.Analysis.Decision == "indeterminate" {
			r.Status = StatusInconclusive
		} else if r.Hold {
			r.Status = StatusQuarantined
		} else {
			r.Status = StatusPending
		}
	}
}

// A mismatch is an actionable quality result, but still a completed check.
// Operational errors and inconclusive scoring need distinct follow-up.
func recordNeedsAttention(r *Record) bool {
	if r == nil {
		return true
	}
	switch r.Status {
	case StatusUnavailable, StatusInconclusive, StatusSkipped, StatusUnsupported, StatusBlocked, "external_change", StatusPauseUncertain:
		return true
	case "paused_observation":
		return r.Analysis == nil || r.Analysis.Decision == "indeterminate"
	}
	return false
}
