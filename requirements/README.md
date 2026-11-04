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
| Dispatch와 자원 | `RES-001~008`, `DSP-001~004` | 영속 ready queue, aging·Job 공정성, Worker 자원 판정, 활성 예약을 반영한 큐 admission·lease |
| 실행 경계 | `EXE-001~003` | shell 없는 인수 배열, 임시 출력 검증 뒤 게시, publish 장애 조정 |
| 아카이브 | `ARC-001~013` | 원본 불변, provenance, 결정적 manifest, 보관됨·확정됨 분리, DB 없는 복원, 아카이브 Storage 선택·확정, 날짜·원본 이름 기반 패키지 경로 |
| 내용 검수 | `REV-001` | 대상 리비전의 추가 전용 ContentReview와 최신 결정 투영 |
| 알림 | `NTF-001~002` | 저장 이벤트 replay와 일시 metric 분리, SSE 재연결 |
| Archivist | `AGT-001~015` | cooldown 복귀, overload 연속성·health probe 회복, allowlist 도구, 중복 Tool Call 방지, 허용 목록 로그 |
| 장치와 Worker | `DEV-001~013`, `WRK-001~012`, `CFG-008~014`, `CAN-001~004` | lock, 포맷 전제조건, Worker별 드라이브·Storage, 점진적 폴더 탐색, 네트워크 DEVICE 대기, 안전한 실행 명세·출력 기록, 로컬 media toolchain과 원격 파일 서비스 설정, 단일 Windows runtime root |
| Archive Intake | `ORG-001~014`, `MED-005~006`, `RND-004~006`, `QCT-003`, `AUD-004`, `TRN-005~007`, `PIP-007~009`, `APP-002~007`, `ARC-009~013`, `DSH-006~014`, `DSH-018`, `DSH-022~023` | ArchiveRun, 사람의 원본·아카이브·폴더 선택, Worker 검사, HOLD 분류 뒤 동일 실행 재개, 1단계 보관, 별도 2단계 STT, 전사 품질 확인 필요, 날짜·원본 기반 결과명, 보관됨·확정됨, ArchiveCatalog |
| 작업 공간·직접 실행 | `DEV-006`, `APP-001` | Storage를 논리 Volume 작업 공간으로 해석하고 PhysicalDevice를 I/O 충돌 키로 분리, Adapter 없이 Capability 직접 실행 |
| 아카이브 작업 공간·증적 | `COL-001~003`, `DSH-001~017`, `DSH-019~023`, `OBS-001`, `TST-001` | 사람 직접 실행과 Archivist 도구 호출의 공통 업무 문맥, 구조화된 선택 참조, 예약 차감 Worker 자원 상태, 상태·드라이브·Worker 화면과 문제 해결 위치 이동, 점진적 폴더 탐색과 범위 잠금, 사람이 읽는 아카이브 결과명, 새 문제 알림과 낡은 화면 잠금, 조작 화면과 golden 증적 |
| Archivist Host | `LRH-001~014`, `MCP-001~005`, `CTL-005~006` | Host 인증·Turn lease·재개·Tool Call·ActionCard, 단계 완료 SYSTEM 재호출·업무 문맥, 정제된 진행 항목, 선택 범위의 HOLD 검토·단일 분류 MCP 경계 |

실제 DB transaction, sidecar 복원, 파일 게시 장애 조정과 HTTP 재연결은 각각 `PER-001`, `ARC-007`, `EXE-003`, `NTF-002`의 integration test로 검증한다. Windows junction 경계는 `ARC-008`의 전용 Windows acceptance test로 검증한다. 폐기 가능한 이동식 장치가 필요한 포맷은 `DEV-005`로 계획 상태를 유지한다.

## 5. 현재 기준선

- 전체 요구사항: 247개
- 구현됨: 231개, 주석 테스트 231개와 1:1 연결
- 구현된 integration 테스트: `ARC-006~007`, `ARC-013`, `PER-001`, `NTF-002`, `EXE-003`, `DSH-001~002`, `DSH-004~007`, `DSH-009~014`, `DSH-016~023`, `DEV-006~008`, `DEV-010`, `DEV-013`, `APP-001~004`, `APP-008`, `OBS-001`, `TST-001`, `MED-002`, `MED-004`, `MED-006`, `INT-001`, `INT-003~004`, `RND-002`, `RND-005~006`, `QCT-002`, `CAT-002~003`, `AUD-002`, `AUD-004`, `TRN-002`, `TRN-004`, `TRN-006`, `SUB-002`, `SUM-002~003`, `REV-002`, `CFG-006~010`, `CTL-001~006`, `WRK-005~006`, `WRK-008~012`, `LRH-001`, `LRH-012~013`, `MCP-001`, `MCP-003~005`, `ORG-001`, `ORG-008~009`, `ARC-011`, `COL-001`, `COL-003`, `DSP-003`, `CAN-001~004`
- 구현된 실장비 acceptance 테스트: `ARC-008`, `MED-003`, `RND-003`, `AUD-003`, `LRH-010~011`, `APP-005`, `APP-007`, `CFG-014`
- 계획됨: 16개
- 계획된 실장비 acceptance 테스트: `DEV-005`, `TRN-003`, `WRK-007`, `LRH-009`, `LRH-014`, `DEV-009`, `DEV-012`, `TRN-005`, `CFG-012~013`, `ORG-010`
- 계획된 integration 테스트: `CFG-011`, `APP-006`, `ARC-010`, `ARC-012`, `DSH-015`

정확한 문구와 테스트 이름은 항상 [`catalog.json`](catalog.json)에서 확인한다. 이 문서에는 변경 시 쉽게 낡는 전체 테스트 목록을 복제하지 않는다.

## 6. 실행

```bash
go run ./cmd/reqcheck
go test ./...
```

현재 정상 출력:

```text
traceability ok: 247 requirements (231 implemented, 16 planned), 231 one-to-one tests
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
