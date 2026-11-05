package guard

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

const (
	auditRetention    = 2000
	auditRotateEvents = 4000
	auditRotateBytes  = 8 << 20
	activityRetention = 300
	budgetBlock       = 16
)

type StoreMetrics struct {
	StateWrites  uint64 `json:"state_writes"`
	AuditWrites  uint64 `json:"audit_writes"`
	BudgetWrites uint64 `json:"budget_writes"`
}
type Store struct {
	dir            string
	lock           *os.File
	State          State
	pendingAudit   []Event
	auditRepairTo  *int64
	auditCount     int
	auditBytes     int64
	budgetDay      string
	budgetReserved int
	budgetDirty    bool
	metrics        StoreMetrics
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
	s := &Store{dir: dir, lock: l, State: State{Version: 1, Records: map[int64]*Record{}}}
	fail := func(err error) (*Store, error) {
		s.unlock()
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err == nil {
		if json.Unmarshal(raw, &s.State) != nil || s.State.Version != 1 || s.State.Records == nil || s.State.Requests < 0 {
			message := "状态文件损坏；保留原文件并停止监测"
			if previous, readErr := os.ReadFile(filepath.Join(dir, "state.json.prev")); readErr == nil {
				var state State
				if json.Unmarshal(previous, &state) == nil && state.Version == 1 && state.Records != nil {
					message += "；state.json.prev 保留了上一版，可核对后手动恢复"
				}
			}
			return fail(errors.New(message))
		}
	} else if !os.IsNotExist(err) {
		return fail(errors.New("无法读取状态文件"))
	}
	legacy := append([]Event(nil), s.State.Events...)
	s.State.Events, s.State.AuditEvents, s.State.ActivityEvents = nil, nil, nil
	for _, r := range s.State.Records {
		if r == nil {
			return fail(errors.New("状态文件包含空账号记录；保留原文件并停止监测"))
		}
		migrateStatus(r)
	}
	journalExists := false
	for _, name := range []string{"audit.jsonl.1", "audit.jsonl"} {
		exists, count, size, readErr := s.readAudit(name)
		if readErr != nil {
			return fail(readErr)
		}
		journalExists = journalExists || exists
		if name == "audit.jsonl" {
			s.auditCount, s.auditBytes = count, size
		}
	}
	if !journalExists {
		for _, event := range legacy {
			s.appendEvent(event, isAudit(event.Action))
			if isAudit(event.Action) {
				s.pendingAudit = append(s.pendingAudit, event)
			}
		}
	} else {
		s.State.Events = append([]Event(nil), s.State.AuditEvents...)
		trimEvents(&s.State.Events, activityRetention)
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "budget.json")); err == nil {
		var budget struct {
			Day             string `json:"day"`
			ReservedThrough int    `json:"reserved_through"`
		}
		if json.Unmarshal(raw, &budget) != nil || budget.Day == "" || budget.ReservedThrough < 0 {
			return fail(errors.New("预算文件损坏；保留原文件并停止监测，避免重复使用已预留额度"))
		}
		s.budgetDay, s.budgetReserved = budget.Day, budget.ReservedThrough
		if budget.Day == time.Now().UTC().Format("2006-01-02") {
			if s.State.Day != budget.Day {
				s.State.Day, s.State.Requests = budget.Day, 0
			}
			if budget.ReservedThrough > s.State.Requests {
				s.State.Requests = budget.ReservedThrough
			}
		}
	} else if !os.IsNotExist(err) {
		return fail(errors.New("无法读取预算文件"))
	}
	return s, nil
}

func (s *Store) unlock() {
	if s.lock != nil {
		_ = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
		_ = s.lock.Close()
		s.lock = nil
	}
}

func (s *Store) Close() {
	if s.lock == nil {
		return
	}
	// Clean shutdown narrows the conservative request lease only after actual
	// usage is durable. A failed flush leaves the larger on-disk bound intact.
	if s.budgetDirty {
		if s.Save() == nil {
			_ = s.writeBudget(s.State.Day, s.State.Requests)
		}
	}
	s.unlock()
}

func (s *Store) Save() error {
	if err := s.flushAudit(); err != nil {
		return err
	}
	current := s.State
	current.Events, current.AuditEvents, current.ActivityEvents = nil, nil, nil
	b, err := json.Marshal(current)
	if err != nil {
		return err
	}
	if err = s.atomicFile("state.json", b, true); err != nil {
		return err
	}
	s.metrics.StateWrites++
	return nil
}

// atomicFile commits a synced temporary file and directory. A hard-link backup
// retains the previous valid state without copying it on the normal filesystem.
func (s *Store) atomicFile(name string, b []byte, previous bool) error {
	f, err := os.CreateTemp(s.dir, "."+name+"-*")
	if err != nil {
		return errors.New("无法写入状态文件")
	}
	temp := f.Name()
	defer os.Remove(temp)
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return errors.New("状态写入未完成")
	}
	path := filepath.Join(s.dir, name)
	if previous {
		backup := temp + ".prev"
		defer os.Remove(backup)
		if err := os.Link(path, backup); err == nil {
			if err := os.Rename(backup, path+".prev"); err != nil {
				return errors.New("无法保留上一版状态")
			}
		} else if !os.IsNotExist(err) {
			old, readErr := os.ReadFile(path)
			if readErr != nil {
				return errors.New("无法读取上一版状态")
			}
			if err := s.atomicFile(name+".prev", old, false); err != nil {
				return err
			}
		}
	}
	if err = os.Rename(temp, path); err != nil {
		return errors.New("无法提交状态文件")
	}
	d, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func isAudit(action string) bool {
	switch action {
	case "sample_started", "sample_finished", "sample_retry", "check":
		return false
	default:
		return true
	}
}
func trimEvents(events *[]Event, keep int) {
	if len(*events) > keep {
		copy(*events, (*events)[len(*events)-keep:])
		*events = (*events)[:keep]
	}
}
func (s *Store) appendEvent(event Event, audit bool) {
	s.State.Events = append(s.State.Events, event)
	trimEvents(&s.State.Events, activityRetention)
	if audit {
		s.State.AuditEvents = append(s.State.AuditEvents, event)
		trimEvents(&s.State.AuditEvents, auditRetention)
	} else {
		s.State.ActivityEvents = append(s.State.ActivityEvents, event)
		trimEvents(&s.State.ActivityEvents, activityRetention)
	}
}
func (s *Store) Event(id int64, action, reason string, now time.Time) {
	event := Event{At: now, AccountID: id, Action: action, Reason: reason}
	audit := isAudit(action)
	s.appendEvent(event, audit)
	if audit {
		s.pendingAudit = append(s.pendingAudit, event)
	}
}

