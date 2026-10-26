# 요구사항과 실행 가능한 명세

## 1. 단일 기준

[`catalog.json`](catalog.json)이 제품 요구사항의 단일 기준이다. 각 항목은 다음을 가진다.

- 변경하지 않는 요구사항 ID와 소유 설계 영역
- 한 가지 관찰 가능한 업무 결과
- 구체적인 Given / When / Then
- `implemented` 또는 `planned` 상태
- `unit`, `integration`, `acceptance` 테스트 수준
- 구현된 경우 정확한 테스트 파일과 함수 이름

`implemented` 요구사항의 테스트 함수 바로 위에는 `// Requirement: XXX-000` 주석을 둔다. [`cmd/reqcheck`](../cmd/reqcheck/main.go)는 카탈로그와 Go AST를 비교하여 구현 요구사항과 테스트가 정확히 1:1인지 확인한다.

[`semantic-lock.json`](semantic-lock.json)은 ID별 Design/Statement/Given/When/Then/status/level의 SHA-256 지문이다. 테스트 파일 이동은 지문을 바꾸지 않지만 업무 의미 변경은 검사를 실패시킨다. 의미 변경을 검토한 뒤에만 `make requirements-lock`으로 갱신한다.

## 2. 상태와 테스트 수준

| 값 | 뜻 | 카탈로그 규칙 |
|---|---|---|
| `implemented` | 현재 저장소에서 동작을 관찰할 수 있다 | 테스트 파일·함수와 정확히 한 개의 주석 테스트가 필수 |
| `planned` | 계약은 정했지만 필요한 Adapter나 실장비가 아직 없다 | 테스트 참조와 주석 테스트를 두지 않음 |
| `unit` | Core의 규칙을 메모리에서 검증한다 | 외부 서비스 없이 반복 실행 가능 |
| `integration` | filesystem, SQLite, HTTP 같은 Adapter 경계를 검증한다 | 실제 경계 또는 충실한 테스트 인스턴스 사용 |
| `acceptance` | 운영체제·GPU·이동식 장치 등 실환경을 검증한다 | 격리된 전용 장비와 명시적 실행 절차 필요 |

`planned`는 테스트를 생략해도 된다는 뜻이 아니다. Adapter 또는 장비 준비 전의 정직한 상태 표시이며, 구현을 시작할 때 테스트 참조를 추가하고 `implemented`로 전환한다.

## 3. 원자성 규칙

- 한 요구사항은 한 가지 실패 이유 또는 한 가지 업무 결과만 소유한다.
- 같은 결과의 경계값 여러 개는 하나의 table-driven 테스트에서 검증할 수 있다.
- 서로 독립적으로 바뀔 수 있는 결과는 요구사항과 테스트를 나눈다.
- private 구현이 아니라 상태, 오류, 명령 기록, 도메인 이벤트 또는 아티팩트를 관찰한다.
- mock 호출 횟수는 중복 실행 방지처럼 횟수 자체가 업무 결과일 때만 검증한다.
- 이벤트 이름만 확인하지 않는다. 수락 명령은 감사 문맥과 전후 리비전을, 거부 명령은 오류와 이벤트 부재를 함께 검증한다.
- 실제 장비가 필요한 요구사항도 고유 ID와 단일 acceptance 시나리오를 가진다.
- 커버리지 수치를 올리거나 우연한 내부 구현을 고정하기 위한 테스트는 추가하지 않는다. 테스트는 카탈로그의 관찰 가능한 결과만 검증한다.
- 실제 기술 경계의 제한이나 더 나은 설계가 확인되면 Adapter를 억지로 기존 문구에 맞추지 않는다. 근거를 검토한 뒤 소유 설계, 요구사항, semantic lock과 1:1 테스트를 같은 변경에서 함께 수정한다.
- 테스트 검증은 CI 결과만 완료 판정에 사용한다. CI runner는 코드 리비전·환경·테스트 이름·요구사항 ID·수준·결과·시간과 산출물 참조를 기계 판독 가능한 artifact로 누적한다. `TST-001`
- Core unit test에 운영 모니터링 의존성을 주입하지 않는다. runner가 모든 테스트 결과를 기록하고, Application Capability integration test가 공통 Evidence Sink의 운영 증적을 별도로 검증한다. `OBS-001`

## 4. 이벤트 스토밍에서 내려온 계약

[`08-event-storming.md`](../docs/design/08-event-storming.md)의 명령 → 판정 → 이벤트 → 정책 흐름을 다음 테스트 계약으로 내렸다.

