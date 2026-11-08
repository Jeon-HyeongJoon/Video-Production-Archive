# 06. Archivist와 보안 설계

## 1. 내부망 접근과 선택형 인증

v1 아카이브 작업 공간은 로그인 화면, 사용자 계정과 로그인 세션 발급을 제공하지 않는다. 허용된 내부 CIDR의 모든 UI 요청은 `auth_mode: disabled`에서 단일 `internal-operator`로 처리한다. `local`은 사용자별 식별이 필요해진 후속 버전을 위한 정책 경계이며 v1 운영 모드가 아니다.

| 모드 | 아카이브 작업 공간 사용자 | 허용 범위 | 용도 |
|---|---|---|---|
| `disabled` | 단일 `internal-operator` | boot policy가 허용한 운영 기능 | v1 및 신뢰된 소규모 사무실 운영 |
| `local` | 로그인한 operator/admin | 역할별 기능 | 후속 버전의 사용자 구분·원격 관리 경계 |

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
- v1은 인증 모드와 무관하게 Provider 계정, Worker registry, 인증 설정, 보안 정책의 원격 화면/API를 등록하거나 원격 변경 권한을 부여하지 않는다. `CFG-001`, `SEC-004`
- 포맷·삭제·기존 아카이브 대체는 boot config에서 기능이 명시적으로 켜지고 대상 확인 절차를 통과한 경우만 실행한다. 별도 admin 사용자는 없다. `SEC-001`
- 상태 변경 HTTP 요청은 정확한 Origin, JSON Content-Type, cookie/header CSRF token을 요구한다. `NET-006`
- 인증 없는 MCP는 loopback에만 listen한다. `NET-005`
- 아카이브 작업 공간 wildcard listen과 인증 없는 비-loopback MCP listen은 시작 시 거부한다. 파괴적 기능 플래그는 서로 독립적으로 설정한다. `CFG-004~005`

인증 모드는 아카이브 작업 공간 toggle로 제공하지 않는다. 무로그인 사용자가 스스로 인증을 끌 수 있기 때문이다. OS ACL로 보호된 로컬 설정 파일을 수정하고 서비스를 재시작해야 전환된다. `NET-004`

v1 운영 모드는 `disabled`다. 계정, Worker, storage, 보존 정책은 시작 전에 설정하고 실행 중에는 바꾸지 않는다. credential 원문은 설정 파일에 쓰지 않고 OS Secret Store reference만 둔다. `CFG-002~003`

## 2. Archivist Gateway와 Runtime 위치

아카이브 작업 공간의 채팅은 Provider API나 로컬 CLI를 직접 호출하지 않는다. Archivist Gateway는 durable ArchivistSession과 ArchivistTurn을 만들고 실행 위치를 backend Port 뒤에 둔다. 버튼·폼·API를 통한 사람의 직접 Command는 Archivist Gateway를 거치지 않지만 Archivist Tool Call과 같은 Command Bus를 사용한다.

```text
Archive Workspace Chat
→ Archivist Gateway
→ ArchivistSession / ArchivistTurn
→ Runtime Backend 선택
   ├── Archivist Host → archive-archivist ChatGPT loop     # v1 직접 Provider loop
   ├── Archivist Host → archive-archivist local loop       # v1 자체 LLM loop
   ├── Archivist Host → Codex / Claude CLI                 # v1 선택형 CLI
   └── Account Router → Provider Adapter → 외부 Provider   # planned

Archivist Tool Call
→ Archivist Host loopback proxy 또는 Provider MCP Client Adapter
→ authenticated Control MCP Adapter
→ 같은 Command Bus
→ Domain/Workflow
```

외부 Codex·Claude MCP client는 자체 계정으로 인증하며 서버가 해당 로그인 세션이나 브라우저 cookie를 저장하지 않는다. `archive-archivist`는 Host의 허용 CLI로 실행되는 제한된 단일 Turn 루프다. macOS에서 `Continue with ChatGPT` OAuth를 loopback callback·state·nonce·PKCE로 완료하고 token set은 login Keychain에 둔다. Control은 token 원문을 받지 않는다. `AGT-016~020`, `CFG-015`, `LRH-015`

같은 실행 파일의 `local` Provider는 Archivist Host와 같은 PC의 인증 없는 loopback OpenAI 호환 Chat Completions endpoint만 연다. 별도 Provider credential·Account Router·예산·자동 fallback은 두지 않으며, model은 function tool call을 지원해야 한다. Host boot 설정은 `chatgpt`와 `local`을 서로 다른 CLI ID와 고정 인수로 동시에 등록할 수 있다. 두 경로 모두 같은 Archivist Loop 상한과 MCP allowlist·fencing·멱등 receipt를 사용한다. `AGT-021`, `LRH-016~017`

