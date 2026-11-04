package archivist

import (
	"errors"
	"sort"
	"strings"
	"time"
)

type AccountState string

const (
	Active         AccountState = "ACTIVE"
	Throttled      AccountState = "THROTTLED"
	Cooldown       AccountState = "COOLDOWN"
	QuotaExhausted AccountState = "QUOTA_EXHAUSTED"
	SpendLimit     AccountState = "SPEND_LIMIT"
	AuthFailed     AccountState = "AUTH_FAILED"
	Unhealthy      AccountState = "UNHEALTHY"
	Disabled       AccountState = "DISABLED"
)

type DataClass int

const (
	Public DataClass = iota
	Internal
	Confidential
)

type Account struct {
	ID                   string
	Provider             string
	Alias                string
	SecretRef            string
	Priority             int
	State                AccountState
	CooldownUntil        time.Time
	Capabilities         map[string]bool
	MaximumDataClass     DataClass
	BudgetAvailable      bool
	ReconnectRequired    bool
	ConsecutiveOverloads int
	LastSuccessAt        time.Time
}

type SecretStore interface {
	Put(accountID, credential string) (secretRef string, err error)
}

type Registration struct {
	ID               string
	Provider         string
	Alias            string
	Credential       string
	Priority         int
	Capabilities     map[string]bool
	MaximumDataClass DataClass
}

func Register(registration Registration, secrets SecretStore) (Account, error) {
	if registration.ID == "" || registration.Provider == "" || registration.Credential == "" {
		return Account{}, errors.New("invalid account registration")
	}
	secretRef, err := secrets.Put(registration.ID, registration.Credential)
	if err != nil {
		return Account{}, err
	}
	return Account{
		ID: registration.ID, Provider: registration.Provider, Alias: registration.Alias,
		SecretRef: secretRef, Priority: registration.Priority, State: Active,
		Capabilities: registration.Capabilities, MaximumDataClass: registration.MaximumDataClass,
		BudgetAvailable: true,
	}, nil
}

var ErrAIUnavailable = errors.New("no eligible provider account; AI unavailable")

type RouteRequest struct {
	RequiredCapabilities []string
	DataClass            DataClass
	StickyAccountID      string
}

type Router struct{}

func (Router) Select(now time.Time, accounts []Account, request RouteRequest) (Account, error) {
	eligible := make([]Account, 0, len(accounts))
	for _, account := range accounts {
		account = ReconcileCooldown(account, now)
		if !isEligible(now, account, request) {
			continue
		}
		if account.ID == request.StickyAccountID {
			return account, nil
		}
		eligible = append(eligible, account)
	}
	if len(eligible) == 0 {
		return Account{}, ErrAIUnavailable
	}
	sort.SliceStable(eligible, func(i, j int) bool { return eligible[i].Priority < eligible[j].Priority })
	return eligible[0], nil
}

func ReconcileCooldown(account Account, now time.Time) Account {
	if account.State == Cooldown && !account.CooldownUntil.IsZero() && !now.Before(account.CooldownUntil) {
		account.State = Active
		account.CooldownUntil = time.Time{}
	}
	return account
}

// RecordSuccess ends an overload sequence. It does not recover states that
// require a budget change, credential repair or an explicit health probe.
func RecordSuccess(account Account, now time.Time) Account {
	if account.State == Active || account.State == Cooldown {
		account.State = Active
		account.CooldownUntil = time.Time{}
		account.ConsecutiveOverloads = 0
		account.LastSuccessAt = now
	}
	return account
}

func RecoverUnhealthy(account Account, healthProbePassed bool, now time.Time) Account {
	if account.State == Unhealthy && healthProbePassed {
		account.State = Active
		account.CooldownUntil = time.Time{}
		account.ConsecutiveOverloads = 0
		account.LastSuccessAt = now
	}
	return account
}

func isEligible(now time.Time, account Account, request RouteRequest) bool {
	if account.State != Active || !account.BudgetAvailable || now.Before(account.CooldownUntil) || request.DataClass > account.MaximumDataClass {
		return false
	}
	for _, capability := range request.RequiredCapabilities {
		if !account.Capabilities[capability] {
			return false
		}
	}
	return true
}

