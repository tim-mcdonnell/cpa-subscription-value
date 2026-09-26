package poll

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/tim-mcdonnell/cpa-subscription-value/internal/abi"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/domain"
	"github.com/tim-mcdonnell/cpa-subscription-value/internal/store"
)

// DefaultInterval is the per-account poll cadence. Neither endpoint is
// documented; community tools report their rate limits as "aggressive" and
// settle on 20–30 minutes, so do not go much lower.
const DefaultInterval = 20 * time.Minute

// MaxBackoff caps the delay after repeated 429/5xx answers.
const MaxBackoff = 6 * time.Hour

const unauthorizedLogEvery = time.Hour

// Host is the slice of the plugin host the poller uses; *abi.Host satisfies it.
type Host interface {
	AuthList() ([]abi.AuthFileEntry, error)
	AuthGet(authIndex string) (abi.AuthGetResponse, error)
	HTTPDo(abi.HTTPRequest) (abi.HTTPResponse, error)
}

// Status is stored under SettingKey(auth_index) after every poll attempt.
type Status struct {
	At         time.Time `json:"at"`
	OK         bool      `json:"ok"`
	Error      string    `json:"error,omitempty"`
	HTTPStatus int       `json:"http_status,omitempty"`
	Readings   int       `json:"readings"`
	NextAt     time.Time `json:"next_at"`
}

// SettingKey is where an account's last poll Status lives.
func SettingKey(authIndex string) string { return "last_poll_" + authIndex }

// RawSettingKey is where an account's last upstream body lives (≤ MaxRawBody).
func RawSettingKey(authIndex string) string { return "last_poll_raw_" + authIndex }

// AccountResult is one account's outcome within a PollOnce.
type AccountResult struct {
	AccountID int64           `json:"account_id"`
	AuthIndex string          `json:"auth_index"`
	Provider  domain.Provider `json:"provider"`
	Status
}

// Summary reports one PollOnce pass; accounts not yet due are not listed.
type Summary struct {
	Polled   int             `json:"polled"`
	OK       int             `json:"ok"`
	Failed   int             `json:"failed"`
	Readings int             `json:"readings"`
	Accounts []AccountResult `json:"accounts"`
}

type accountState struct {
	delay        time.Duration
	nextAt       time.Time
	lastAuthWarn time.Time
}

// Poller polls every known claude/codex account on its own schedule.
type Poller struct {
	st       *store.Store
	host     Host
	log      func(level, msg string, fields map[string]any)
	interval time.Duration

	now    func() time.Time
	jitter func(time.Duration) time.Duration

	pass  sync.Mutex // one PollOnce at a time
	mu    sync.Mutex // guards state
	state map[string]*accountState
}

// New builds a Poller; interval <= 0 means DefaultInterval. log may be nil.
func New(st *store.Store, host Host, log func(level, msg string, fields map[string]any), interval time.Duration) *Poller {
	if interval <= 0 {
		interval = DefaultInterval
	}
	if log == nil {
		log = func(string, string, map[string]any) {}
	}
	return &Poller{
		st:       st,
		host:     host,
		log:      log,
		interval: interval,
		now:      time.Now,
		jitter: func(d time.Duration) time.Duration {
			return d + time.Duration((rand.Float64()*0.2-0.1)*float64(d))
		},
		state: map[string]*accountState{},
	}
}

