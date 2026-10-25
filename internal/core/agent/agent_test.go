package agent

import (
	"errors"
	"testing"
	"time"
)

type memorySecrets struct{ stored string }

func (m *memorySecrets) Put(_ string, credential string) (string, error) {
	m.stored = credential
	return "wincred://provider/account-1", nil
}

// Requirement: AGT-001
func TestAccountRegistrationSeparatesCredentialFromPersistedMetadata(t *testing.T) {
	store := &memorySecrets{}
	account, err := Register(Registration{ID: "account-1", Provider: "provider-a", Credential: "very-secret", MaximumDataClass: Internal}, store)
	if err != nil {
		t.Fatal(err)
	}
	if store.stored != "very-secret" || account.SecretRef != "wincred://provider/account-1" {
		t.Fatalf("secret was not delegated to secret store: %+v", account)
	}
	if account.Alias == "very-secret" || account.Provider == "very-secret" {
		t.Fatal("credential leaked into persisted metadata")
	}
}

func eligibleAccount(id string, priority int) Account {
	return Account{
		ID: id, Provider: "provider", Priority: priority, State: Active, BudgetAvailable: true,
		Capabilities: map[string]bool{"tools": true, "vision": true}, MaximumDataClass: Confidential,
	}
}

// Requirement: AGT-002
func TestRouterFiltersByStateCooldownCapabilityDataPolicyAndBudget(t *testing.T) {
	now := time.Now()
	valid := eligibleAccount("valid", 9)
	disabled := eligibleAccount("disabled", 1)
	disabled.State = Disabled
	cooling := eligibleAccount("cooling", 2)
	cooling.State = Cooldown
	cooling.CooldownUntil = now.Add(time.Minute)
	incapable := eligibleAccount("incapable", 3)
	delete(incapable.Capabilities, "vision")
	restricted := eligibleAccount("restricted", 4)
	restricted.MaximumDataClass = Public
	overBudget := eligibleAccount("budget", 5)
	overBudget.BudgetAvailable = false
	selected, err := (Router{}).Select(now, []Account{disabled, cooling, incapable, restricted, overBudget, valid}, RouteRequest{
		RequiredCapabilities: []string{"tools", "vision"}, DataClass: Internal,
	})
	if err != nil || selected.ID != "valid" {
		t.Fatalf("selected=%+v err=%v", selected, err)
	}
}

// Requirement: AGT-003
func TestRouterKeepsEligibleStickyAccountBeforePriority(t *testing.T) {
	accounts := []Account{eligibleAccount("primary", 1), eligibleAccount("sticky", 9)}
	selected, err := (Router{}).Select(time.Now(), accounts, RouteRequest{RequiredCapabilities: []string{"tools"}, StickyAccountID: "sticky"})
	if err != nil || selected.ID != "sticky" {
		t.Fatalf("selected=%+v err=%v", selected, err)
	}
}

// Requirement: AGT-004
func TestRateLimitPlacesAccountInCooldownAndAllowsFallback(t *testing.T) {
	now := time.Now()
	outcome := ApplyFailure(eligibleAccount("primary", 1), TemporaryRateLimit, now, 90*time.Second)
	if outcome.Account.State != Cooldown || !outcome.Account.CooldownUntil.Equal(now.Add(90*time.Second)) || !outcome.AllowFallback {
		t.Fatalf("outcome=%+v", outcome)
	}
}

// Requirement: AGT-005
func TestQuotaAndSpendFailuresDisableBudgetAndAllowFallback(t *testing.T) {
	for _, kind := range []FailureKind{QuotaFailure, SpendFailure} {
		outcome := ApplyFailure(eligibleAccount("primary", 1), kind, time.Now(), 0)
		if outcome.Account.BudgetAvailable || !outcome.AllowFallback ||
			(outcome.Account.State != QuotaExhausted && outcome.Account.State != SpendLimit) {
			t.Fatalf("kind=%s outcome=%+v", kind, outcome)
		}
	}
}

// Requirement: AGT-006
func TestInvalidRequestAndSafetyRefusalNeverTriggerAccountFallback(t *testing.T) {
	for _, kind := range []FailureKind{InvalidRequest, SafetyRefusal} {
		outcome := ApplyFailure(eligibleAccount("primary", 1), kind, time.Now(), 0)
		if outcome.AllowFallback || outcome.Account.State != Active {
			t.Fatalf("kind=%s outcome=%+v", kind, outcome)
		}
	}
}

// Requirement: AGT-007
func TestRouterReturnsAIUnavailableWhenNoAccountIsEligible(t *testing.T) {
	account := eligibleAccount("only", 1)
	account.State = Disabled
	_, err := (Router{}).Select(time.Now(), []Account{account}, RouteRequest{})
	if !errors.Is(err, ErrAIUnavailable) {
		t.Fatalf("got %v, want manual mode", err)
	}
}

// Requirement: AGT-008
func TestLogFieldRedactionRemovesCredentialsButKeepsUsageCounts(t *testing.T) {
	redacted := RedactFields(map[string]string{
		"Authorization": "Bearer secret", "Proxy-Authorization": "secret", "apikey": "secret", "refresh_token": "secret",
		"Cookie": "session=secret", "client_secret": "secret", "input_tokens": "42", "output_tokens": "9", "cached_tokens": "3", "request_id": "req-1",
	})
	for _, key := range []string{"Authorization", "Proxy-Authorization", "apikey", "refresh_token", "Cookie", "client_secret"} {
		if _, exists := redacted[key]; exists {
			t.Fatalf("%s survived allowlist: %+v", key, redacted)
		}
	}
	if redacted["input_tokens"] != "42" || redacted["output_tokens"] != "9" || redacted["cached_tokens"] != "3" || redacted["request_id"] != "req-1" {
		t.Fatalf("non-secret metadata was removed: %+v", redacted)
	}
}