type FailureKind string

const (
	TemporaryRateLimit    FailureKind = "TEMPORARY_RATE_LIMIT"
	QuotaFailure          FailureKind = "QUOTA_EXHAUSTED"
	SpendFailure          FailureKind = "SPEND_LIMIT"
	AuthenticationFailure FailureKind = "AUTH_FAILURE"
	InvalidRequest        FailureKind = "INVALID_REQUEST"
	SafetyRefusal         FailureKind = "SAFETY_REFUSAL"
	Overloaded            FailureKind = "OVERLOADED"
)

type FailureOutcome struct {
	Account          Account
	AllowFallback    bool
	RequiresOperator bool
}

func ApplyFailure(account Account, kind FailureKind, now time.Time, retryAfter time.Duration) FailureOutcome {
	outcome := FailureOutcome{Account: account}
	switch kind {
	case TemporaryRateLimit:
		outcome.Account.State = Cooldown
		outcome.Account.CooldownUntil = now.Add(retryAfter)
		outcome.AllowFallback = true
	case Overloaded:
		outcome.Account.ConsecutiveOverloads++
		outcome.AllowFallback = true
		if outcome.Account.ConsecutiveOverloads >= 3 {
			outcome.Account.State = Unhealthy
			outcome.Account.CooldownUntil = time.Time{}
		} else {
			outcome.Account.State = Cooldown
			outcome.Account.CooldownUntil = now.Add(retryAfter)
		}
	case QuotaFailure:
		outcome.Account.State = QuotaExhausted
		outcome.Account.BudgetAvailable = false
		outcome.AllowFallback = true
	case SpendFailure:
		outcome.Account.State = SpendLimit
		outcome.Account.BudgetAvailable = false
		outcome.AllowFallback = true
	case AuthenticationFailure:
		outcome.Account.State = AuthFailed
		outcome.Account.ReconnectRequired = true
		outcome.AllowFallback = true
		outcome.RequiresOperator = true
	case InvalidRequest, SafetyRefusal:
		outcome.AllowFallback = false
	}
	return outcome
}

func RedactFields(fields map[string]string) map[string]string {
	allowed := map[string]bool{
		"request_id": true, "conversation_id": true, "archivist_turn_id": true,
		"provider_account_id": true, "model": true, "input_tokens": true,
		"output_tokens": true, "cached_tokens": true, "estimated_cost": true,
		"pricing_version": true, "occurred_at": true, "error_code": true,
	}
	redacted := make(map[string]string, len(allowed))
	for key, value := range fields {
		name := strings.ToLower(strings.ReplaceAll(key, "-", "_"))
		if allowed[name] {
			redacted[name] = value
		}
	}
	return redacted
}

var ErrUnsafeToolCall = errors.New("archivist tool call is not allowed")

type ToolCallSource string

const (
	StructuredToolCall ToolCallSource = "STRUCTURED_TOOL_CALL"
	UntrustedContent   ToolCallSource = "UNTRUSTED_CONTENT"
)

func ValidateToolCall(source ToolCallSource, toolName string, allowlist map[string]bool) error {
	if source != StructuredToolCall || toolName == "" || !allowlist[toolName] || toolName == "run_shell" {
		return ErrUnsafeToolCall
	}
	return nil
}

type ToolCallRecord struct {
	ID            string
	Completed     bool
	ResultSummary string
}

type FallbackContext struct {
	JobID              string
	StepID             string
	CompletedSummaries map[string]string
	RetryToolCallIDs   []string
}

func BuildFallbackContext(jobID, stepID string, calls []ToolCallRecord) FallbackContext {
	context := FallbackContext{JobID: jobID, StepID: stepID, CompletedSummaries: make(map[string]string)}
	for _, call := range calls {
		if call.Completed {
			context.CompletedSummaries[call.ID] = call.ResultSummary
		} else {
			context.RetryToolCallIDs = append(context.RetryToolCallIDs, call.ID)
		}
	}
	return context
}