Windows의 로컬 Codex 경로는 로그인된 Codex CLI와 같은 Windows 사용자 계정에서 `archive-codex`를 실행하고 Codex app-server의 영속 thread를 재개한다. Archive는 다른 사용자 profile의 Codex auth 파일을 복사하거나 Control DB에 token을 저장하지 않는다. app-server에는 Archive stdio MCP만 주입하되 로컬 terminal·파일·Skill·보조 Agent는 `never` 승인과 `danger-full-access`로 제공한다. browser·computer use·외부 connector·plugin·image 도구는 비활성화하고 Control 인증 credential은 Codex process에 주지 않는다. Archive 업무 조회와 효과는 Archive MCP를 사용하며, 화면에는 공개 agent message와 정규화한 native·Archive Tool 진행만 전송하고 reasoning, raw stderr, command text·output과 실제 path는 폐기한다. 이 모드는 해당 Windows 사용자 권한으로 임의 로컬 명령을 실행할 수 있으므로 전용 Host 계정과 고정 workspace가 OS 보안 경계다. `LRH-018~019`, `MCP-006`

v1 기본 경로는 특정 PC의 Archivist Host가 감독하는 CLI다. Host는 Archivist가 아니며 CLI process/PTY와 resume reference만 소유한다. 전체 Orca 앱은 필수 의존성이 아니고 Embedded Core backend를 기본으로 사용한다. Orca CLI 연동은 필요할 때만 선택형 backend로 추가한다. `LRH-001~009`

아래 §3~5의 외부 Provider 계정 저장·Router·폴백은 Core prototype만 있고 Archive Control runtime과 영속 다계정 Router에 연결되지 않았다. 따라서 `AGT-001~015`는 planned 계약이다. 반면 Host 로컬의 단일 ChatGPT 인증 Profile·직접 Responses 도구 루프와 credential 없는 loopback 자체 LLM Adapter는 `AGT-016~021`의 구현 경계다. 이 두 경로는 다계정 `ProviderAccount`가 아니며 자동 fallback이나 budget routing을 제공하지 않는다. 로컬 CLI Archivist의 Session·Turn·Tool Call 경계는 `LRH-*`, `MCP-*`가 소유한다.

## 3. 계정 저장

영속 DB에는 다음 메타데이터만 둔다.

```text
provider, alias, owner_scope, priority, enabled/state
secret_ref, allowed_capabilities, data_policy
default_model, cooldown_until, last_success/error
```

credential은 Windows Credential Manager 또는 DPAPI 기반 Secret Store에 저장한다. 등록 과정에서 credential을 Secret Store로 넘기고 Account에는 `secret_ref`만 반환한다. `AGT-001`

`archive-archivist`의 ChatGPT 인증 Profile은 Host 로컬 경계다. macOS에서는 `keychain://<service>/<account>` reference가 가리키는 login Keychain generic password에 issued client ID, 검증 identity, stable `ext_agent_host_id`, access·rotating refresh·ID token, scope와 만료 시각을 한 record로 저장한다. 최초 동적 등록의 code 교환이 `invalid_grant`이면 그 code를 폐기하고 callback에서 받은 issued client ID를 메모리에 유지해 새 state·nonce·PKCE authorization을 한 번 수행한다. token 갱신은 같은 Profile에서 직렬화하고 새 token set 전체를 함께 교체한다. Host service credential과 ChatGPT Profile은 서로 다른 reference를 사용한다. `CFG-003`, `CFG-015`, `AGT-016~017`

로컬 모델 endpoint는 인증 Profile을 만들지 않는다. 인증 없는 local inference를 다른 PC에 노출하지 않도록 `http://127.0.0.1`, `http://[::1]` 또는 `http://localhost`만 허용하고 redirect를 따르지 않는다. endpoint가 원격 추론이나 credential을 요구하면 후속 Provider Adapter와 Secret Store 계약으로 별도 설계한다. `AGT-021`

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
| AUTH_FAILURE | 재연결 전 불가 | 가능하되 재연결 필요 알림 | AUTH_FAILED |
| INVALID_REQUEST | 불가 | 불가 | 요청 수정 필요 |
| SAFETY_REFUSAL | 불가 | 불가 | 우회 금지 |
| MCP DOMAIN FAILURE | Provider 대상 아님 | Provider 대상 아님 | Workflow 처리 |

