# 06. 에이전트와 보안 설계

## 1. 내부망 접근과 선택형 인증

로그인 기능은 처음부터 다음 두 모드의 정책 경계로 구현한다.

| 모드 | Dashboard 사용자 | 허용 범위 | 용도 |
|---|---|---|---|
| `disabled` | 단일 `internal-operator` | boot policy가 허용한 운영 기능 | 개발 및 신뢰된 소규모 사무실 운영 |
| `local` | 로그인한 operator/admin | 역할별 기능 | 사용자 구분·원격 관리가 필요한 운영 |

권장 설정 예시:

```yaml
security:
  auth_mode: disabled
  allowed_cidrs:
    - 192.168.40.0/24
  allowed_origins:
    - http://192.168.40.8:8080
  trusted_proxies: []
  mcp_listen_address: 127.0.0.1
```

`disabled`는 인증만 생략하며 접근 제어 전체를 끄는 옵션이 아니다.

- 내부 CIDR 밖의 요청은 로그인 화면도 제공하지 않고 거부한다. `NET-001`
- 사용자 계정과 작업 소유자를 만들지 않고 모든 UI Command actor를 `internal-operator`로 기록한다. source IP와 request ID는 진단 메타데이터이며 신원으로 사용하지 않는다. `NET-002`
- Provider 계정, Worker registry, 인증 설정, 보안 정책의 원격 화면/API는 등록하지 않는다. `CFG-001`, `SEC-004`
- 포맷·삭제·기존 아카이브 대체는 boot config에서 기능이 명시적으로 켜지고 대상 확인 절차를 통과한 경우만 실행한다. 별도 admin 사용자는 없다. `SEC-001`
- 상태 변경 HTTP 요청은 정확한 Origin, JSON Content-Type, cookie/header CSRF token을 요구한다. `NET-006`
- 인증 없는 MCP는 loopback에만 listen한다. `NET-005`
- Dashboard wildcard listen과 인증 없는 비-loopback MCP listen은 시작 시 거부한다. 파괴적 기능 플래그는 서로 독립적으로 설정한다. `CFG-004~005`

인증 모드는 Dashboard toggle로 제공하지 않는다. 무로그인 사용자가 스스로 인증을 끌 수 있기 때문이다. OS ACL로 보호된 로컬 설정 파일을 수정하고 서비스를 재시작해야 전환된다. `NET-004`

개발 단계의 기본 모드는 `disabled`다. 계정, Worker, storage, 보존 정책은 시작 전에 설정하고 실행 중에는 바꾸지 않는다. credential 원문은 설정 파일에 쓰지 않고 OS Secret Store reference만 둔다. `CFG-002~003`

## 2. Agent Gateway 위치

Dashboard 채팅은 Provider API를 직접 호출하지 않는다.

```text
Dashboard
→ Agent Gateway
→ Account Router
→ Provider Adapter
→ 외부 Provider

Agent Tool Call
→ MCP Client Adapter
→ 같은 Command Bus
→ Domain/Workflow
```

외부 Codex·Claude MCP client는 자체 계정으로 인증하며 서버가 해당 로그인 세션이나 브라우저 cookie를 저장하지 않는다.

## 3. 계정 저장

영속 DB에는 다음 메타데이터만 둔다.

```text
provider, alias, owner_scope, priority, enabled/state
secret_ref, allowed_capabilities, data_policy
default_model, cooldown_until, last_success/error
```

credential은 Windows Credential Manager 또는 DPAPI 기반 Secret Store에 저장한다. 등록 과정에서 credential을 Secret Store로 넘기고 Account에는 `secret_ref`만 반환한다. `AGT-001`

## 4. Router

필터 순서:

1. ACTIVE 및 cooldown 종료
2. 내부 budget 사용 가능
3. 필요한 tool/vision/context capability
4. 작업 데이터 등급 전송 허용
5. 대화 sticky 계정
6. 수치가 낮은 priority

필터 계약은 `AGT-002`, sticky 계약은 `AGT-003`이다.

## 5. 오류와 폴백