// Run polls until ctx is done, waking when the earliest account is due and
// at least once per interval to pick up newly seen accounts.
func (p *Poller) Run(ctx context.Context) {
	for {
		p.PollOnce(ctx)
		t := time.NewTimer(p.nextWake())
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

func (p *Poller) nextWake() time.Duration {
	now := p.now()
	wait := p.jitter(p.interval)
	p.mu.Lock()
	for _, s := range p.state {
		if d := s.nextAt.Sub(now); d < wait {
			wait = d
		}
	}
	p.mu.Unlock()
	return max(wait, time.Second)
}

// Delay is the account's current backoff delay (the interval when healthy).
func (p *Poller) Delay(authIndex string) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s := p.state[authIndex]; s != nil {
		return s.delay
	}
	return p.interval
}

// PollOnce polls every claude/codex account that is due and records readings
// and Status. Accounts come from the store (i.e. from usage events).
func (p *Poller) PollOnce(ctx context.Context) Summary {
	p.pass.Lock()
	defer p.pass.Unlock()
	var sum Summary
	accounts, err := p.st.ListAccounts()
	if err != nil {
		p.log("error", "poll: list accounts", map[string]any{"error": err.Error()})
		return sum
	}
	var due []domain.Account
	now := p.now()
	for _, a := range accounts {
		if (a.Provider == domain.ProviderClaude || a.Provider == domain.ProviderCodex) && p.due(a.AuthIndex, now) {
			due = append(due, a)
		}
	}
	p.fillIdentity(due)
	for _, a := range due {
		if ctx.Err() != nil {
			break
		}
		res := p.pollAccount(ctx, a)
		sum.Polled++
		if res.OK {
			sum.OK++
		} else {
			sum.Failed++
		}
		sum.Readings += res.Readings
		sum.Accounts = append(sum.Accounts, res)
	}
	return sum
}

// due reports whether an account should be polled now. An account first
// seen in this process resumes the schedule persisted in its Status, so a
// plugin restart does not poll early.
func (p *Poller) due(authIndex string, now time.Time) bool {
	p.mu.Lock()
	s := p.state[authIndex]
	p.mu.Unlock()
	if s == nil {
		s = &accountState{delay: p.interval}
		var last Status
		if ok, err := p.st.GetSetting(SettingKey(authIndex), &last); err == nil && ok && !last.NextAt.IsZero() {
			s.nextAt = last.NextAt
			s.delay = min(max(last.NextAt.Sub(last.At), p.interval), MaxBackoff)
		}
		p.mu.Lock()
		p.state[authIndex] = s
		p.mu.Unlock()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return !now.Before(s.nextAt)
}

// fillIdentity copies emails from host.auth.list onto accounts lacking one.
func (p *Poller) fillIdentity(accounts []domain.Account) {
	need := false
	for _, a := range accounts {
		need = need || a.Email == ""
	}
	if !need {
		return
	}
	files, err := p.host.AuthList()
	if err != nil {
		p.log("debug", "poll: auth list", map[string]any{"error": err.Error()})
		return
	}
	byIndex := make(map[string]abi.AuthFileEntry, len(files))
	for _, f := range files {
		byIndex[f.AuthIndex] = f
	}
	for i, a := range accounts {
		f, ok := byIndex[a.AuthIndex]
		if !ok || a.Email != "" || f.Email == "" {
			continue
		}
		accounts[i].Email = f.Email
		p.updateAccount(accounts[i])
	}
}

func (p *Poller) updateAccount(a domain.Account) {
	// a carries its stored first/last_seen, so a poll never moves last_seen (that tracks traffic).
	if _, err := p.st.UpsertAccount(a); err != nil {
		p.log("warn", "poll: update account", map[string]any{"auth_index": a.AuthIndex, "error": err.Error()})
	}
}

func (p *Poller) pollAccount(ctx context.Context, a domain.Account) AccountResult {
	now := p.now()
	res := AccountResult{AccountID: a.ID, AuthIndex: a.AuthIndex, Provider: a.Provider, Status: Status{At: now}}
	readings, raw, err := p.fetch(ctx, a, now)
	if raw.StatusCode != 0 {
		p.saveRaw(a.AuthIndex, now, raw)
	}
	res.HTTPStatus = raw.StatusCode

	p.mu.Lock()
	s := p.state[a.AuthIndex]
	fields := map[string]any{"auth_index": a.AuthIndex, "provider": string(a.Provider)}
	level, msg := "", ""
	switch {
	case err == nil:
		s.delay = p.interval
	case errors.Is(err, ErrBackoff):
		next := 2 * s.delay
		if se := (*StatusError)(nil); errors.As(err, &se) {
			next = max(next, se.RetryAfter)
		}
		s.delay = min(next, MaxBackoff)
		level, msg = "warn", "poll: upstream throttled, backing off"
		fields["delay"] = s.delay.String()
	case errors.Is(err, ErrUnauthorized):
		// CPA refreshes the credential itself; re-read it next interval.
		s.delay = p.interval
		if now.Sub(s.lastAuthWarn) >= unauthorizedLogEvery {
			s.lastAuthWarn = now
			level, msg = "warn", "poll: access token rejected; waiting for CPA to refresh it"
		}
	default:
		s.delay = p.interval
		level, msg = "warn", "poll: fetch failed"
	}
	s.nextAt = now.Add(p.jitter(s.delay))
	res.NextAt = s.nextAt
	p.mu.Unlock()

	if err != nil {
		res.Error = err.Error()
		if msg != "" {
			fields["error"] = res.Error
			if raw.StatusCode != 0 {
				fields["http_status"] = raw.StatusCode
			}
			p.log(level, msg, fields)
		}
	} else {
		res.OK = true
		for i := range readings {
			readings[i].AccountID = a.ID
			if err := p.st.InsertReading(&readings[i]); err != nil {
				res.OK, res.Error = false, err.Error()
				p.log("error", "poll: store reading", map[string]any{"auth_index": a.AuthIndex, "error": res.Error})
				break
			}
			res.Readings++
		}
		if raw.PlanType != "" && raw.PlanType != a.PlanType {
			a.PlanType = raw.PlanType
			p.updateAccount(a)
		}
	}
	if err := p.st.SetSetting(SettingKey(a.AuthIndex), res.Status); err != nil {
		p.log("error", "poll: save status", map[string]any{"auth_index": a.AuthIndex, "error": err.Error()})
	}
	return res
}

// fetch reads the credential and calls the provider endpoint. The token
// stays inside this function.
func (p *Poller) fetch(ctx context.Context, a domain.Account, now time.Time) ([]domain.MeterReading, RawResult, error) {
	auth, err := p.host.AuthGet(a.AuthIndex)
	if err != nil {
		return nil, RawResult{}, errors.New("auth get: " + err.Error())
	}
	cred := parseCredential(auth.JSON)
	if cred.accessToken == "" {
		return nil, RawResult{}, errors.New("auth file has no access_token")
	}
	if a.Provider == domain.ProviderCodex {
		return FetchCodex(ctx, p.host, cred.accessToken, cred.accountID, now)
	}
	return FetchClaude(ctx, p.host, cred.accessToken, now)
}

func (p *Poller) saveRaw(authIndex string, now time.Time, raw RawResult) {
	v := struct {
		At time.Time `json:"at"`
		RawResult
		Body any `json:"body"`
	}{At: now, RawResult: raw, Body: string(raw.Body)}
	if !raw.Truncated && json.Valid(raw.Body) {
		v.Body = json.RawMessage(raw.Body)
	}
	if err := p.st.SetSetting(RawSettingKey(authIndex), v); err != nil {
		p.log("debug", "poll: save raw body", map[string]any{"auth_index": authIndex, "error": err.Error()})
	}
}