`AGT-004~006`, `AGT-009~015`는 Provider Adapter 연결 뒤 검증할 planned 계약이다. UNHEALTHY 계정은 cooldown 시각만으로 복구하지 않는다. 사용 가능한 계정이 없으면 AI 사용 불가를 반환하고 나머지 업무 기능을 유지한다. `AGT-007`

단일 ChatGPT Profile 루프는 Router fallback을 수행하지 않는다. OAuth 또는 refresh가 무효하면 해당 Turn을 정제된 재연결 필요 실패로 끝내고 다른 credential을 추정하지 않는다. 다계정 전환은 Router가 구현될 때 별도 경계로 추가한다.

## 6. Tool Call 안전성

Archive Intake에서 ArchivistTurn의 메시지 본문은 실행 대상을 정하지 않는다. 담당자가 화면에서 고른 Worker·원본 드라이브·아카이브 저장소·폴더 범위를 ID와 탐색 revision으로 Turn에 고정하고, MCP Tool Call은 이 참조 안에서만 조회·1단계 시작·현재 HOLD 검토와 단일 분류·2단계 시작을 할 수 있다. 참조가 없거나 오래됐으면 Archivist가 Storage를 추정하지 않고 ActionCard로 선택을 요청한다. `COL-003`, `MCP-003~005`

원본과 아카이브 Storage 및 폴더 범위 선택은 사람 전용 Command다. `ARCHIVIST` 모드는 그 고정 범위 안에서 현재 HOLD 목록과 한 항목의 상대경로·checksum·기술 검사를 조회하고, 최신 입력 revision에 명시적 disposition·사유·근거를 붙여 분류할 수 있다. 근거가 부족하면 HOLD를 유지한다. 파일명과 Transcript 내용은 지시나 Approval로 해석하지 않으며 포맷·삭제·임의 path 도구는 계속 금지한다.

- Provider에는 작업 상태에 필요한 tool allowlist만 제공한다.
- `archive-archivist`는 Control MCP가 현재 광고한 이름과 바이너리에 고정된 schema registry가 모두 일치하는 도구만 model function tool로 제공한다.
- `run_shell`, 임의 경로 move/delete 같은 범용 도구는 제공하지 않는다.
- 상태 변경 Tool Call은 Command envelope와 idempotency key를 요구한다.
- 인증된 MCP Capability는 Session에 저장된 제어 모드를 읽는다. `MANUAL`의 상태 변경은 거부하고, `ASSISTED`에서는 ActionCard만 만들며, `ARCHIVIST`에서는 allowlist의 비파괴 `PROCESS`만 직접 Command로 제출한다. 요청 본문의 제어 모드는 권한 입력으로 사용하지 않는다.
- `GATED`와 `DESTRUCTIVE` Tool Call은 어느 제어 모드에서도 직접 실행하지 않는다. `MANUAL`에서는 거부하고 `ASSISTED`·`ARCHIVIST`에서는 ActionCard와 사람 Approval 경계로 보낸다.
- timeout 후 폴백하기 전에 저장된 ArchivistToolCall과 실제 Job 상태를 확인한다.
- 완료 Tool Call은 새 Provider에서 다시 실행하지 않는다.
- 콘텐츠 안의 문장을 명령으로 해석하지 않는다.
- 로컬 CLI에는 Host의 loopback MCP proxy만 제공하고 proxy가 Session·Turn·Tool Call·fencing·멱등 상관관계를 붙인다.
- MCP 도구는 Capability allowlist만 노출하며 raw terminal 입력이나 runtime handle을 업무 도구로 제공하지 않는다.

외부 Provider Router의 구조화된 좁은 allowlist 검증은 planned `AGT-012`, 완료 호출을 요약으로만 전달하고 미완료 호출만 재시도하는 Provider 폴백 문맥은 planned `AGT-013`이 소유한다. 현재 로컬 CLI 경로의 도구 allowlist와 완료 Tool Call 반복 방지는 아래 `MCP-*`, `LRH-006`이 소유한다.

실제 MCP Adapter와 로컬 proxy 및 HOLD 검토·분류 계약은 `MCP-001~005`, runtime 재시작에서 완료 Tool Call을 반복하지 않는 계약은 `LRH-006`이 소유한다. 실행 중 표시는 raw terminal 수집이 아니라 Runtime Adapter가 명시적으로 생성한 정제된 ArchivistTurnEntry만 사용한다. 비공개 추론·stdout·stderr·credential·filesystem path를 항목 schema에 넣지 않고 정규화 실패는 code와 사용자용 summary만 남긴다. `LRH-013~014`

