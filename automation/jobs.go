package automation

import (
	"context"
	"encoding/base64"
	"sort"
	"strings"
	"sync"
	"time"

	"kiro-go/auth"
	"kiro-go/logger"
)

// Bulk automation jobs.
//
// A Job runs many account logins through a bounded worker pool so an operator can
// import a whole list of "email|password" lines at once with controllable
// concurrency (threads). Each account is one unit of work; workers pull from a
// queue, run LoginCodeBuddy, and on success persist the account + capture quota.
// Progress is tracked per-account and per-job so the dashboard can poll a live
// snapshot. Manual-assist accounts (2FA/CAPTCHA) park with their browser session
// open for the operator to finish, then finalize.
//
// Browser model: ONE Playwright browser per job (the Engine), and one ISOLATED
// BrowserContext per account (the Session). A context is its own cookie/storage
// jar, so concurrent account logins never share session state. This is the
// reference implementation's model and is lighter than a browser-per-worker.
//
// One job runs at a time per manager (the singleton). Concurrency within a job is
// the operator-chosen thread count, clamped to a sane range.

const (
	DefaultConcurrency = 3
	MinConcurrency     = 1
	MaxConcurrency     = 10

	// perAccountTimeout is the HARD cap on one account's automated login. The
	// safety net against the "stuck for hours" hang: a single CodeBuddy→Google
	// login (terms → google → email → password → region → CLI-authorize → token)
	// completes in well under two minutes when it works, so anything past this is
	// stuck and must be unwound so the worker frees its slot.
	perAccountTimeout = 3 * time.Minute

	// Live preview sizing. Each worker's page is screenshotted at a capped frame
	// rate (see runPreviewLoop) so the fleet streams a continuously-updating LIVE
	// view while total CPU stays bounded regardless of concurrency. JPEG at this
	// quality keeps each frame small; the dashboard scales the tile.
	previewQuality = 45
)

// AccountStatus is the per-account state within a job.
type AccountStatus string

const (
	AcctQueued      AccountStatus = "queued"
	AcctRunning     AccountStatus = "running"
	AcctNeedsManual AccountStatus = "needs_manual"
	AcctSuccess     AccountStatus = "success"
	AcctFailed      AccountStatus = "failed"
	AcctInvalid     AccountStatus = "failed_invalid_credentials"
	AcctCancelled   AccountStatus = "cancelled"
)

func isTerminal(s AccountStatus) bool {
	switch s {
	case AcctSuccess, AcctFailed, AcctInvalid, AcctCancelled:
		return true
	}
	return false
}

// AccountJob is one account's progress through a job.
type AccountJob struct {
	Line     int           `json:"line"`
	Email    string        `json:"email"`
	Status   AccountStatus `json:"status"`
	Step     string        `json:"step"`
	Message  string        `json:"message"`
	AcctID   string        `json:"accountId,omitempty"`
	QuotaMsg string        `json:"quota,omitempty"`
	WorkerID int           `json:"workerId,omitempty"`
	manualMu sync.Mutex    `json:"-"`

	// log is the per-account step history (bounded) so the operator can see what
	// happened to each email, especially failures.
	log []LogEntry

	// session is the isolated browser context+page for THIS account while it's
	// running or parked for manual completion. Used for the preview frame and (on
	// manual) for the operator to finish by hand. Guarded by manualMu.
	session       *Session
	manualResolve chan struct{} // closed when operator marks complete or job cancels

	// preview is the latest captured JPEG (data URL) of THIS account's browser,
	// so the dashboard can show one live frame per concurrent worker. Guarded by
	// manualMu.
	preview string
}

// LogEntry is one timestamped step in an account's history.
type LogEntry struct {
	At      time.Time `json:"at"`
	Step    string    `json:"step"`
	Message string    `json:"message"`
}

const maxLogEntries = 50