// Requirement: AGT-010
func TestExpiredCooldownReturnsAccountToActiveRouting(t *testing.T) {
	now := time.Now()
	account := eligibleAccount("primary", 1)
	account.State = Cooldown
	account.CooldownUntil = now.Add(-time.Second)
	selected, err := (Router{}).Select(now, []Account{account}, RouteRequest{RequiredCapabilities: []string{"tools"}})
	if err != nil || selected.State != Active || selected.ID != account.ID {
		t.Fatalf("expired cooldown was not recovered: selected=%+v err=%v", selected, err)
	}
}

// Requirement: AGT-011
func TestRepeatedProviderOverloadEscalatesFromCooldownToUnhealthy(t *testing.T) {
	now := time.Now()
	account := eligibleAccount("primary", 1)
	for attempt := 1; attempt <= 3; attempt++ {
		outcome := ApplyFailure(account, Overloaded, now, time.Minute)
		account = outcome.Account
		if !outcome.AllowFallback {
			t.Fatalf("attempt %d blocked fallback", attempt)
		}
		if attempt < 3 {
			account = ReconcileCooldown(account, now.Add(2*time.Minute))
			if account.State != Active {
				t.Fatalf("attempt %d did not recover for limited retry: %+v", attempt, account)
			}
		}
	}
	if account.State != Unhealthy {
		t.Fatalf("repeated overload did not become unhealthy: %+v", account)
	}
}

// Requirement: AGT-012
func TestAgentToolCallsRequireStructuredSourceAndNarrowAllowlist(t *testing.T) {
	allowlist := map[string]bool{"render.create_rendition": true, "run_shell": true}
	if err := ValidateToolCall(StructuredToolCall, "render.create_rendition", allowlist); err != nil {
		t.Fatalf("allowed domain tool rejected: %v", err)
	}
	for _, attempt := range []struct {
		source ToolCallSource
		name   string
	}{{StructuredToolCall, "run_shell"}, {StructuredToolCall, "files.delete_arbitrary"}, {UntrustedContent, "render.create_rendition"}} {
		if err := ValidateToolCall(attempt.source, attempt.name, allowlist); !errors.Is(err, ErrUnsafeToolCall) {
			t.Fatalf("unsafe tool call accepted: %+v err=%v", attempt, err)
		}
	}
}

// Requirement: AGT-013
func TestFallbackContextRetriesOnlyIncompleteToolCallsAndUsesCompletedSummaries(t *testing.T) {
	context := BuildFallbackContext("JOB-1", "render", []ToolCallRecord{
		{ID: "call-complete", Completed: true, ResultSummary: "rendition ART-1 published"},
		{ID: "call-pending", Completed: false},
	})
	if context.CompletedSummaries["call-complete"] == "" || len(context.RetryToolCallIDs) != 1 || context.RetryToolCallIDs[0] != "call-pending" {
		t.Fatalf("unsafe fallback context: %+v", context)
	}
}

// Requirement: AGT-009
func TestAuthenticationFailureDisablesAccountAndRequiresOperatorNotice(t *testing.T) {
	outcome := ApplyFailure(eligibleAccount("primary", 1), AuthenticationFailure, time.Now(), 0)
	if outcome.Account.State != AuthFailed || !outcome.Account.RequiresOperatorNotice ||
		!outcome.AllowFallback || !outcome.RequiresOperator {
		t.Fatalf("authentication failure was hidden: %+v", outcome)
	}
}

// Requirement: AGT-014
func TestProviderSuccessResetsConsecutiveOverloadCount(t *testing.T) {
	now := time.Now()
	account := eligibleAccount("primary", 1)
	account = ApplyFailure(account, Overloaded, now, time.Minute).Account
	account = ReconcileCooldown(account, now.Add(2*time.Minute))
	account = RecordSuccess(account, now.Add(3*time.Minute))
	if account.State != Active || account.ConsecutiveOverloads != 0 || !account.LastSuccessAt.Equal(now.Add(3*time.Minute)) {
		t.Fatalf("successful call did not reset overload sequence: %+v", account)
	}
	account = ApplyFailure(account, Overloaded, now.Add(4*time.Minute), time.Minute).Account
	if account.State != Cooldown || account.ConsecutiveOverloads != 1 {
		t.Fatalf("old overloads leaked into new sequence: %+v", account)
	}
}

// Requirement: AGT-015
func TestUnhealthyAccountRecoversOnlyAfterSuccessfulHealthProbe(t *testing.T) {
	now := time.Now()
	account := eligibleAccount("primary", 1)
	account.State = Unhealthy
	account.ConsecutiveOverloads = 3
	if recovered := RecoverUnhealthy(account, false, now); recovered.State != Unhealthy {
		t.Fatalf("failed probe recovered account: %+v", recovered)
	}
	recovered := RecoverUnhealthy(account, true, now)
	if recovered.State != Active || recovered.ConsecutiveOverloads != 0 || !recovered.LastSuccessAt.Equal(now) {
		t.Fatalf("successful probe did not recover account: %+v", recovered)
	}
}