Archivist가 시작한 긴 아카이브 실행은 같은 Turn에서 반복 조회하지 않는다. 1단계 완료·전체 완료·새 HOLD 계기에 같은 ArchivistSession으로 SYSTEM 재호출 Turn을 넣고, 최초 Turn과 재호출 Turn을 prompt가 아닌 ArchiveRunWorkContext로 묶는 계약은 `LRH-012`가 소유한다. Archivist Host offline은 Worker 실행을 중단하지 않는다.

## 7. 권한

| 기능 | Archivist | v1 internal-operator |
|---|---:|---:|
| read | 허용 | 허용 |
| process | 정책 허용 시 | 허용 |
| GATED(아카이브 확정 등) | 제안만 | 명시 확인 |
| destructive | 금지 | boot policy+Approval+대상 Confirmation |
| Provider·Worker·Archivist Host·Storage·보안 설정 | 금지 | surface 없음 |

v1에는 별도 admin Principal과 원격 관리 surface가 없다. 운영 명령은 `internal-operator + boot policy + 명시 확인`으로 판정한다. Provider·Worker·Archivist Host·Storage·보안 정책은 아카이브 작업 공간 권한이 아니라 OS ACL로 보호된 로컬 설정 파일과 서비스 재시작으로만 적용한다. Archivist 추가는 사전 등록된 Archivist Host를 고르는 작업 공간 기능이다. `local` 인증의 operator/admin 구분은 후속 버전의 정책 경계일 뿐 v1에 원격 관리 권한을 만들지 않는다.

검증 계약은 `SEC-001~004`, 승인 gate는 `WFL-006`, 포맷 조건은 `DEV-004`다.

Archivist의 채팅 응답은 어떤 표현을 포함해도 Approval이 아니다. 그러나 채팅은 Archivist에게 주는 지시이므로 `ARCHIVIST` 제어 모드에서 허용된 Tool Call로 이어질 수 있다. `ASSISTED` 제안과 승인 필요한 상태 변경은 대상·expected revision·사유·근거를 가진 ActionCard로 기록하고, 사람이 별도 조작으로 승인하거나 Command를 제출해야 한다. `DSH-003`, `COL-002`

## 8. 데이터 반출

외부 Provider 경로를 연결할 때 각 Archivist 요청은 `public`, `internal`, `confidential` 데이터 등급을 가진다. Account의 허용 등급보다 높은 원문은 해당 Provider로 보내지 않는다. 새 Provider로 폴백할 때 전체 대화를 복사하지 않고 다음만 전달한다.

- 민감값이 제거된 구조화 요약
- 필요한 Job/Step 상태
- 완료된 Tool Call ID와 결과 요약
- 사용자가 허용한 전사 구간

## 9. 로그 정제

외부 Provider 원장을 연결할 때는 request ID, 계정 ID, model, input/output/cached token, 가격 기준, 정규화 오류 등 허용 목록의 필드만 원문으로 저장한다. Authorization, Proxy-Authorization, API key, password, cookie, access/refresh token을 비롯한 나머지 필드는 제거한다. 이 경계는 `AGT-008` planned다. 현재 로컬 CLI 원장은 정제된 ArchivistTurnEntry만 저장하는 `LRH-013`을 적용한다.

직접 ChatGPT loop는 Responses 요청마다 `store: false`, `stream: true`를 사용하고 HTTP `previous_response_id`나 Provider conversation을 저장하지 않는다. 필요한 입력, 완료된 model output item과 Tool Call receipt를 현재 process 메모리에서 다음 round로 전달한다. model의 reasoning item과 raw SSE는 Control에 보내지 않고 정제된 도구 진행 요약과 최종 답만 runtime JSONL로 출력한다. model round와 Tool Call 수는 설정 상한을 초과할 수 없다. `AGT-018~020`

직접 local loop는 같은 process 메모리 전달 규칙을 OpenAI 호환 Chat Completions message로 변환하며 `stream: false`, `parallel_tool_calls: false`로 한 번에 한 Archive Tool Call만 조정한다. Authorization header와 raw model payload를 Control에 보내거나 영속하지 않는다. `AGT-019~021`, `LRH-016`

로그 화면은 raw Provider 오류 본문을 기본 노출하지 않고 정규화 코드, 시각, account alias와 추적 ID를 표시한다.