// Job is a bulk import run.
type Job struct {
	ID          string        `json:"id"`
	Backend     string        `json:"backend"`
	Concurrency int           `json:"concurrency"`
	Headless    bool          `json:"headless"`
	ProxyURL    string        `json:"proxyURL,omitempty"`
	CreatedAt   time.Time     `json:"createdAt"`
	Accounts    []*AccountJob `json:"accounts"`

	engine    *Engine
	cancel    context.CancelFunc
	cancelled bool
	done      bool

	manualWg sync.WaitGroup
	mu       sync.Mutex
}

// Preview is one live browser frame (one per concurrent worker).
type Preview struct {
	Line      int       `json:"line"`
	Email     string    `json:"email"`
	WorkerID  int       `json:"workerId"`
	Status    string    `json:"status"`
	Step      string    `json:"step"`
	UpdatedAt time.Time `json:"updatedAt"`
	ImageData string    `json:"imageData"` // data:image/jpeg;base64,...
}

// JobSnapshot is the JSON-safe view returned to the dashboard.
type JobSnapshot struct {
	ID          string               `json:"id"`
	Backend     string               `json:"backend"`
	Concurrency int                  `json:"concurrency"`
	CreatedAt   time.Time            `json:"createdAt"`
	Done        bool                 `json:"done"`
	Cancelled   bool                 `json:"cancelled"`
	Summary     map[string]int       `json:"summary"`
	Failures    []AccountJobSnapshot `json:"failures"` // failed/invalid accounts, for at-a-glance triage
	Accounts    []AccountJobSnapshot `json:"accounts"`
	Previews    []Preview            `json:"previews"` // one live frame per active worker
}

type AccountJobSnapshot struct {
	Line     int           `json:"line"`
	Email    string        `json:"email"`
	Status   AccountStatus `json:"status"`
	Step     string        `json:"step"`
	Message  string        `json:"message"`
	AcctID   string        `json:"accountId,omitempty"`
	QuotaMsg string        `json:"quota,omitempty"`
	WorkerID int           `json:"workerId,omitempty"`
	Log      []LogEntry    `json:"log,omitempty"`
}

// Manager owns the singleton job. Construct via NewManager / use the package
// singleton through GetManager (wired by the proxy handler).
type Manager struct {
	mu      sync.Mutex
	current *Job
}

var (
	singleton     *Manager
	singletonOnce sync.Once
)

// GetManager returns the process-wide automation job manager.
func GetManager() *Manager {
	singletonOnce.Do(func() { singleton = &Manager{} })
	return singleton
}

// splitCredential splits one "email<sep>password" line on the FIRST delimiter it
// finds, where the delimiter can be any of the common ones operators paste:
// pipe, colon, comma, semicolon, tab, or whitespace. Everything before the
// delimiter is the email; everything after (verbatim, so passwords may contain
// further delimiters) is the password. Returns ok=false when the line has no
// usable split or an empty half.
//
// This makes bulk import tolerant of mixed formats — "a@b.com|pw", "a@b.com:pw",
// "a@b.com,pw", "a@b.com pw" all work — so a paste from any source proceeds
// instead of being rejected. The email is detected as the run up to the first
// delimiter; we pick the EARLIEST occurring delimiter so "user:name@x|pw" (colon
// inside isn't the intended split) still favors whatever the operator used
// first... but to avoid splitting on a colon that's part of the password we
// prefer pipe/tab/comma/semicolon over colon/space when several are present.
func splitCredential(line string) (email, password string, ok bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", "", false
	}
	// Preferred delimiters first (explicit field separators), then weaker ones.
	// For each tier, use the earliest occurrence in the line.
	tiers := [][]string{
		{"|", "\t", ";", ","},
		{":"},
		{" "},
	}
	for _, tier := range tiers {
		best := -1
		for _, d := range tier {
			if idx := strings.Index(line, d); idx >= 0 && (best == -1 || idx < best) {
				best = idx
			}
		}
		if best > 0 {
			// find the delimiter length at best (all are 1 byte here)
			email = strings.TrimSpace(line[:best])
			password = strings.TrimSpace(line[best+1:])
			if email != "" && password != "" {
				return email, password, true
			}
		}
	}
	return "", "", false
}