func (s *Store) readAudit(name string) (bool, int, int64, error) {
	f, err := os.Open(filepath.Join(s.dir, name))
	if os.IsNotExist(err) {
		return false, 0, 0, nil
	}
	if err != nil {
		return false, 0, 0, errors.New("无法读取审计日志")
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return true, 0, 0, err
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	count := 0
	for scanner.Scan() {
		var event Event
		if json.Unmarshal(scanner.Bytes(), &event) != nil || event.Action == "" {
			return true, count, stat.Size(), fmt.Errorf("审计日志 %s 损坏；保留原文件以便核对", name)
		}
		s.State.AuditEvents = append(s.State.AuditEvents, event)
		trimEvents(&s.State.AuditEvents, auditRetention)
		count++
	}
	if scanner.Err() != nil {
		return true, count, stat.Size(), errors.New("无法完整读取审计日志")
	}
	return true, count, stat.Size(), nil
}
func (s *Store) flushAudit() error {
	if len(s.pendingAudit) == 0 {
		return nil
	}
	if s.auditCount >= auditRotateEvents || s.auditBytes >= auditRotateBytes {
		if err := os.Rename(filepath.Join(s.dir, "audit.jsonl"), filepath.Join(s.dir, "audit.jsonl.1")); err != nil {
			return errors.New("无法轮转审计日志")
		}
		s.auditCount, s.auditBytes = 0, 0
	}
	f, err := os.OpenFile(filepath.Join(s.dir, "audit.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return errors.New("无法写入审计日志")
	}
	if s.auditRepairTo != nil {
		if err := f.Truncate(*s.auditRepairTo); err != nil {
			f.Close()
			return errors.New("审计日志待修复写入失败")
		}
		if err := f.Sync(); err != nil {
			f.Close()
			return errors.New("审计日志待修复写入未完成")
		}
		s.auditRepairTo = nil
	}
	before, err := f.Stat()
	if err != nil {
		f.Close()
		return errors.New("无法读取审计日志写入位置")
	}
	var data bytes.Buffer
	for _, event := range s.pendingAudit {
		raw, err := json.Marshal(event)
		if err != nil {
			f.Close()
			return err
		}
		data.Write(raw)
		data.WriteByte('\n')
	}
	n, err := f.Write(data.Bytes())
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil || n != data.Len() {
		// Keep the original committed boundary. The next durability retry
		// truncates an ambiguous partial append before writing the same events.
		offset := before.Size()
		s.auditRepairTo = &offset
		return errors.New("审计日志写入未完成")
	}
	s.auditCount += len(s.pendingAudit)
	s.auditBytes += int64(n)
	s.pendingAudit = nil
	s.metrics.AuditWrites++
	return nil
}

// ReserveRequest uses a durable high-water mark in a small separate file.
// A crash may conservatively consume at most budgetBlock-1 unused requests,
// but can never replay spent quota. Sample activity does not rewrite state.json.
func (s *Store) ReserveRequest(now time.Time, limit int) error {
	day := now.UTC().Format("2006-01-02")
	if s.State.Day != day {
		s.State.Day, s.State.Requests = day, 0
	}
	if s.State.Requests >= limit {
		return errors.New("今日探测请求预算已用完")
	}
	next := s.State.Requests + 1
	if s.budgetDay != day || next > s.budgetReserved {
		through := min(limit, next+budgetBlock-1)
		if err := s.writeBudget(day, through); err != nil {
			return err
		}
	}
	s.State.Requests = next
	s.budgetDirty = true
	return nil
}
func (s *Store) writeBudget(day string, through int) error {
	raw, _ := json.Marshal(struct {
		Day             string `json:"day"`
		ReservedThrough int    `json:"reserved_through"`
	}{day, through})
	if err := s.atomicFile("budget.json", raw, false); err != nil {
		return err
	}
	s.budgetDay, s.budgetReserved = day, through
	s.metrics.BudgetWrites++
	return nil
}

// snapshotState is called under the engine lock. Analyses are immutable after
// publication; records, slices and manual progress must be copied.
func (s *Store) snapshotState() State {
	state := s.State
	state.Records = make(map[int64]*Record, len(s.State.Records))
	for id, record := range s.State.Records {
		r := *record
		state.Records[id] = &r
	}
	state.Events = append([]Event(nil), s.State.Events...)
	state.AuditEvents = append([]Event(nil), s.State.AuditEvents...)
	state.ActivityEvents = append([]Event(nil), s.State.ActivityEvents...)
	if state.Manual != nil {
		manual := *state.Manual
		manual.CurrentIDs = append([]int64(nil), manual.CurrentIDs...)
		state.Manual = &manual
	}
	sort.SliceStable(state.Events, func(i, j int) bool { return state.Events[i].At.Before(state.Events[j].At) })
	return state
}