| 정규화 오류 | 같은 계정 재시도 | 다른 승인 후보 | 상태 |
|---|---:|---:|---|
| TEMPORARY_RATE_LIMIT | reset 후 | 가능 | COOLDOWN |
| OVERLOADED/5xx | 제한 backoff | 가능 | COOLDOWN/UNHEALTHY |
| QUOTA_EXHAUSTED | 불가 | 가능 | QUOTA_EXHAUSTED |
| SPEND_LIMIT | 불가 | 가능 | SPEND_LIMIT |
| AUTH_FAILURE | 재연결 전 불가 | 가능하되 관리자 알림 | AUTH_FAILED |
| INVALID_REQUEST | 불가 | 불가 | 요청 수정 필요 |
| SAFETY_REFUSAL | 불가 | 불가 | 우회 금지 |
| MCP DOMAIN FAILURE | Provider 대상 아님 | Provider 대상 아님 | Workflow 처리 |

`AGT-004~006`, `AGT-009~015`가 오류별 동작과 cooldown 복귀·반복 overload 격리·성공 시 연속 횟수 초기화·health probe 복구를 검증한다. UNHEALTHY 계정은 cooldown 시각만으로 복구하지 않는다. 사용 가능한 계정이 없으면 AI 사용 불가를 반환하고 나머지 업무 기능을 유지한다. `AGT-007`

## 6. Tool Call 안전성

- Provider에는 작업 상태에 필요한 tool allowlist만 제공한다.
- `run_shell`, 임의 경로 move/delete 같은 범용 도구는 제공하지 않는다.
- 상태 변경 Tool Call은 Command envelope와 idempotency key를 요구한다.
- timeout 후 폴백하기 전에 저장된 AgentToolCall과 실제 Job 상태를 확인한다.
- 완료 Tool Call은 새 Provider에서 다시 실행하지 않는다.
- 콘텐츠 안의 문장을 명령으로 해석하지 않는다.

구조화된 좁은 allowlist 검증은 `AGT-012`, 완료 호출을 요약으로만 전달하고 미완료 호출만 재시도하는 폴백 문맥은 `AGT-013`이 소유한다.

## 7. 권한

| 기능 | Agent | disabled internal-operator | local operator | local admin |
|---|---:|---:|---:|---:|
| read | 허용 | 허용 | 허용 | 허용 |
| process | 정책 허용 시 | 허용 | 허용 | 허용 |
| GATED(아카이브 확정 등) | 제안만 | 명시 확인 | 명시 승인 | 명시 승인 |
| destructive | 금지 | boot policy+대상 확인 | 요청만 | 별도 승인+대상 확인 |
| Provider·Worker·보안 설정 | 금지 | surface 없음 | 금지 | 허용 |

위 표의 담당자·관리자 구분은 향후 `local` 인증을 켰을 때 사용한다. `disabled`에서는 별도 admin Principal이 없고, 운영 명령은 `internal-operator + boot policy + 명시 확인`으로 판정한다. 관리 surface는 존재하지 않는다.

검증 계약은 `SEC-001~004`, 승인 gate는 `WFL-006`, 포맷 조건은 `DEV-004`다.

## 8. 데이터 반출

각 Agent 요청은 `public`, `internal`, `confidential` 데이터 등급을 가진다. Account의 허용 등급보다 높은 원문은 해당 Provider로 보내지 않는다. 새 Provider로 폴백할 때 전체 대화를 복사하지 않고 다음만 전달한다.

- 민감값이 제거된 구조화 요약
- 필요한 Job/Step 상태
- 완료된 Tool Call ID와 결과 요약
- 사용자가 허용한 전사 구간

## 9. 로그 정제

Agent 원장에는 request ID, 계정 ID, model, input/output/cached token, 가격 기준, 정규화 오류 등 허용 목록의 필드만 원문으로 저장한다. Authorization, Proxy-Authorization, API key, password, cookie, access/refresh token을 비롯한 나머지 필드는 제거한다. `AGT-008`

로그 화면은 raw Provider 오류 본문을 기본 노출하지 않고 정규화 코드, 시각, account alias와 추적 ID를 표시한다.
