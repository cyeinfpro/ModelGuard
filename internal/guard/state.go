package guard

import (
	"encoding/json"
	"errors"
	"github.com/cyeinfpro/ModelGuard/internal/fingerprint"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type Record struct {
	AccountID      int64                 `json:"account_id"`
	Model          string                `json:"model"`
	Expected       string                `json:"expected"`
	Status         string                `json:"status"`
	Reason         string                `json:"reason"`
	Failures       int                   `json:"failures"`
	Unavailable    int                   `json:"unavailable"`
	RecoveryPasses int                   `json:"recovery_passes"`
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
	CurrentIDs  []int64   `json:"current_ids"`
	Passed      int       `json:"passed"`
	Mismatches  int       `json:"mismatches"`
	Unavailable int       `json:"unavailable"`
	Skipped     int       `json:"skipped"`
	ID          string    `json:"id"`
	Status      string    `json:"status"`
	Total       int       `json:"total"`
	Completed   int       `json:"completed"`
	Errors      int       `json:"errors"`
	CurrentID   int64     `json:"current_id"`
	RequestedAt time.Time `json:"requested_at"`
	StartedAt   time.Time `json:"started_at"`
	FinishedAt  time.Time `json:"finished_at"`
	Message     string    `json:"message"`
}
type State struct {
	Manual   *ManualRun        `json:"manual,omitempty"`
	Version  int               `json:"version"`
	AdminURL string            `json:"admin_url"`
	Records  map[int64]*Record `json:"records"`
	Events   []Event           `json:"events"`
	Day      string            `json:"day"`
	Requests int               `json:"requests"`
}
type Store struct {
	dir   string
	lock  *os.File
	State State
}

func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, errors.New("无法创建状态目录")
	}
	l, err := os.OpenFile(filepath.Join(dir, "guard.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, errors.New("无法打开运行锁")
	}
	if syscall.Flock(int(l.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		l.Close()
		return nil, errors.New("另一个 Guard 进程已占用状态目录")
	}
	s := &Store{dir: dir, lock: l, State: State{Version: 1, Records: map[int64]*Record{}, Events: []Event{}}}
	raw, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err == nil {
		if json.Unmarshal(raw, &s.State) != nil || s.State.Version != 1 || s.State.Records == nil {
			s.Close()
			return nil, errors.New("状态文件损坏；保留原文件并停止监测")
		}
	} else if !os.IsNotExist(err) {
		s.Close()
		return nil, errors.New("无法读取状态文件")
	}
	return s, nil
}
func (s *Store) Close() {
	if s.lock != nil {
		_ = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
		_ = s.lock.Close()
		s.lock = nil
	}
}
func (s *Store) Save() error {
	b, err := json.MarshalIndent(s.State, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.dir, ".state-*")
	if err != nil {
		return errors.New("无法写入状态文件")
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err == nil {
		err = ce
	}
	if err != nil {
		return errors.New("状态写入未完成")
	}
	if err = os.Rename(name, filepath.Join(s.dir, "state.json")); err != nil {
		return errors.New("无法提交状态文件")
	}
	d, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func (s *Store) Event(id int64, action, reason string, now time.Time) {
	s.State.Events = append(s.State.Events, Event{now, id, action, reason})
	if len(s.State.Events) > 300 {
		s.State.Events = s.State.Events[len(s.State.Events)-300:]
	}
}
func (s *Store) ReserveRequest(now time.Time, limit int) error {
	day := now.UTC().Format("2006-01-02")
	if s.State.Day != day {
		s.State.Day = day
		s.State.Requests = 0
	}
	if s.State.Requests >= limit {
		return errors.New("今日探测请求预算已用完")
	}
	s.State.Requests++
	return s.Save()
}

// Three valid samples form ONE round. A confirmed mismatch pauses immediately.
// Unknown outcomes retain mismatch evidence but never count as a mismatch.
// Recovery requires two consecutive valid rounds plus an ownership check.
func Transition(r *Record, verdict, reason string, now time.Time, c Config) bool {
	r.CheckedAt = now
	r.Reason = reason
	r.NextAt = now.Add(time.Duration(c.NormalSeconds) * time.Second)
	if verdict == "match" {
		r.Failures = 0
		r.Unavailable = 0
		if r.Hold {
			r.RecoveryPasses++
			r.Status = "recovering"
			r.NextAt = now.Add(time.Duration(c.RetrySeconds) * time.Second)
			if r.RecoveryPasses >= 2 {
				r.Status = "recovery_ready"
				r.NextAt = now.Add(time.Duration(c.QuarantineSeconds) * time.Second)
			}
		} else {
			r.Status = "passed"
			r.RecoveryPasses = 0
		}
		return false
	}
	r.RecoveryPasses = 0
	r.NextAt = now.Add(time.Duration(c.RetrySeconds) * time.Second)
	if verdict == "mismatch" {
		r.Failures++
		r.Unavailable = 0
		r.Status = "suspect"
	} else {
		r.Unavailable++
		// Failure is not a quality verdict. Keep existing ownership, retry soon,
		// and never create a new quarantine, including with legacy opt-in configs.
		r.Status = "unavailable"
		return false
	}
	if r.Hold {
		r.Status = "quarantined"
		r.NextAt = now.Add(time.Duration(c.QuarantineSeconds) * time.Second)
		return false
	}
	threshold := verdict == "mismatch"
	if threshold {
		r.Status = "pause_recommended"
	}
	return threshold && c.AutoPause
}