// ParseAccounts turns credential lines into AccountJobs, reporting the line
// numbers that were malformed. Blank lines are skipped. Any common delimiter is
// accepted (see splitCredential).
func ParseAccounts(lines []string) (accounts []*AccountJob, invalid []int) {
	for i, raw := range lines {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		email, _, ok := splitCredential(raw)
		if !ok {
			invalid = append(invalid, i+1)
			continue
		}
		accounts = append(accounts, &AccountJob{
			Line:   i + 1,
			Email:  email,
			Status: AcctQueued,
			Step:   "queued",
		})
	}
	return accounts, invalid
}

func clampConcurrency(n int) int {
	if n < MinConcurrency {
		return DefaultConcurrency
	}
	if n > MaxConcurrency {
		return MaxConcurrency
	}
	return n
}

// StartInput parameters a new bulk job.
type StartInput struct {
	Backend     string
	Concurrency int
	Headless    bool
	ProxyURL    string
	// Lines are "email|password" rows.
	Lines []string
}

// OnPersist is called by a job when an account login succeeds; it must persist
// the credentials and return the new account id (and optional quota message). The
// proxy package supplies this so the automation package stays free of handler/
// pool dependencies.
type OnPersist func(ctx context.Context, res CodeBuddyLoginResult, email string) (acctID string, quotaMsg string, err error)