| 영역 | 요구사항 | 고정한 핵심 규칙 |
|---|---|---|
| Command와 Event | `CMD-001~010`, `PER-001` | 권한 선판정, 의미 기반 멱등성, 결정적 실패 receipt, Unit of Work, SQLite 원자성 |
| Step 실행 | `WFL-001~013` | 단일 상태 전이표, 최신 Approval 결정, ExecutionLease 배타성, fencing, 게시 후 복구, 실패 분류 |
| 실행 경계 | `EXE-001~003` | shell 없는 인수 배열, 임시 출력 검증 뒤 게시, publish 장애 조정 |
| 아카이브 | `ARC-001~008` | 원본 불변, provenance, 결정적 manifest, 확정 증적, DB 없는 복원, 안전 경로 |
| 내용 검수 | `REV-001` | 대상 리비전의 추가 전용 ContentReview와 최신 결정 투영 |
| 알림 | `NTF-001~002` | 저장 이벤트 replay와 일시 metric 분리, SSE 재연결 |
| Agent | `AGT-001~015` | cooldown 복귀, overload 연속성·health probe 회복, allowlist 도구, 중복 Tool Call 방지, 허용 목록 로그 |
| 장치와 Worker | `DEV-001~005`, `WRK-001~004` | lock, 포맷 전제조건, 실장비 검증, 설정·heartbeat storage 교집합, pinning |
| 작업 공간·직접 실행 | `DEV-006`, `APP-001` | Storage를 PhysicalDevice 작업 공간으로 해석, Adapter 없이 Capability 직접 실행 |
| Dashboard·증적 | `DSH-001`, `OBS-001`, `TST-001` | Agent 업무 현황 투영, 기능 실행 증적, 모든 테스트 실행 증적 묶음 |

실제 DB transaction, sidecar 복원, 파일 게시 장애 조정과 HTTP 재연결은 각각 `PER-001`, `ARC-007`, `EXE-003`, `NTF-002`의 integration test로 검증한다. macOS 개발 환경에서 안전하게 실행할 수 없는 Windows 포맷과 junction은 `DEV-005`, `ARC-008`로 계획 상태를 유지한다.

## 5. 현재 기준선

- 전체 요구사항: 129개
- 구현됨: 123개, 주석 테스트 123개와 1:1 연결
- 구현된 integration 테스트: `ARC-006`, `PER-001`, `ARC-007`, `EXE-003`, `NTF-002`, `TST-001`, `DEV-006`, `APP-001`, `OBS-001`, `DSH-001`, `MED-002`, `INT-001`, `INT-003~004`, `MED-004`, `RND-002`, `QCT-002`, `CAT-002~003`, `AUD-002`, `TRN-002`, `TRN-004`, `SUB-002`, `SUM-002~003`, `REV-002`, `CFG-006`, `CTL-001~002`
- 계획됨: 6개
- 계획된 실장비 acceptance 테스트: `DEV-005`, `ARC-008`, `MED-003`, `RND-003`, `AUD-003`, `TRN-003`
- 계획된 integration 테스트: 없음

정확한 문구와 테스트 이름은 항상 [`catalog.json`](catalog.json)에서 확인한다. 이 문서에는 변경 시 쉽게 낡는 전체 테스트 목록을 복제하지 않는다.

## 6. 실행

```bash
go run ./cmd/reqcheck
go test ./...
```

현재 정상 출력:

```text
traceability ok: 129 requirements (123 implemented, 6 planned), 123 one-to-one tests
```

## 7. 새 요구사항 추가 절차

1. 용어집과 이벤트 스토밍에서 명령, 이벤트, 실행자, 대상, 일관성 경계를 확인한다.
2. 소유 설계 문서와 Prefix를 선택한다.
3. 카탈로그에 원자적인 Given / When / Then, 상태와 테스트 수준을 기록한다.
4. Core로 검증할 수 있으면 예정 테스트 파일·함수를 기록하고 같은 ID의 실패 테스트를 작성한다.
5. Adapter를 구현할 때는 실제 filesystem, SQLite, HTTP 같은 경계를 사용하는 실패 테스트를 먼저 작성하고 요구사항에 필요한 최소 동작만 구현한다.
6. Adapter나 실장비가 아직 없으면 테스트를 허위 연결하지 않고 `planned`로 둔다.
7. 테스트 과정에서 기술 제한이나 더 나은 구조가 발견되면 소유 설계와 요구사항을 검토해 고친 뒤 같은 변경에서 테스트도 고친다.
8. 최소 구현 후 `reqcheck`와 전체 테스트를 통과시키고 `implemented`로 전환한다.