// Start launches a new job, returning its id. Errors if a job is already running.
func (m *Manager) Start(in StartInput, persist OnPersist) (*Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current != nil && !m.current.done {
		return nil, errJobRunning
	}

	accounts, _ := ParseAccounts(in.Lines)
	// Build password map keyed by line, using the SAME delimiter detection as
	// ParseAccounts so the two never disagree.
	pw := map[int]string{}
	for i, raw := range in.Lines {
		if _, password, ok := splitCredential(raw); ok {
			pw[i+1] = password
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	job := &Job{
		ID:          auth.GenerateAccountID(),
		Backend:     in.Backend,
		Concurrency: clampConcurrency(in.Concurrency),
		Headless:    in.Headless,
		ProxyURL:    in.ProxyURL,
		CreatedAt:   time.Now(),
		Accounts:    accounts,
		cancel:      cancel,
	}
	m.current = job

	go job.run(ctx, pw, persist)
	return job, nil
}

var errJobRunning = &jobErr{"an automation job is already running"}

type jobErr struct{ msg string }

func (e *jobErr) Error() string { return e.msg }

// run executes the job's accounts through a bounded worker pool.
func (j *Job) run(ctx context.Context, passwords map[int]string, persist OnPersist) {
	defer func() {
		j.mu.Lock()
		j.done = true
		eng := j.engine
		j.engine = nil
		j.mu.Unlock()
		if eng != nil {
			eng.Close()
		}
		logger.Infof("[Automation] job %s finished", j.ID)
	}()

	// Launch ONE browser for the whole job. Each account gets its own isolated
	// context off this browser. If the browser can't start (e.g. Playwright not
	// installed), fail every account with a clear message instead of hanging.
	eng, err := StartEngine(EngineOptions{Headless: j.Headless, ProxyURL: j.ProxyURL})
	if err != nil {
		logger.Errorf("[Automation] job %s could not start browser: %v", j.ID, err)
		for _, a := range j.Accounts {
			j.setStatus(a, AcctFailed, "browser_unavailable", "Could not start browser: "+err.Error())
		}
		return
	}
	j.mu.Lock()
	j.engine = eng
	j.mu.Unlock()

	// Live preview capturer: one throttled JPEG of whichever accounts are currently
	// running/awaiting-manual. Runs for the life of the job and stops when ctx is
	// cancelled. Single capturer (not per-account) bounds CPU.
	previewCtx, stopPreview := context.WithCancel(ctx)
	defer stopPreview()
	go j.runPreviewLoop(previewCtx)

	// Bounded worker pool. Each slot index doubles as a stable worker id for display.
	slots := make(chan int, j.Concurrency)
	for i := 1; i <= j.Concurrency; i++ {
		slots <- i
	}
	var wg sync.WaitGroup

	for _, acct := range j.Accounts {
		if ctx.Err() != nil {
			j.setStatus(acct, AcctCancelled, "cancelled", "Job cancelled")
			continue
		}
		workerID := <-slots
		wg.Add(1)
		go func(a *AccountJob, wid int) {
			defer wg.Done()
			defer func() { slots <- wid }()
			j.processAccount(ctx, a, passwords[a.Line], wid, persist)
		}(acct, workerID)
	}
	wg.Wait()
	// Manual-assist accounts run their wait OFF the worker pool (so they don't
	// starve concurrency); block here until every parked session resolves.
	j.manualWg.Wait()
}

// processAccount runs one account login and persists on success. A needs-manual
// result is detached so the worker slot is freed immediately (the operator may
// take minutes to finish a 2FA challenge); the detached wait is tracked by
// j.manualWg so run() still blocks for it.
func (j *Job) processAccount(ctx context.Context, a *AccountJob, password string, workerID int, persist OnPersist) {
	if ctx.Err() != nil {
		j.setStatus(a, AcctCancelled, "cancelled", "Job cancelled")
		return
	}
	a.mu(func() { a.WorkerID = workerID })
	j.setStatus(a, AcctRunning, "starting", "Starting login")

	onStep := func(step, msg string) { j.setStep(a, step, msg) }

	// HARD per-account timeout. This is the safety net against the "stuck for
	// hours" hang: even if a call inside the login loop blocks past its own
	// timeout, this context cancellation unwinds the whole attempt.
	acctCtx, cancelAcct := context.WithTimeout(ctx, perAccountTimeout)
	defer cancelAcct()

	res := LoginCodeBuddy(acctCtx, CodeBuddyLoginInput{
		Engine:      j.engine,
		Email:       a.Email,
		Password:    password,
		Backend:     j.Backend,
		Fingerprint: NewFingerprint(),
		OnSession: func(s *Session) {
			a.mu(func() { a.session = s })
		},
	}, onStep)

	switch res.Status {
	case StatusSuccess:
		j.finalizeSuccess(ctx, a, res, persist)
		j.clearSession(a)
	case StatusNeedsManual:
		j.manualWg.Add(1)
		go func() {
			defer j.manualWg.Done()
			j.parkManual(ctx, a, res, persist)
		}()
	case StatusInvalidCredentials:
		j.setStatus(a, AcctInvalid, "invalid_credentials", "Invalid email or password")
		j.clearSession(a)
	case StatusCancelled:
		j.setStatus(a, AcctCancelled, "cancelled", "Cancelled")
		j.clearSession(a)
	default:
		j.setStatus(a, AcctFailed, "failed", orDefault(res.Error, "Login failed"))
		j.clearSession(a)
	}
}

// clearSession closes and drops the live session for a finished account so the
// preview capturer stops touching it and the context's resources are freed.
func (j *Job) clearSession(a *AccountJob) {
	a.manualMu.Lock()
	s := a.session
	a.session = nil
	a.preview = ""
	a.manualMu.Unlock()
	if s != nil {
		s.Close()
	}
}

// finalizeSuccess persists the account and records quota.
func (j *Job) finalizeSuccess(ctx context.Context, a *AccountJob, res CodeBuddyLoginResult, persist OnPersist) {
	j.setStep(a, "saving", "Saving account")
	acctID, quotaMsg, err := persist(ctx, res, a.Email)
	if err != nil {
		j.setStatus(a, AcctFailed, "save_failed", "Login ok but save failed: "+err.Error())
		return
	}
	a.mu(func() {
		a.AcctID = acctID
		a.QuotaMsg = quotaMsg
	})
	j.setStatus(a, AcctSuccess, "done", "Account saved")
}

// parkManual records a needs-manual account and waits (the worker slot is already
// released by the caller's defer) for the operator to complete it. The session
// stays open until resolved or the job is cancelled.
func (j *Job) parkManual(ctx context.Context, a *AccountJob, res CodeBuddyLoginResult, persist OnPersist) {
	a.manualMu.Lock()
	a.session = res.Session
	a.manualResolve = make(chan struct{})
	resolve := a.manualResolve
	a.manualMu.Unlock()

	j.setStatus(a, AcctNeedsManual, "awaiting_manual", orDefault(res.Error, "Finish login in the browser, then mark complete"))

	// Wait for the operator to mark complete (CompleteManual closes resolve) or
	// the job to cancel. Bounded by the manual timeout.
	select {
	case <-resolve:
		// Operator finished: re-capture tokens/cookie from the live session.
		j.completeManualSession(ctx, a, res, persist)
	case <-ctx.Done():
		j.setStatus(a, AcctCancelled, "cancelled", "Job cancelled during manual completion")
	case <-time.After(DefaultManualTimeout):
		j.setStatus(a, AcctFailed, "manual_timeout", "Manual completion timed out")
	}

	a.manualMu.Lock()
	s := a.session
	a.session = nil
	a.manualMu.Unlock()
	if s != nil {
		s.Close()
	}
}

// completeManualSession captures the web cookie from a session the operator just
// finished by hand, then persists whatever token the login result carried.
func (j *Job) completeManualSession(ctx context.Context, a *AccountJob, res CodeBuddyLoginResult, persist OnPersist) {
	a.manualMu.Lock()
	s := a.session
	a.manualMu.Unlock()
	if s != nil {
		if cookie := captureWebCookie(ctx, s, func(step, m string) { j.setStep(a, step, m) }); cookie != "" {
			res.WebCookie = cookie
		}
	}
	if res.AccessToken == "" {
		j.setStatus(a, AcctFailed, "no_token", "Manual login finished but no access token was captured")
		return
	}
	j.finalizeSuccess(ctx, a, res, persist)
}

// CompleteManual signals that the operator finished a needs-manual account.
func (j *Job) CompleteManual(line int) bool {
	for _, a := range j.Accounts {
		if a.Line != line {
			continue
		}
		a.manualMu.Lock()
		ch := a.manualResolve
		a.manualResolve = nil
		a.manualMu.Unlock()
		if ch != nil {
			close(ch)
			return true
		}
		return false
	}
	return false
}

// Cancel stops the job and any pending manual waits.
func (j *Job) Cancel() {
	j.mu.Lock()
	j.cancelled = true
	cancel := j.cancel
	j.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (j *Job) setStatus(a *AccountJob, s AccountStatus, step, msg string) {
	a.mu(func() {
		a.Status = s
		a.Step = step
		a.Message = msg
		a.appendLogLocked(step, msg)
	})
}

func (j *Job) setStep(a *AccountJob, step, msg string) {
	a.mu(func() {
		// Don't overwrite a terminal status with an in-flight step.
		if !isTerminal(a.Status) {
			a.Step = step
			a.Message = msg
			a.appendLogLocked(step, msg)
		}
	})
}

// appendLogLocked adds a bounded log entry. Caller must hold manualMu.
func (a *AccountJob) appendLogLocked(step, msg string) {
	a.log = append(a.log, LogEntry{At: time.Now(), Step: step, Message: msg})
	if len(a.log) > maxLogEntries {
		a.log = a.log[len(a.log)-maxLogEntries:]
	}
}

// mu runs fn under the account's lock. (Small helper so callers don't repeat the
// lock/unlock dance.)
func (a *AccountJob) mu(fn func()) {
	a.manualMu.Lock()
	defer a.manualMu.Unlock()
	fn()
}

// runPreviewLoop streams a continuously-updating LIVE view of every active
// account's browser: each tick it screenshots each running/manual worker's page
// and publishes it as a data URL. Playwright's screenshot updates EVERY tick even
// when the page is static — so the operator can monitor a stuck page instead of
// staring at one frozen image. Captures run concurrently per tick so one slow page
// never stalls the others, and the tick rate is capped so total CPU stays bounded.
func (j *Job) runPreviewLoop(ctx context.Context) {
	// ~3 fps: smooth enough to read as "live", cheap enough for a 2–4 worker fleet.
	const interval = 350 * time.Millisecond
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		// Snapshot the live (account,session) set under lock, then capture OUTSIDE
		// the lock so a slow screenshot never blocks status updates.
		type shot struct {
			a       *AccountJob
			session *Session
		}
		var live []shot
		for _, a := range j.Accounts {
			a.manualMu.Lock()
			s := a.session
			st := a.Status
			a.manualMu.Unlock()
			if s != nil && (st == AcctRunning || st == AcctNeedsManual) {
				live = append(live, shot{a, s})
			}
		}
		var swg sync.WaitGroup
		for _, sh := range live {
			swg.Add(1)
			go func(sh shot) {
				defer swg.Done()
				// CREDENTIAL SAFETY: never screenshot on a Google domain — the password
				// field is visible there and a JPEG would leak the typed password into the
				// dashboard snapshot. Hold the previous frame on Google screens.
				if sh.session.OnGoogle() {
					return
				}
				img, err := Screenshot(sh.session.Page(), previewQuality)
				if err != nil || len(img) == 0 {
					return
				}
				data := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(img)
				sh.a.mu(func() { sh.a.preview = data })
			}(sh)
		}
		swg.Wait()
	}
}

// Snapshot returns the current job state for the dashboard.
func (m *Manager) Snapshot() *JobSnapshot {
	m.mu.Lock()
	job := m.current
	m.mu.Unlock()
	if job == nil {
		return nil
	}
	return job.snapshot()
}

// Current returns the running/last job, or nil.
func (m *Manager) Current() *Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current
}

func (j *Job) snapshot() *JobSnapshot {
	j.mu.Lock()
	done, cancelled := j.done, j.cancelled
	j.mu.Unlock()

	summary := map[string]int{}
	accts := make([]AccountJobSnapshot, 0, len(j.Accounts))
	failures := make([]AccountJobSnapshot, 0)
	previews := make([]Preview, 0)
	for _, a := range j.Accounts {
		a.manualMu.Lock()
		logCopy := make([]LogEntry, len(a.log))
		copy(logCopy, a.log)
		s := AccountJobSnapshot{
			Line: a.Line, Email: a.Email, Status: a.Status,
			Step: a.Step, Message: a.Message, AcctID: a.AcctID, QuotaMsg: a.QuotaMsg,
			WorkerID: a.WorkerID, Log: logCopy,
		}
		// One live frame per active worker (running or awaiting manual).
		if a.preview != "" && (a.Status == AcctRunning || a.Status == AcctNeedsManual) {
			previews = append(previews, Preview{
				Line: a.Line, Email: a.Email, WorkerID: a.WorkerID,
				Status: string(a.Status), Step: a.Step,
				UpdatedAt: time.Now(), ImageData: a.preview,
			})
		}
		a.manualMu.Unlock()
		summary[string(s.Status)]++
		accts = append(accts, s)
		if s.Status == AcctFailed || s.Status == AcctInvalid {
			failures = append(failures, s)
		}
	}
	summary["total"] = len(j.Accounts)
	sort.Slice(accts, func(i, k int) bool { return accts[i].Line < accts[k].Line })
	sort.Slice(failures, func(i, k int) bool { return failures[i].Line < failures[k].Line })
	sort.Slice(previews, func(i, k int) bool { return previews[i].WorkerID < previews[k].WorkerID })

	return &JobSnapshot{
		ID: j.ID, Backend: j.Backend, Concurrency: j.Concurrency,
		CreatedAt: j.CreatedAt, Done: done, Cancelled: cancelled,
		Summary: summary, Failures: failures, Accounts: accts, Previews: previews,
	}
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
